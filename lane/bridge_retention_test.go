package lane_test

import (
	"testing"
	"time"

	kit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/bus/sdk/go/protocol"
)

// The SDK owns terminal records independently of the ACP child's lifetime.
// In particular, losing the bridge must not replace the caller's retirement
// policy with a private worker deadline, nor let a later refused Run erase a result.
func TestBridgeLossRetainsResultUntilCallerClose(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "after terminal"
		if busy {
			name = "during run"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			h := e.start()
			h.mustOpen(kit.OpenOptions{})
			text := "retained result"
			if busy {
				text = "SLOW retained result"
			}
			run := h.Start(text)
			if busy {
				eventually(t, "active native prompt", func() bool { return len(e.events("prompt")) == 1 })
				e.bridge.Drop()
				if err := h.Interrupt(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case ready := <-h.ready:
				if ready.RunID != run || ready.State != "done" {
					t.Fatalf("terminal = %+v", ready)
				}
			case <-time.After(waitFor):
				t.Fatal("missing terminal")
			}
			if !busy {
				e.bridge.Drop()
			}
			eventually(t, "native session closed", func() bool { return len(e.events("session_close")) == 1 })
			// This crosses the old one-second test grace without waiting for a
			// production thirty-second timeout. Neither read nor ACK retires the worker.
			select {
			case <-h.gone:
				t.Fatal("bridge loss retired worker before caller close")
			case <-time.After(1200 * time.Millisecond):
			}
			first, ok := h.request("turn.status", protocol.ReadRequest{SessionID: h.laneID, RunID: run})
			if !ok || first.Error != nil {
				t.Fatalf("retained result: %+v", first)
			}
			refusedRun := h.Start("must not reach native")
			f, ok := h.request("turn.wait", protocol.WaitRequest{SessionID: h.laneID, RunID: refusedRun})
			var refused protocol.RunStatus
			if !ok || f.Error != nil {
				t.Fatalf("refused Run status: %+v", f)
			}
			if err := protocol.UnmarshalResult("turn.wait", f.Result, &refused); err != nil {
				t.Fatal(err)
			}
			if refused.State != "unavailable" || refused.Reason != "lane bridge lost" {
				t.Fatalf("new Run = %+v", refused)
			}
			later, ok := h.request("turn.wait", protocol.WaitRequest{SessionID: h.laneID, RunID: run})
			if !ok || later.Error != nil || string(first.Result) != string(later.Result) {
				t.Fatalf("terminal changed: status=%s wait=%+v", first.Result, later)
			}
			if f, ok := h.request("turn.ack", protocol.RunRef{SessionID: h.laneID, RunID: run}); !ok || f.Error != nil {
				t.Fatalf("ack: %+v", f)
			}
			if f, ok := h.request("turn.ack", protocol.RunRef{SessionID: h.laneID, RunID: refusedRun}); !ok || f.Error != nil {
				t.Fatalf("refused Run ack: %+v", f)
			}
			if got := len(e.events("prompt")); got != 1 {
				t.Fatalf("native prompts = %d, want only original", got)
			}
			h.Close()
		})
	}
}

func TestBridgeLossWorkerStillExitsOnDaemonEOF(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	e.bridge.Drop()
	eventually(t, "native session closed", func() bool { return len(e.events("session_close")) == 1 })
	if err := h.conn.Close(); err != nil {
		t.Fatal(err)
	}
	h.waitGone()
}
