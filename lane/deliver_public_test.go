package lane_test

// TW-L3 — public-surface tests for a busy lane's Deliver → lane.steer path
// (PLAN bus-0512, Test-writer stream). Spec: PLAN `## Interface` «Lane receipts» (D2/D13),
// D5 (bridge loss fails closed), D21 (fallback retries, "not BUSY-MID"), D18-adjacent
// directive neutralization. Everything is driven through the public worker surface
// (message.deliver over the daemon socket) and observed in the developer-owned doubles
// (lane/internal/fakebridge log, the fakeacp event log, the worker's stderr) — no wrapper
// internals are touched. Shared harness: harness_test.go / wrapper_test.go (package lane_test).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/bus/sdk/go/protocol"

	"github.com/imortkz/openclaw-peer/lane/internal/fakebridge"
)

// mustStartBusy opens the lane, starts a SLOW turn (the fake child emits one chunk and
// waits for cancel) and returns once the prompt reached the child: the lane is now busy.
func mustStartBusy(t *testing.T, e *testEnv, h *harness) string {
	t.Helper()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt to reach the child", func() bool { return len(e.callerPrompts()) == 1 })
	return run
}

// endSlowTurn interrupts the SLOW turn and collects its terminal.
func endSlowTurn(t *testing.T, h *harness, run string) {
	t.Helper()
	if rerr := h.Interrupt(); rerr != nil {
		t.Fatalf("interrupt: %v", rerr)
	}
	h.Wait(run)
}

// TestDeliverSteersActiveTurn: a busy lane's delivery goes through the bridge as one
// lane.steer of the rendered text, answers `written`, and never becomes a second ACP prompt.
func TestDeliverSteersActiveTurn(t *testing.T) {
	e := newEnv(t) // fakebridge steer default: eligible
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })

	if r := deliveryReceipt(t, h.Deliver("steer me mid-run")); r.Disposition != "written" {
		t.Fatalf("receipt %+v, want written (D2: written only after an eligible steer)", r)
	}

	events := steerEvents(e)
	if len(events) != 1 {
		t.Fatalf("lane.steer events %d, want exactly 1: %+v", len(events), events)
	}
	if events[0].Params["sessionKey"] != key {
		t.Fatalf("steer sessionKey %v, want the lane key %q", events[0].Params["sessionKey"], key)
	}
	if !strings.Contains(str(events[0].Params["text"]), "steer me mid-run") {
		t.Fatalf("steer text %q does not carry the delivered body", str(events[0].Params["text"]))
	}
	if n := len(e.callerPrompts()); n != 1 {
		t.Fatalf("the steered delivery reached the agent as a second prompt (%d prompts)", n)
	}
	endSlowTurn(t, h, run)
}

// TestSteerTextNeutralized: a delivered `/model x` command is neutralized with U+200B
// before the slash on its way into the running turn — never an un-prefixed directive.
func TestSteerTextNeutralized(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })

	deliveryReceipt(t, h.Deliver("/model x then continue"))

	events := steerEvents(e)
	if len(events) != 1 {
		t.Fatalf("lane.steer events %d, want 1: %+v", len(events), events)
	}
	text := str(events[0].Params["text"])
	if !strings.Contains(text, "steer") && !strings.Contains(text, "model") {
		t.Fatalf("steer text %q does not look like the rendered delivery", text)
	}
	// Every /model occurrence must be neutralized: a U+200B directly before the slash.
	if got, want := strings.Count(text, "\u200b/model"), strings.Count(text, "/model"); got == 0 || got != want {
		t.Fatalf("steer text %q: %d/%d /model occurrences neutralized with U+200B", text, got, want)
	}
	endSlowTurn(t, h, run)
}

