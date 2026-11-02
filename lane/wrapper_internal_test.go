package lane

// cappedResult is unexported, so its edge cases are covered here in package lane rather than in
// wrapper_test.go (test-writer's file, external package lane_test).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	kit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/bus/sdk/go/protocol"
	"github.com/imortkz/openclaw-peer/lane/internal/acp"
	"github.com/imortkz/openclaw-peer/lane/internal/fakebridge"
	"github.com/sessionbus/peer-common/host"
)

func TestCloseReportsACPCleanupFailureWithoutSocket(t *testing.T) {
	process, err := acp.Start(context.Background(), []string{"/bin/sh", "-c", "read line; printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":1,\"message\":\"close exploded\\ntoken=SECRETVALUE1234567890\"}}'; cat >/dev/null"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	w := &Wrapper{process: process, acpSessionID: "session"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = w.Close(ctx, kit.SessionCloseRequest{})
	if err == nil || !strings.Contains(err.Error(), "close exploded") {
		t.Fatalf("Close error = %v", err)
	}
	if strings.ContainsAny(err.Error(), "\r\n") || strings.Contains(err.Error(), "SECRETVALUE1234567890") {
		t.Fatalf("Close error not single-line and sanitized: %q", err)
	}
	if w.process != nil {
		t.Fatal("process retained after Close")
	}
	if err := process.Close(ctx); err != nil {
		t.Fatalf("process still running: %v", err)
	}
}

// cappedResult must reach protocol.MaxTextRunes directly (byte cap first, then a
// character-budget cut), not by halving from the 900 KiB byte cap — halving an ASCII-heavy
// answer past the character limit throws away far more than necessary.
func TestCappedResultReachesCharacterLimit(t *testing.T) {
	original := strings.Repeat("a", 300000)
	result := cappedResult(kit.TurnResult{Outcome: "completed", Result: original})
	if !result.Truncated {
		t.Fatal("a 300000-character ASCII result must be truncated")
	}
	if err := protocol.ValidateTurnResult(result); err != nil {
		t.Fatalf("capped result still fails schema validation: %v", err)
	}
	marker := "\n[truncated by openclaw-peer: "
	idx := strings.Index(result.Result, marker)
	if idx < 0 {
		t.Fatalf("no truncation note found in %q", result.Result[max(0, len(result.Result)-80):])
	}
	payload, note := result.Result[:idx], result.Result[idx:]
	kept := utf8.RuneCountInString(payload)
	// The exact budget cappedResult can compute in one pass uses len(original) as the note's
	// omitted-count digits (an upper bound, since the true omitted count is <= len(original));
	// that is the bound worth asserting, not a perfect fixed-point optimum a single pass can't
	// reach when the true omitted count happens to need fewer digits.
	worstCaseNote := fmt.Sprintf("\n[truncated by openclaw-peer: %d bytes omitted]", len(original))
	if want := protocol.MaxTextRunes - utf8.RuneCountInString(worstCaseNote); kept < want {
		t.Fatalf("kept %d characters, want at least %d: halving lost more than necessary", kept, want)
	}
	if total := utf8.RuneCountInString(result.Result); total > protocol.MaxTextRunes {
		t.Fatalf("total result is %d characters, want <= %d", total, protocol.MaxTextRunes)
	}
	if omitted, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(note, marker), " bytes omitted]")); err != nil || omitted != len(original)-len(payload) {
		t.Fatalf("truncation note %q does not match kept %d of %d bytes", note, len(payload), len(original))
	}
}

// The character budget must only apply when a cut is actually needed. Applying
// it unconditionally clips an answer that already fits (262094-262144 characters) even though it
// passes ValidateTurnResult and the frame cap whole.
func TestCappedResultDoesNotTruncateWhatFits(t *testing.T) {
	for _, n := range []int{protocol.MaxTextRunes, protocol.MaxTextRunes - 10} {
		original := strings.Repeat("a", n)
		result := cappedResult(kit.TurnResult{Outcome: "completed", Result: original})
		if result.Truncated || result.Result != original {
			t.Fatalf("%d characters: Truncated=%v len(Result)=%d, want the answer returned whole", n, result.Truncated, len(result.Result))
		}
	}
}

// A model's ordinary assistant-text refusal must obey the same result schema cap as a
// successful answer. A long reply can start with the refusal prefix even when model-generated.
func TestThinkingRefusalResultIsCapped(t *testing.T) {
	turn := &acpTurn{w: &Wrapper{}, reasoningEffort: "xhigh", done: make(chan turnReply, 1)}
	turn.text.WriteString(thinkingUnsupportedPrefix("xhigh") + "mock/mock. Use one of: low. " + strings.Repeat("x", 300000))
	turn.done <- turnReply{stop: "end_turn"}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "failed" || !strings.HasPrefix(result.Result, "unsupported value reasoning_effort=xhigh (") || !result.Truncated {
		t.Fatalf("reasoning refusal result: outcome=%q truncated=%v prefix=%q", result.Outcome, result.Truncated, result.Result[:min(len(result.Result), 80)])
	}
	if err := protocol.ValidateTurnResult(result); err != nil {
		t.Fatalf("reasoning refusal result rejected by the SDK: %v", err)
	}
}

// The runtime admin guard must refuse Open when its harmless read-only
// probe (exec.approvals.get) succeeds — that can only happen if the lane's own Gateway credential
// carries operator.admin, since 8.2 classifies that method as needing exactly that scope.
func TestAssertNonAdminCredentialRefusesWhenProbeSucceeds(t *testing.T) {
	w := &Wrapper{cfg: gatewayConfig(t, `printf '{"ok":true,"key":"probe"}'`)}
	err := w.assertNonAdminCredential(context.Background())
	if err == nil || !strings.Contains(err.Error(), "operator.admin") {
		t.Fatalf("assertNonAdminCredential = %v, want an operator.admin refusal", err)
	}
}

func TestResumeChecksCredentialBeforeGatewaySessionRead(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "calls")
	script := fmt.Sprintf(`echo "$1" >> %q; printf '{"ok":true,"result":{}}'`, logPath)
	gatewayConfig(t, script)
	w := &Wrapper{cfg: Config{
		Agents:        []Agent{{ID: "lane", Workspace: root}},
		CredentialDir: filepath.Join(root, "cli"),
	}}
	key := "agent:lane:sb-12345678-1234-4123-8123-123456789abc"
	_, err := w.openResume(context.Background(), kit.OpenRequest{ResumeSessionID: key, Open: kit.OpenOptions{Cwd: root}})
	if err == nil || !strings.Contains(err.Error(), "lane credential has operator.admin") {
		t.Fatalf("openResume error = %v, want admin guard refusal", err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(calls)); got != execApprovalsGetProbeMethod {
		t.Fatalf("Gateway calls = %q, want only credential probe", got)
	}
}

// The platform's own clean missing-scope refusal (the expected shape for a correctly-provisioned
// read/write/approvals device) must pass the guard — this is the case that must never regress into
// a false positive that blocks every ordinary lane.
func TestAssertNonAdminCredentialPassesOnACleanRefusal(t *testing.T) {
	script := `printf '{"ok":false,"error":{"type":"gateway_request_error","code":"FORBIDDEN","message":"missing scope: operator.admin","details":{"missingScope":"operator.admin"}}}'; exit 1`
	w := &Wrapper{cfg: gatewayConfig(t, script)}
	if err := w.assertNonAdminCredential(context.Background()); err != nil {
		t.Fatalf("assertNonAdminCredential = %v, want nil for a correctly-refused non-admin credential", err)
	}
}

// The device-token scope-mismatch text (including the sessions.create retry case) is also accepted as a
// confirmed non-admin signal, as an explicit second shape — defensively, in case some
// other admin-gated method call also ever produces it.
func TestAssertNonAdminCredentialPassesOnDeviceTokenScopeMismatchText(t *testing.T) {
	script := `printf '{"ok":false,"error":{"type":"gateway_request_error","code":"FORBIDDEN","message":"unauthorized: device token scope mismatch (re-pair or approve scope upgrade)"}}'; exit 1`
	w := &Wrapper{cfg: gatewayConfig(t, script)}
	if err := w.assertNonAdminCredential(context.Background()); err != nil {
		t.Fatalf("assertNonAdminCredential = %v, want nil for a confirmed device-token scope mismatch", err)
	}
}

// A transport-level failure (the probe command itself is unreachable, or
// its output doesn't parse) is not a confirmed answer either way, so it must fail CLOSED — refuse
// this Open. The earlier fail-open behavior was unsafe. The failure must not be
// cached either: a transient hiccup must not permanently poison the worker.
func TestAssertNonAdminCredentialFailsClosedOnTransportFailure(t *testing.T) {
	w := &Wrapper{cfg: gatewayConfig(t, "exit 1")}
	err := w.assertNonAdminCredential(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lane credential check failed") {
		t.Fatalf("assertNonAdminCredential = %v, want a \"lane credential check failed\" refusal", err)
	}
	if w.adminGuardDone {
		t.Fatal("an inconclusive probe must not be cached (adminGuardDone set)")
	}
}

// An unrecognized Gateway error (a confirmed response, but not the specific missing-scope/
// scope-mismatch shape) is equally inconclusive and must fail closed the same way.
func TestAssertNonAdminCredentialFailsClosedOnUnrecognizedGatewayError(t *testing.T) {
	script := `printf '{"ok":false,"error":{"type":"gateway_request_error","code":"INVALID_REQUEST","message":"unknown method: exec.approvals.get"}}'; exit 1`
	w := &Wrapper{cfg: gatewayConfig(t, script)}
	err := w.assertNonAdminCredential(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lane credential check failed") || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("assertNonAdminCredential = %v, want a \"lane credential check failed\" refusal naming the reason", err)
	}
}

