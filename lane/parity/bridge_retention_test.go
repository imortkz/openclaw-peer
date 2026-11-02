package parity

import (
	"testing"
	"time"
)

// The real daemon owns policy timers; the worker must preserve them after the
// bridge has gone and native resources have been closed.
func TestParityBridgeLossPreservesRetirementPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		delay      int
		refusedRun bool
	}{
		{"disabled", 0, true},
		{"timed", 1200, false},
		{"unavailable does not rearm", 1200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBus(t)
			var lane spawnResult
			b.mustCall("lane.spawn", map[string]any{
				"product": "openclaw-peer", "name": "bridge-retention", "persistent": true,
				"auto_close_ms": tc.delay, "notify": false, "extra_groups": []string{"lanetest"},
				"open": map[string]any{"arguments": []string{"--agent", "a"}},
			}, &lane)
			original := b.runText(lane.SessionID, "result survives bridge loss")
			completed(t, original)
			b.bridge.Drop()
			eventually(t, "native cleanup", func() bool { return len(b.events("session_close")) == 1 })
			refs := []string{original.RunID}
			if tc.refusedRun {
				refused := b.runText(lane.SessionID, "must not reach native")
				if refused.State != "unavailable" || refused.Reason != "lane bridge lost" {
					t.Fatalf("post-loss Run: %+v", refused)
				}
				refs = append(refs, refused.RunID)
			}
			var retained runStatus
			b.mustCall("turn.status", map[string]any{"session_id": lane.SessionID, "run_id": original.RunID}, &retained)
			if completed(t, retained) != original.Result.Result {
				t.Fatal("original result changed")
			}
			if tc.delay == 0 || tc.refusedRun {
				// An admitted Run clears the previous deadline. An unavailable
				// terminal does not arm another one, even for a timed lane.
				time.Sleep(1500 * time.Millisecond)
				b.mustCall("turn.wait", map[string]any{"session_id": lane.SessionID, "run_id": original.RunID}, &retained)
				if completed(t, retained) != original.Result.Result {
					t.Fatal("terminal was not retained")
				}
				for _, run := range refs {
					b.mustCall("turn.ack", map[string]any{"session_id": lane.SessionID, "run_id": run}, nil)
				}
				b.mustCall("session.close", map[string]any{"session_id": lane.SessionID}, nil)
			} else {
				// Keep the original record unacknowledged: unknown_session then
				// demonstrates retirement, not merely deletion of an ACKed record.
				eventually(t, "daemon automatic close", func() bool {
					_, err := b.call("turn.status", map[string]any{"session_id": lane.SessionID, "run_id": original.RunID})
					return err != nil && err.Code == -32001
				})
			}
			if got := len(b.events("prompt")); got != 1 {
				t.Fatalf("native prompts = %d, want only original", got)
			}
		})
	}
}

// A real ACP crash must preserve SDK records just like loss of the bridge.
func TestParityChildCrashRetainsResults(t *testing.T) {
	b := newBus(t)
	var lane spawnResult
	b.mustCall("lane.spawn", map[string]any{
		"product": "openclaw-peer", "name": "child-retention", "persistent": true,
		"auto_close_ms": 0, "notify": false, "extra_groups": []string{"lanetest"},
		"open": map[string]any{"arguments": []string{"--agent", "a"}},
	}, &lane)
	older := b.runText(lane.SessionID, "older unacknowledged result")
	completed(t, older)
	crashed := b.runText(lane.SessionID, "please CRASH")
	if crashed.State != "unavailable" || crashed.Reason == "" {
		t.Fatalf("crashed run: %+v", crashed)
	}
	time.Sleep(1500 * time.Millisecond) // past the old test retention window
	for _, method := range []string{"turn.status", "turn.wait"} {
		var retained runStatus
		b.mustCall(method, map[string]any{"session_id": lane.SessionID, "run_id": crashed.RunID}, &retained)
		if retained.State != crashed.State || retained.Reason != crashed.Reason || retained.RunID != crashed.RunID {
			t.Fatalf("%s changed crashed record: %+v", method, retained)
		}
	}
	refused := b.runText(lane.SessionID, "must not reach child")
	if refused.State != "unavailable" || refused.Reason != "lane native session exited" {
		t.Fatalf("post-exit Run: %+v", refused)
	}
	var retained runStatus
	b.mustCall("turn.status", map[string]any{"session_id": lane.SessionID, "run_id": older.RunID}, &retained)
	if completed(t, retained) != older.Result.Result {
		t.Fatal("older record changed after child crash")
	}
	if got := len(b.events("prompt")); got != 2 {
		t.Fatalf("native prompts = %d, want original and crash only", got)
	}
	for _, ref := range []string{older.RunID, crashed.RunID, refused.RunID} {
		b.mustCall("turn.ack", map[string]any{"session_id": lane.SessionID, "run_id": ref}, nil)
	}
	b.mustCall("session.close", map[string]any{"session_id": lane.SessionID}, nil)
	if _, err := b.call("turn.status", map[string]any{"session_id": lane.SessionID, "run_id": crashed.RunID}); err == nil || err.Code != -32001 {
		t.Fatalf("status after close: %v", err)
	}
}