// TestDeliverSteerFallback: the D13/D21 table for a busy lane whose steer is not accepted.
// The receipt answers fast (< 1 s); the fallback line names itself "not BUSY-MID".
func TestDeliverSteerFallback(t *testing.T) {
	t.Run("not eligible after retries returns not_running fast with the fallback line", func(t *testing.T) {
		e := newEnv(t)
		e.steerSeq = []string{fakebridge.False}
		h := e.startBinary()
		run := mustStartBusy(t, e, h)

		start := time.Now()
		f := h.Deliver("while busy")
		if d := time.Since(start); d >= time.Second {
			t.Fatalf("Deliver held %s, want < 1 s (D21: ≤3 retries × 100 ms)", d)
		}
		if f.Error == nil || f.Error.Code != protocol.NotRunning {
			t.Fatalf("answer %s err %s, want not_running", f.Result, rpcText(f.Error))
		}
		// 1 initial steer + 3 retries, then the give-up line on stderr.
		if n := len(steerEvents(e)); n != 4 {
			t.Fatalf("lane.steer events %d, want 4 (initial + 3 retries)", n)
		}
		eventually(t, "the fallback line on stderr", func() bool {
			return strings.Contains(h.workerStderr(), "steer fallback (not BUSY-MID): reason=not_eligible")
		})
		if n := len(e.callerPrompts()); n != 1 {
			t.Fatalf("the delivery reached the agent as a prompt (%d prompts)", n)
		}
		endSlowTurn(t, h, run)
	})

	t.Run("eligible on a retry is written", func(t *testing.T) {
		e := newEnv(t)
		e.steerSeq = []string{fakebridge.False, fakebridge.Eligible}
		h := e.startBinary()
		run := mustStartBusy(t, e, h)

		if r := deliveryReceipt(t, h.Deliver("second try")); r.Disposition != "written" {
			t.Fatalf("receipt %+v, want written once a retry is eligible", r)
		}
		if n := len(steerEvents(e)); n != 2 {
			t.Fatalf("lane.steer events %d, want 2 (refused, then eligible)", n)
		}
		if strings.Contains(h.workerStderr(), "steer fallback") {
			t.Fatalf("unexpected fallback line; stderr %q", h.workerStderr())
		}
		endSlowTurn(t, h, run)
	})

	t.Run("no reply is uncertain native admission", func(t *testing.T) {
		e := newEnv(t)
		e.steer = fakebridge.NoReply
		h := e.startBinary()
		run := mustStartBusy(t, e, h)

		f := h.Deliver("into the void")
		if f.Error == nil || f.Error.Code != protocol.Internal {
			t.Fatalf("answer %s err %s, want -32603", f.Result, rpcText(f.Error))
		}
		if !strings.Contains(string(f.Error.Data), "uncertain_native_admission") {
			t.Fatalf("error data %s, want uncertain_native_admission", f.Error.Data)
		}
		if n := len(steerEvents(e)); n != 1 {
			t.Fatalf("lane.steer events %d, want 1 (the frame was written)", n)
		}
		if strings.Contains(h.workerStderr(), "steer fallback") {
			t.Fatalf("an uncertain admission must not run the fallback; stderr %q", h.workerStderr())
		}
		// The worker keeps its lane: the run can still be interrupted and collected.
		endSlowTurn(t, h, run)
	})

	// The PLAN table's `close before write → not_running` row is not reachable through the
	// public worker surface: fakebridge's Close mode drops the bridge connection immediately
	// after lane.register, i.e. while the lane is still idle, so D5 fails the worker closed
	// before any Deliver can be answered (the ErrNotWritten→NotRunning mapping stays covered
	// at the lane.Bridge client level by TW-L1's TestBridgeSteerOutcomes). Finding TW-L3-1.

	t.Run("bridge lost mid-run rejects as not submitted", func(t *testing.T) {
		e := newEnv(t)
		h := e.start()
		run := mustStartBusy(t, e, h)

		e.bridge.Drop()
		eventually(t, "the loss to be seen", func() bool {
			f := h.Deliver("late")
			if f.Error != nil {
				return false
			}
			var r kit.DeliveryReceipt
			if err := protocol.UnmarshalResult("message.deliver", f.Result, &r); err != nil {
				return false
			}
			return r.Disposition == "rejected" && r.Reason == "not_submitted"
		})
		endSlowTurn(t, h, run)
	})
}