// "Run once per worker process (cached), not per turn" applies only to a
// *definitive* outcome — the credential does not change between turns, so a second call after a
// confirmed non-admin refusal must not repeat the Gateway round trip.
func TestAssertNonAdminCredentialCachesConfirmedOutcome(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := fmt.Sprintf(`n=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > %q; printf '{"ok":false,"error":{"type":"gateway_request_error","code":"FORBIDDEN","message":"missing scope: operator.admin","details":{"missingScope":"operator.admin"}}}'; exit 1`, counter, counter)
	w := &Wrapper{cfg: gatewayConfig(t, script)}
	if err := w.assertNonAdminCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.assertNonAdminCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Fatalf("probe ran %s times across two assertNonAdminCredential calls after a confirmed outcome, want exactly 1", got)
	}
}

// The inverse of the cache test above: an inconclusive transport failure
// outcome must NOT be cached, so a second call gets a fresh probe and can recover once the
// Gateway is reachable again.
func TestAssertNonAdminCredentialRetriesAfterInconclusiveOutcome(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	// First invocation fails transport-side (no valid JSON, exit 1); second succeeds with a clean
	// non-admin refusal. Both share one counter file so the test can tell them apart.
	script := fmt.Sprintf(`
n=$(cat %q 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > %q
if [ "$n" -eq 1 ]; then
  exit 1
fi
printf '{"ok":false,"error":{"type":"gateway_request_error","code":"FORBIDDEN","message":"missing scope: operator.admin","details":{"missingScope":"operator.admin"}}}'
exit 1
`, counter, counter)
	w := &Wrapper{cfg: gatewayConfig(t, script)}
	first := w.assertNonAdminCredential(context.Background())
	if first == nil || !strings.Contains(first.Error(), "lane credential check failed") {
		t.Fatalf("first call = %v, want an inconclusive-outcome refusal", first)
	}
	second := w.assertNonAdminCredential(context.Background())
	if second != nil {
		t.Fatalf("second call = %v, want nil — the retry after an uncached inconclusive outcome should reach the clean refusal", second)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "2" {
		t.Fatalf("probe ran %s times, want exactly 2 (the inconclusive first call must not be cached)", got)
	}
}

func gatewayConfig(t *testing.T, script string) Config {
	t.Helper()
	bin := t.TempDir()
	shim := "#!/bin/sh\nshift 2\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "openclaw"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return Config{CredentialDir: filepath.Join(bin, "cli")}
}

func TestChildEnvOverridesDaemonCredentialInProbe(t *testing.T) {
	log := filepath.Join(t.TempDir(), "env")
	script := fmt.Sprintf(`printf '%%s\n%%s\n' "$OPENCLAW_STATE_DIR" "$OPENCLAW_CONFIG_PATH" > %q; printf '{"ok":false,"error":{"type":"gateway_request_error","code":"FORBIDDEN","message":"missing scope: operator.admin","details":{"missingScope":"operator.admin"}}}'; exit 1`, log)
	cfg := gatewayConfig(t, script)
	t.Setenv("OPENCLAW_STATE_DIR", "/admin")
	t.Setenv("OPENCLAW_CONFIG_PATH", "/admin/openclaw.json")
	w := &Wrapper{cfg: cfg}
	if err := w.assertNonAdminCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), cfg.CredentialDir+"\n"+filepath.Join(cfg.CredentialDir, "openclaw.json")+"\n"; got != want {
		t.Fatalf("child credential env %q, want %q", got, want)
	}
}

func TestOpenModelRefusalWithoutSocket(t *testing.T) {
	script := `printf '{"ok":false,"error":{"type":"gateway_request_error","code":"INVALID_REQUEST","message":"model not allowed"}}'; exit 1`
	w := &Wrapper{cfg: gatewayConfig(t, script)}
	entries := make([]string, 23)
	for i := range entries {
		entries[i] = fmt.Sprintf("mock/model-%02d", i)
	}
	for _, tc := range []struct {
		name   string
		models *AgentModels
		suffix string
	}{
		{"full", &AgentModels{Allow: entries, Default: "mock/a"}, "; allowed for a: " + strings.Join(entries, ", ") + "; default: mock/a"},
		{"any", &AgentModels{Allow: []string{}}, "; allowed for a: any"},
		{"missing allow", &AgentModels{Default: "mock/a"}, ""},
		{"legacy", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := w.gatewayPatchModel(context.Background(), "agent:a:key", "bad/model", Agent{ID: "a", Models: tc.models})
			want := "unsupported value model=bad/model (refused by OpenClaw: model not allowed" + tc.suffix + ")"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func feedChunk(w *Wrapper, text string) {
	raw, _ := json.Marshal(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}})
	w.onUpdate("", raw)
}

// The ACP text is bounded as it arrives: the buffer never passes outputCap, the cut is on a rune
// boundary, the terminal is still delivered, and the note counts every omitted byte.
func TestOutputBoundedAtReceive(t *testing.T) {
	w := &Wrapper{}
	turn := &acpTurn{w: w, done: make(chan turnReply, 1), admitted: make(chan struct{}), notSent: make(chan struct{})}
	w.active = turn
	const chunk = 64 << 10
	total := 0
	for i := 0; i < 32; i++ { // 2 MiB; "é" is 2 bytes, 899.5*1024 lands mid-rune for odd offsets
		text := strings.Repeat("é", chunk/2)
		if i == 5 {
			text = "x" + text // shifts the rune grid so the cap falls inside a rune
		}
		total += len(text)
		feedChunk(w, text)
		if turn.text.Len() > outputCap {
			t.Fatalf("buffer %d bytes after chunk %d, cap %d", turn.text.Len(), i, outputCap)
		}
	}
	if !utf8.ValidString(turn.text.String()) {
		t.Fatal("buffered text is not valid UTF-8")
	}
	select {
	case <-turn.admitted:
	default:
		t.Fatal("dropping chunks must still mark the turn admitted")
	}
	if turn.text.Len()+turn.dropped != total {
		t.Fatalf("kept %d + dropped %d != received %d", turn.text.Len(), turn.dropped, total)
	}
	turn.done <- turnReply{stop: "end_turn"}
	result, err := turn.Wait(context.Background())
	if err != nil || result.Outcome != "completed" || !result.Truncated {
		t.Fatalf("result: %+v err=%v", result, err)
	}
	if !utf8.ValidString(result.Result) || protocol.ValidateTurnResult(result) != nil {
		t.Fatalf("result invalid: %v", protocol.ValidateTurnResult(result))
	}
	i := strings.LastIndex(result.Result, "\n[truncated by openclaw-peer: ")
	if i < 0 {
		t.Fatal("no truncation note")
	}
	var omitted int
	if _, err := fmt.Sscanf(result.Result[i:], "\n[truncated by openclaw-peer: %d bytes omitted]", &omitted); err != nil {
		t.Fatal(err)
	}
	if kept := i; kept+omitted != total {
		t.Fatalf("kept %d + omitted %d != received %d", kept, omitted, total)
	}
}

// DSH-F-01: a Close joining an in-flight Close that never finishes (its session/close write
// blocked on the child) returns after closeJoinBound instead of hanging Serve.
func TestCloseJoinerIsBoundedWhenCloserBlocks(t *testing.T) {
	old := closeJoinBound
	closeJoinBound = 200 * time.Millisecond
	t.Cleanup(func() { closeJoinBound = old })
	w := &Wrapper{closing: make(chan struct{})} // a closer that never completes
	returned := make(chan error, 1)
	start := time.Now()
	go func() { returned <- w.Close(context.Background(), kit.SessionCloseRequest{}) }()
	select {
	case err := <-returned:
		if d := time.Since(start); d < 150*time.Millisecond {
			t.Fatalf("joiner returned after %v, before the bound", d)
		}
		if err == nil || !strings.Contains(err.Error(), "in-flight close is still running") {
			t.Fatalf("joiner error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("joiner hung on a blocked closer")
	}
}

// Bridge loss returns after native cleanup without scheduling SDK shutdown. The
// daemon may retain results indefinitely (auto_close_ms=0), or until explicit close.
func TestBridgeLossIdleDoesNotShutDownWorker(t *testing.T) {
	gone := make(chan struct{}, 1)
	w := &Wrapper{shutdown: func() { gone <- struct{}{} }}
	returned := make(chan struct{})
	go func() { w.onBridgeLost(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("bridge loss did not return after native cleanup")
	}
	select {
	case <-gone:
		t.Fatal("bridge loss shut down the result-serving worker")
	default:
	}
}

// CR-L2b-1: once the bridge is lost no new Run starts (no prompt may reach the child without a
// bridge to steer through or serve the lane voice).
func TestBeginRunRefusedAfterBridgeLoss(t *testing.T) {
	w := &Wrapper{}
	if err := w.beginRun(&kit.Run{}); err != nil {
		t.Fatalf("before the loss: %v", err)
	}
	w.endRun(&kit.Run{})
	w.mu.Lock()
	w.bridgeLost = true
	w.mu.Unlock()
	if err := w.beginRun(&kit.Run{}); err == nil || !strings.Contains(err.Error(), "bridge lost") {
		t.Fatalf("beginRun after the loss: %v", err)
	}
}

// CR-L3-1: a steer that cannot be written (the bridge is gone, noticed during Deliver rather
// than by the bridgeLost flag) is definitely not submitted — not a NotRunning that would send
// the message to a wake run that then fails.
func TestDeliverNotWrittenIsNotSubmitted(t *testing.T) {
	b, srv := dialFake(t, fakebridge.Options{})
	b.Settings(context.Background())
	srv.Drop()
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge not marked lost")
	}
	w := &Wrapper{bridge: b, process: &acp.Process{}, key: "agent:lane:sb-x"}
	w.active = &acpTurn{w: w, done: make(chan turnReply, 1)}
	req := kit.DeliveryRequest{MessageID: "m1", From: kit.DeliverySource{SessionID: "parent@local", Name: "parent", Product: "claude-peer", Groups: []string{"g"}}, Body: "hi"}
	r, err := w.Deliver(context.Background(), req, nil)
	if err != nil || r.Disposition != "rejected" || r.Reason != "not_submitted" {
		t.Fatalf("receipt %+v err %v, want rejected/not_submitted", r, err)
	}
}

// Register can succeed while the loss callback has already cleaned up the still
// unpublished lane. Serialize that ordering without relying on socket timing.
func TestAttachDoesNotPublishNativeAfterBridgeLoss(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FAKEACP_STATE", filepath.Join(root, "state"))
	t.Setenv("FAKEACP_LOG", filepath.Join(root, "fake.log"))
	t.Setenv("FAKEACP_SCRIPT", "")
	w := &Wrapper{cfg: Config{CredentialDir: filepath.Join(root, "cli")}}
	b, _ := dialFake(t, fakebridge.Options{OnRegister: func(_, _, _ string) string {
		w.onBridgeLost()
		return "" // the registration reply still succeeds
	}})
	w.bridge = b
	lock, err := host.AcquireSessionLock(filepath.Join(root, "bus.sock"), "openclaw-peer", "test-session")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var child *acp.Process
	_, err = w.attach(context.Background(), lock, "lane", "test-session", kit.OpenOptions{}, func(p *acp.Process) (string, error) {
		child = p
		return "native-session", nil
	})
	if child != nil {
		t.Cleanup(func() { _ = child.Close(context.Background()) })
	}
	if err == nil || err.Error() != "lane bridge lost" {
		t.Fatalf("attach = %v, want bridge-loss refusal", err)
	}
	if w.process != nil || w.lock != nil || w.key != "" {
		t.Fatal("published native resources after loss")
	}
	if _, err := lock.File().Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("unpublished native lock was not released: %v", err)
	}
	if child == nil {
		t.Fatal("test did not start native child")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := child.Call(ctx, "initialize", map[string]any{}, nil); err == nil {
		t.Fatal("unpublished native child still accepts calls")
	}
}

func TestNativeExitRefusesNewRunWithBridgeLossPriority(t *testing.T) {
	w := &Wrapper{}
	w.markNativeExit(errors.New("ordinary turn error"))
	if err := w.beginRun(nil); err != nil {
		t.Fatalf("ordinary turn error disabled native work: %v", err)
	}
	w.markNativeExit(fmt.Errorf("wrapped: %w", acp.ErrChildExited))
	if err := w.beginRun(nil); err == nil || err.Error() != "lane native session exited" {
		t.Fatalf("post-exit Run: %v", err)
	}
	w.bridgeLost = true
	if err := w.beginRun(nil); err == nil || err.Error() != "lane bridge lost" {
		t.Fatalf("combined loss should prefer bridge reason: %v", err)
	}
}

// Done is supplied independently of the SDK transport so this cleanup boundary
// can be tested without a Unix socket or access to the SDK's private Run fields.
type nativeCleanupRun struct{ done <-chan struct{} }

func (r nativeCleanupRun) Done() <-chan struct{} { return r.done }

func TestEndRunClosesNativeResourcesAfterChildExit(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "unused.sock") // lock namespace only; no socket bind
	lock, err := host.AcquireSessionLock(socket, "openclaw-peer", "native-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	process, err := acp.Start(context.Background(), []string{"/bin/sh", "-c", `read line; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'`}, os.Environ())
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	w := &Wrapper{nativeLost: true, process: process, lock: lock, acpSessionID: "session", key: "native-cleanup"}
	t.Cleanup(func() { _ = w.Close(context.Background(), kit.SessionCloseRequest{}) })
	done := make(chan struct{})
	w.endRun(nativeCleanupRun{done: done})
	// Cleanup must wait for publication of the terminal, not merely Run's return.
	time.Sleep(30 * time.Millisecond)
	w.mu.Lock()
	intact := w.process == process && w.lock == lock
	w.mu.Unlock()
	if !intact {
		t.Fatal("native resources cleared before Run.Done")
	}
	if probe, err := host.AcquireSessionLock(socket, "openclaw-peer", "native-cleanup"); err == nil {
		_ = probe.Close()
		t.Fatal("native lock released before Run.Done")
	}
	close(done)
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		cleaned := w.process == nil && w.lock == nil && w.closing == nil
		w.mu.Unlock()
		if cleaned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native process/lock were not cleared after Run.Done with nativeLost")
		}
		time.Sleep(10 * time.Millisecond)
	}
	probe, err := host.AcquireSessionLock(socket, "openclaw-peer", "native-cleanup")
	if err != nil {
		t.Fatalf("native lock still held after cleanup: %v", err)
	}
	_ = probe.Close()
	if err := process.Close(context.Background()); err != nil {
		t.Fatalf("native process cleanup failed: %v", err)
	}
}