// TestBridgeLossFailsClosed: losing the bridge closes native resources, after the
// terminal for a live run. The result-serving worker retires on ordinary close.
func TestBridgeLossFailsClosed(t *testing.T) {
	t.Run("idle native session closes immediately", func(t *testing.T) {
		e := newEnv(t)
		h := e.start()
		h.mustOpen(kit.OpenOptions{})
		prompts := len(e.callerPrompts())

		e.bridge.Drop()
		eventually(t, "native closure", func() bool { return len(e.events("session_close")) == 1 })
		h.Close()
		if n := len(e.callerPrompts()); n != prompts {
			t.Fatalf("prompts grew %d -> %d after the bridge loss", prompts, n)
		}
	})

	t.Run("live native session closes after its terminal", func(t *testing.T) {
		e := newEnv(t)
		h := e.start()
		run := mustStartBusy(t, e, h)

		e.bridge.Drop()
		select {
		case <-h.gone:
			t.Fatal("worker exited while its run was still live")
		case <-time.After(500 * time.Millisecond):
		}
		if n := len(e.callerPrompts()); n != 1 {
			t.Fatalf("prompts after the bridge loss: %d, want 1", n)
		}
		endSlowTurn(t, h, run)
		eventually(t, "native closure", func() bool { return len(e.events("session_close")) == 1 })
		h.Close()
		if n := len(e.callerPrompts()); n != 1 {
			t.Fatalf("prompts after shutdown: %d, want 1 (no farewell prompt)", n)
		}
	})
}

// flake-steer-tempdir (red): the subtest `bridge lost mid-run rejects as not submitted` failed
// in CI on the TempDir cleanup (`unlinkat …/001: directory not empty`). After a bridge loss,
// endRun's goroutine closes the lane once the run is done: Close takes w.process and then waits
// for the ACP child to exit. When the host's connection closes meanwhile (the harness cleanup),
// Serve's own closeProduct → Close finds no process, returns at once, and the worker reports
// gone while the child is still alive and writing into the test directory. The test widens the
// child's exit with an `openclaw` shim that lingers 500 ms after the real fake exits and then
// writes a marker into the test directory; it requires the child to be gone when the worker is.
func TestBridgeLostMidRunWorkerOutlivesChild(t *testing.T) {
	e := newEnv(t)
	marker := filepath.Join(e.dir, "child-exited")
	shimDir := e.mkdir(e.dir, "shim")
	shim := "#!/bin/sh\n" +
		"if [ \"$1\" != acp ]; then exec '" + fakeBin + "' \"$@\"; fi\n" +
		"'" + fakeBin + "' \"$@\"\n" +
		"sleep 0.5\n" +
		"echo exited > '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(shimDir, "openclaw"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := e.start()
	run := mustStartBusy(t, e, h)

	e.bridge.Drop()
	eventually(t, "the loss to be seen", func() bool {
		f := h.Deliver("late")
		if f.Error != nil {
			return false
		}
		var r kit.DeliveryReceipt
		_ = protocol.UnmarshalResult("message.deliver", f.Result, &r)
		return r.Disposition == "rejected" && r.Reason == "not_submitted"
	})
	endSlowTurn(t, h, run)
	// The post-loss close is under way (session/close reached the child) before the host goes.
	eventually(t, "the post-loss session/close", func() bool { return len(e.events("session_close")) == 1 })

	_ = h.conn.Close() // what the harness cleanup does first
	h.waitGone()
	_, err := os.Stat(marker)
	childGone := err == nil
	// Let the child finish before t.TempDir is removed, so this test itself does not flake.
	eventually(t, "the ACP child to exit", func() bool { _, err := os.Stat(marker); return err == nil })
	if !childGone {
		t.Fatal("worker gone while its ACP child was still running (post-loss Close in flight)")
	}
}
