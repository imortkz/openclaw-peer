package lane

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	kit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/bus/sdk/go/protocol"
	"github.com/imortkz/openclaw-peer/lane/internal/acp"
	"github.com/sessionbus/peer-common/host"
)

var errCallbackPending = errors.New("lane callback not built yet")

type Wrapper struct {
	cfg               Config
	socket            string
	caller            *kit.Caller
	shutdown          func()
	mu                sync.Mutex
	key, acpSessionID string
	permissionMode    string
	reasoningEffort   string
	process           *acp.Process
	lock              *host.SessionLock
	bridge            *Bridge
	handoff           host.Handoff
	active            *acpTurn
	liveRun           *kit.Run      // the Run being served (any kind), nil when idle
	closing           chan struct{} // non-nil while a Close is in flight; closed when it is done
	bridgeLost        bool
	nativeLost        bool
	adminGuardDone    bool
	adminGuardErr     error
}

var _ kit.WorkerCallbacks = (*Wrapper)(nil)

// New captures the socket before kit.Worker.Serve clears launch variables.
func New(cfg Config, bridge *Bridge) *Wrapper {
	w := &Wrapper{cfg: cfg, bridge: bridge, socket: os.Getenv(host.SocketEnv)}
	bridge.OnLost(w.onBridgeLost)
	return w
}

// onBridgeLost refuses new native work and closes the ACP child. A live Run finishes
// first so the SDK can publish its result. Keep the worker's SDK connection alive:
// result retention and worker retirement belong to the daemon's lane policy, not
// to a separate bridge-loss deadline.
func (w *Wrapper) onBridgeLost() {
	w.mu.Lock()
	w.bridgeLost = true
	busy := w.liveRun != nil
	w.mu.Unlock()
	if busy {
		fmt.Fprintln(os.Stderr, "bridge lost: closing native session after current run")
		return
	}
	fmt.Fprintln(os.Stderr, "bridge lost: closing native session; retaining results until lane retirement")
	_ = w.Close(context.Background(), kit.SessionCloseRequest{})
}

func (w *Wrapper) beginRun(run *kit.Run) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bridgeLost {
		return errors.New("lane bridge lost")
	}
	if w.nativeLost {
		return errors.New("lane native session exited")
	}
	w.liveRun = run
	return nil
}

func (w *Wrapper) endRun(run interface{ Done() <-chan struct{} }) {
	w.mu.Lock()
	w.liveRun = nil
	lost := w.bridgeLost || w.nativeLost
	w.mu.Unlock()
	if lost {
		go func() {
			<-run.Done()
			// Done means the daemon has the run's ready frame. Close native resources,
			// but let the SDK retain status/wait/ack until ordinary lane retirement.
			_ = w.Close(context.Background(), kit.SessionCloseRequest{})
		}()
	}
}
func (w *Wrapper) SetCaller(c *kit.Caller) { w.caller = c }
func (w *Wrapper) SetShutdown(f func())    { w.shutdown = f }

func (w *Wrapper) Hello(context.Context) (kit.HelloDescription, error) {
	parts := make([]string, 0, len(w.cfg.Agents))
	for _, a := range w.cfg.Agents {
		part := a.ID + " — " + a.Description
		if a.Models != nil && a.Models.Allow != nil {
			part += " [models: " + formatModelAllow(a.Models.Allow, 20)
			if a.Models.Default != "" {
				part += "; default: " + a.Models.Default
			}
			part += "]"
		}
		parts = append(parts, part)
	}
	desc := "OpenClaw agent for this lane"
	if w.cfg.SettingsErr != nil {
		desc += " (lane settings unavailable: " + w.cfg.SettingsErr.Error() + ")"
	}
	if len(parts) == 1 {
		desc += " (default: " + parts[0] + ")"
	} else if len(parts) > 1 {
		desc += ", required: " + strings.Join(parts, "; ")
	}
	return kit.HelloDescription{Product: "openclaw-peer", SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"}, SupportsMessageRun: true, ExtraArguments: []kit.ExtraArgument{{Name: "--agent", TakesValue: true, Description: desc}}}, nil
}

// execApprovalsGetProbeMethod is a harmless, read-only Gateway method (empty params;
// core-descriptors.ts classifies it as needing exactly operator.admin, not an escalation target
// like sessions.create — so a non-admin device gets the platform's own clean missing-scope
// refusal, never call.ts's cwd-specific admin-retry path) used only to detect whether this lane's
// own Gateway credential carries admin.
const execApprovalsGetProbeMethod = "exec.approvals.get"

// isConfirmedNonAdminRefusal reports whether a probe refusal is one of the two shapes 8.2
// actually produces for a non-admin credential calling an operator.admin-only method: the plain
// method-scope gate's missing-scope detail (the ordinary case for exec.approvals.get, which is
// never a sessions.create-style escalation target), or — defensively, the
// device-token scope-mismatch text documents for the unrelated sessions.create retry path,
// in case some other admin-gated method ever produces it too. Anything else (a different
// Gateway error, a transport failure) is not a confirmed answer either way.
func isConfirmedNonAdminRefusal(gwErr *acp.GatewayError) bool {
	if gwErr == nil {
		return false
	}
	return gwErr.MissingScope == "operator.admin" || strings.Contains(gwErr.Message, deviceTokenScopeMismatchText)
}

// assertNonAdminCredential refuses a lane whose OpenClaw connection has admin scope.
// This enforces "no operator.admin anywhere" in code, not only in README. It probes
// execApprovalsGetProbeMethod through the same credential environment every other Gateway
// call uses — identical for docker-exec (smoke) and plain (host) commands, and catches a shared
// admin token exactly like an admin-paired device.
//
// Fails closed: the probe succeeding means the credential is admin (refuse,
// cached — that fact does not change). A confirmed non-admin refusal (isConfirmedNonAdminRefusal)
// means it is not (proceed, cached). Anything else — a transport error, a timeout, an unrecognized
// Gateway error — is not a definitive answer either way, so Open is refused for *that* attempt
// with "lane credential check failed: <reason>" but the outcome is deliberately not cached: a
// transient Gateway hiccup must not permanently poison the worker into refusing every future Open.
//
// Called from Open/openResume, after every purely local check and before mint/lock — same
// placement rule as the other Gateway calls in this file: a refusal that never
// needed the Gateway (bad agent, bad cwd, bad mode, …) still makes no Gateway call at all.
// Both fixed commands use ChildEnv, so this probe checks the same credential as ACP.
func (w *Wrapper) assertNonAdminCredential(ctx context.Context) error {
	w.mu.Lock()
	if w.adminGuardDone {
		err := w.adminGuardErr
		w.mu.Unlock()
		return err
	}
	w.mu.Unlock()

	_, gwErr, err := acp.RunGateway(ctx, []string{"openclaw", "gateway", "call"}, execApprovalsGetProbeMethod, map[string]any{}, w.cfg.ChildEnv(os.Environ()))

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.adminGuardDone {
		// Another Open already resolved this while the probe above was in flight.
		return w.adminGuardErr
	}
	switch {
	case gwErr == nil && err == nil:
		w.adminGuardErr = errors.New("lane credential has operator.admin; pair a read/write/approvals device (lane/README.md) and point the lane at its state dir")
		w.adminGuardDone = true
	case isConfirmedNonAdminRefusal(gwErr):
		w.adminGuardErr = nil
		w.adminGuardDone = true
	default:
		reason := "unknown Gateway error"
		switch {
		case gwErr != nil:
			reason = gwErr.Message
		case err != nil:
			reason = err.Error()
		}
		return fmt.Errorf("lane credential check failed: %s", reason)
	}
	return w.adminGuardErr
}

func (w *Wrapper) Open(ctx context.Context, request kit.OpenRequest) (kit.OpenResult, error) {
	if w.cfg.SettingsErr != nil {
		return kit.OpenResult{}, w.cfg.SettingsErr
	}
	if request.ResumeSessionID != "" {
		return w.openResume(ctx, request)
	}
	if err := w.cfg.ValidateMode(request.Open.PermissionMode); err != nil {
		return kit.OpenResult{}, err
	}
	agent, err := SelectAgent(w.cfg, request.Open.Arguments, "")
	if err != nil {
		return kit.OpenResult{}, err
	}
	cwd := agent.Workspace
	if request.Open.Cwd != "" {
		// ResolveCwd runs before mint/lock: no point minting a key or taking
		// the lane lock for a cwd that's going to be refused anyway.
		if cwd, err = ResolveCwd(agent, request.Open.Cwd); err != nil {
			return kit.OpenResult{}, err
		}
	}
	if request.Open.Model != "" {
		if err := ValidateModel(request.Open.Model); err != nil {
			return kit.OpenResult{}, err
		}
	}
	if request.Open.ReasoningEffort != "" {
		if err := ValidateReasoningEffort(request.Open.ReasoningEffort); err != nil {
			return kit.OpenResult{}, err
		}
	}
	// Probe after every purely local check (so a refusal that never touches the Gateway still
	// makes no Gateway call at all — same invariant test-writer's TestParityAgentChoiceAndRefusals
	// already pins for every one of these local refusals), but before minting a key or taking the
	// lane lock for a credential this Open would refuse anyway.
	if err := w.assertNonAdminCredential(ctx); err != nil {
		return kit.OpenResult{}, err
	}
	if w.socket == "" {
		return kit.OpenResult{}, errors.New("SESSIONBUS_SOCKET is required for lane lock")
	}
	key, err := mintKey(agent.ID)
	if err != nil {
		return kit.OpenResult{}, err
	}
	lock, err := host.AcquireSessionLock(w.socket, "openclaw-peer", key)
	if err != nil {
		return kit.OpenResult{}, err
	}
	if request.Open.Cwd != "" {
		// Create the Gateway session after the lock, before any ACP child. On failure nothing ACP-side
		// has started, so releasing the lock is the whole cleanup.
		if err := w.gatewayCreateSession(ctx, key, request.Open.Cwd, cwd); err != nil {
			_ = lock.Close()
			return kit.OpenResult{}, err
		}
	}
	if request.Open.Model != "" {
		// Patch the session-only model after the optional sessions.create, before any ACP child.
		if err := w.gatewayPatchModel(ctx, key, request.Open.Model, agent); err != nil {
			_ = lock.Close()
			return kit.OpenResult{}, err
		}
	}
	return w.attach(ctx, lock, agent.ID, key, request.Open, func(p *acp.Process) (string, error) {
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := p.Call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}, "_meta": map[string]string{"sessionKey": key}}, &created); err != nil {
			return "", err
		}
		if created.SessionID == "" {
			return "", errors.New("ACP session/new returned no sessionId")
		}
		return created.SessionID, nil
	})
}

// attach starts the ACP child for an Open (fresh or resume), runs open to create or resume the
// ACP session, registers the lane key with the plugin over the bridge, and publishes the lane
// fields. On any failure the child is closed and the lock released.
func (w *Wrapper) attach(ctx context.Context, lock *host.SessionLock, agentID, key string, open kit.OpenOptions, session func(p *acp.Process) (acpSessionID string, err error)) (kit.OpenResult, error) {
	process, err := acp.Start(ctx, []string{"openclaw", "acp"}, w.cfg.ChildEnv(os.Environ()))
	if err != nil {
		_ = lock.Close()
		return kit.OpenResult{}, err
	}
	// The caller's permission_mode must be in force before the first session/request_permission
	// can arrive (before the lane fields are published under the lock below) — otherwise
	// answerPermission decides under mode "".
	w.mu.Lock()
	w.permissionMode = open.PermissionMode
	w.mu.Unlock()
	cleanup := func(err error) (kit.OpenResult, error) {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = process.Close(c)
		_ = lock.Close()
		w.mu.Lock()
		w.permissionMode = ""
		w.mu.Unlock()
		return kit.OpenResult{}, err
	}
	process.OnRequest = w.answerPermission
	process.OnUpdate = w.onUpdate
	var initialized json.RawMessage
	err = process.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, &initialized)
	if err != nil {
		return cleanup(err)
	}
	acpSessionID, err := session(process)
	if err != nil {
		return cleanup(err)
	}
	process.SetUpdateSessionID(acpSessionID)
	if err := w.bridge.Register(ctx, key, agentID, acpSessionID); err != nil {
		return cleanup(fmt.Errorf("lane register: %w", err))
	}
	w.mu.Lock()
	// Loss may have been reported after Register succeeded but before publication.
	// Its idle cleanup could not see this child; never publish it after that cleanup.
	if w.bridgeLost {
		w.mu.Unlock()
		return cleanup(errors.New("lane bridge lost"))
	}
	w.key, w.acpSessionID, w.process, w.lock, w.permissionMode, w.reasoningEffort = key, acpSessionID, process, lock, open.PermissionMode, open.ReasoningEffort
	w.mu.Unlock()
	return kit.OpenResult{SessionID: key}, nil
}

func mintKey(agentID string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint lane key: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("agent:%s:sb-%x-%x-%x-%x-%x", agentID, b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// formatModelAllow uses limit <= 0 for the full refusal list.
func formatModelAllow(allow []string, limit int) string {
	if len(allow) == 0 {
		return "any"
	}
	if limit > 0 && len(allow) > limit {
		return strings.Join(allow[:limit], ", ") + fmt.Sprintf(", … +%d more", len(allow)-limit)
	}
	return strings.Join(allow, ", ")
}

// gatewayPatchModel implements session-only model via write-scope sessions.patch, after the
// optional sessions.create, before any ACP child (fresh Open only — never on resume).
func (w *Wrapper) gatewayPatchModel(ctx context.Context, key, model string, agent Agent) error {
	_, gwErr, err := acp.RunGateway(ctx, []string{"openclaw", "gateway", "call"}, "sessions.patch", map[string]any{"key": key, "model": model}, w.cfg.ChildEnv(os.Environ()))
	if gwErr != nil {
		detail := gwErr.Message
		if agent.Models != nil && agent.Models.Allow != nil {
			detail += "; allowed for " + agent.ID + ": " + formatModelAllow(agent.Models.Allow, 0)
			if agent.Models.Default != "" {
				detail += "; default: " + agent.Models.Default
			}
		}
		return fmt.Errorf("unsupported value model=%s (refused by OpenClaw: %s)", model, detail)
	}
	if err != nil {
		return fmt.Errorf("gateway call failed: %s", err)
	}
	return nil
}

// deviceTokenScopeMismatchText is the text 8.2 sends when its CLI retries a sessions.create
// missing-operator.admin refusal with admin scope (8.2 src/gateway/call.ts:1295-1329;
// 9.6 src/gateway/call.ts:1214-1248,
// shouldEscalateSessionCreateCwdScope/callGatewayWithScopeEscalation) and that retry fails,
// because the lane's device is non-admin by design (8.2 src/gateway/server/ws-connection/
// auth-messages.ts:78-79; 9.6 same path:84-85, scope_mismatch
// case): "unauthorized: device token scope mismatch (re-pair or approve scope upgrade)". It is
// the retry's failure, not the original clean missing-scope one, that reaches here.
const deviceTokenScopeMismatchText = "device token scope mismatch"

// formatCwdGatewayRefusal builds the cwd-refusal text. The Gateway message handler returns before
// authorizeGatewayConnectDevice, the only caller of requestDevicePairing, can run. Three repeated
// fixture refusals also left `pending` empty and the device's scopes unchanged. The failed retry
// cannot mint a pending pairing or scope-upgrade request or widen the device's scopes, so an operator
// seeing "re-pair or approve scope upgrade" in the raw text should not act on it. Any other
// Gateway refusal keeps its plain, unmodified text.
func formatCwdGatewayRefusal(original, message string) string {
	if strings.Contains(message, deviceTokenScopeMismatchText) {
		return fmt.Sprintf("unsupported value cwd=%s (refused by OpenClaw: outside the agent workspace; the lane credential is non-admin by design — do not approve a scope upgrade: %s)", original, message)
	}
	return fmt.Sprintf("unsupported value cwd=%s (refused by OpenClaw: %s)", original, message)
}

// gatewayCreateSession handles a fresh Open with a cwd: after the lock,
// before any ACP child. A GatewayError maps to the cwd-refusal text; any other failure is a
// transport-level spawn_failed (the caller wraps it into the SDK's stderr_tail via a plain error).
// the error names the caller's original value, and not the realpath, which can
// differ from it as soon as a symlink is involved (e.g. macOS /var -> /private/var).
func (w *Wrapper) gatewayCreateSession(ctx context.Context, key, original, realpath string) error {
	_, gwErr, err := acp.RunGateway(ctx, []string{"openclaw", "gateway", "call"}, "sessions.create", map[string]any{"key": key, "cwd": realpath}, w.cfg.ChildEnv(os.Environ()))
	if gwErr != nil {
		return errors.New(formatCwdGatewayRefusal(original, gwErr.Message))
	}
	if err != nil {
		return fmt.Errorf("gateway call failed: %s", err)
	}
	return nil
}

// gatewayCheckReplayedCwd implements the drift check (via sessions.list, not
// sessions.get, which in 8.2 returns only {messages}): the resumed lane's realpath must match the
// spawnedCwd the platform recorded when the lane's cwd was created. cwd=<v> names the
// caller's original value; "now <v>" in the drift reason is still the realpath, since that's the
// value actually being compared against the recorded spawnedCwd.
func (w *Wrapper) gatewayCheckReplayedCwd(ctx context.Context, key, original, realpath string) error {
	raw, gwErr, err := acp.RunGateway(ctx, []string{"openclaw", "gateway", "call"}, "sessions.list", map[string]any{"search": key}, w.cfg.ChildEnv(os.Environ()))
	if gwErr != nil {
		return fmt.Errorf("unsupported value cwd=%s (refused by OpenClaw: %s)", original, gwErr.Message)
	}
	if err != nil {
		return fmt.Errorf("gateway call failed: %s", err)
	}
	var listing struct {
		Sessions []struct {
			Key        string `json:"key"`
			SpawnedCwd string `json:"spawnedCwd"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return fmt.Errorf("gateway call failed: %s", err)
	}
	for _, row := range listing.Sessions {
		if row.Key != key {
			continue
		}
		if row.SpawnedCwd != realpath {
			return fmt.Errorf("unsupported value cwd=%s (lane cwd changed since creation: now %s, created %s)", original, realpath, row.SpawnedCwd)
		}
		return nil
	}
	return fmt.Errorf("unsupported value cwd=%s (session not found)", original)
}

// openResume implements the resume path: admission by key shape
// before any lock, child or frame, and agent resolution, both via SelectAgent (one
// admission rule, not duplicated here); a non-empty replayed cwd is checked against the current
// declared root and the platform's recorded spawnedCwd before any ACP work; no
// sessions.create, no model patch.
func (w *Wrapper) openResume(ctx context.Context, request kit.OpenRequest) (kit.OpenResult, error) {
	if w.cfg.SettingsErr != nil {
		return kit.OpenResult{}, w.cfg.SettingsErr
	}
	key := request.ResumeSessionID
	agent, err := SelectAgent(w.cfg, request.Open.Arguments, key)
	if err != nil {
		return kit.OpenResult{}, err
	}
	if err := w.cfg.ValidateMode(request.Open.PermissionMode); err != nil {
		return kit.OpenResult{}, err
	}
	if request.Open.ReasoningEffort != "" {
		if err := ValidateReasoningEffort(request.Open.ReasoningEffort); err != nil {
			return kit.OpenResult{}, err
		}
	}
	cwd := agent.Workspace
	if request.Open.Cwd != "" {
		realpath, err := ResolveCwd(agent, request.Open.Cwd)
		if err != nil {
			return kit.OpenResult{}, err
		}
		cwd = realpath
	}
	// Resolve local inputs before probing; do not read session state with an admin credential.
	if err := w.assertNonAdminCredential(ctx); err != nil {
		return kit.OpenResult{}, err
	}
	if request.Open.Cwd != "" {
		if err := w.gatewayCheckReplayedCwd(ctx, key, request.Open.Cwd, cwd); err != nil {
			return kit.OpenResult{}, err
		}
	}
	if w.socket == "" {
		return kit.OpenResult{}, errors.New("SESSIONBUS_SOCKET is required for lane lock")
	}
	lock, err := host.AcquireSessionLock(w.socket, "openclaw-peer", key)
	if err != nil {
		return kit.OpenResult{}, err
	}
	return w.attach(ctx, lock, agent.ID, key, request.Open, func(p *acp.Process) (string, error) {
		if err := p.Call(ctx, "session/resume", map[string]any{"sessionId": key, "cwd": cwd, "mcpServers": []any{}}, nil); err != nil {
			return "", err
		}
		return key, nil
	})
}

func (w *Wrapper) answerPermission(_ context.Context, method string, raw json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, errors.New("method not found")
	}
	var req struct {
		ToolCall struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(raw, &req)
	pr := PermissionRequest{ToolKind: req.ToolCall.Kind, ToolTitle: req.ToolCall.Title}
	for _, o := range req.Options {
		pr.Options = append(pr.Options, PermissionOption{OptionID: o.OptionID, Kind: o.Kind})
	}
	w.mu.Lock()
	mode := w.permissionMode
	w.mu.Unlock()
	d := Decide(mode, pr)
	logPermissionDecision(mode, req.ToolCall.Title, d, w.cfg.AuditLog)
	if d.OptionID != "" {
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": d.OptionID}}, nil
	}
	return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
}

// logPermissionDecision writes an audit line for each permission decision (any group member can request exec):
// one worker stderr line per decision, "permission <mode> <tool title <=80> -> allow|deny". When
// configured, the same line is appended to lanes.auditLog for observation outside the
// daemon's child-stderr tail buffer. The
// title comes from the ACP child, an untrusted-input boundary: control characters
// (including an embedded newline, which would otherwise forge a second, fake-looking log line)
// are stripped before the 80-rune clip. cancelled maps to deny — nothing was allowed.
func logPermissionDecision(mode, title string, d Decision, auditPath string) {
	if mode == "" {
		mode = "default"
	}
	outcome := "deny"
	if d.Outcome == "allow" {
		outcome = "allow"
	}
	line := fmt.Sprintf("permission %s %s -> %s\n", mode, clipTitle(title, 80), outcome)
	_, _ = fmt.Fprint(os.Stderr, line)
	if auditPath == "" {
		return
	}
	file, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "permission audit write failed: %v\n", err)
		return
	}
	if _, err = file.WriteString(line); err != nil {
		fmt.Fprintf(os.Stderr, "permission audit write failed: %v\n", err)
	}
	if err = file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "permission audit close failed: %v\n", err)
	}
}

func clipTitle(title string, maxRunes int) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, title)
	runes := []rune(clean)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func (w *Wrapper) onUpdate(_ string, raw json.RawMessage) {
	var u struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &u) != nil || u.SessionUpdate != "agent_message_chunk" || u.Content.Type != "text" {
		return
	}
	w.mu.Lock()
	if t := w.active; t != nil {
		t.appendBounded(u.Content.Text)
		t.markAdmitted()
	}
	w.mu.Unlock()
}

// outputCap bounds the ACP text a turn keeps in memory (the result cap below is 900 KiB too).
const outputCap = 900 << 10

// appendBounded keeps chunk while the buffer is under outputCap, cutting at a rune boundary, and
// counts the bytes it drops. Called with w.mu held.
func (t *acpTurn) appendBounded(chunk string) {
	room := outputCap - t.text.Len()
	if room >= len(chunk) {
		t.text.WriteString(chunk)
		return
	}
	cut := 0
	if room > 0 {
		cut = room
		for cut > 0 && !utf8.RuneStart(chunk[cut]) {
			cut--
		}
		t.text.WriteString(chunk[:cut])
	}
	t.dropped += len(chunk) - cut
}

type acpTurn struct {
	w               *Wrapper
	process         *acp.Process
	sid             string
	reasoningEffort string
	text            strings.Builder
	dropped         int // bytes of agent text over outputCap, never buffered
	done            chan turnReply
	admitted        chan struct{}
	admitOnce       sync.Once
	notSent         chan struct{}
	notSentOne      sync.Once
}
type turnReply struct {
	stop string
	err  error
}

// markAdmitted fires on the first sign the ACP peer is handling this turn: a streamed update, or
// a successful session/prompt response. Used by Run(Delivery) to report the "injected" receipt.
func (t *acpTurn) markAdmitted() { t.admitOnce.Do(func() { close(t.admitted) }) }

// markNotSent fires when session/prompt never reached the child at all: the ACP client
// was already closed, or the write itself failed. Used by Run(Delivery) to report
// rejected/not_submitted instead of leaving admission merely "uncertain".
func (t *acpTurn) markNotSent() { t.notSentOne.Do(func() { close(t.notSent) }) }

func (w *Wrapper) startTurn(ctx context.Context, text string) (host.Turn, error) {
	w.mu.Lock()
	p, sid, level := w.process, w.acpSessionID, w.reasoningEffort
	if p == nil || sid == "" {
		w.mu.Unlock()
		return nil, errors.New("lane is not open")
	}
	t := &acpTurn{w: w, process: p, sid: sid, reasoningEffort: level, done: make(chan turnReply, 1), admitted: make(chan struct{}), notSent: make(chan struct{})}
	w.active = t
	w.mu.Unlock()
	params := map[string]any{"sessionId": sid, "prompt": []map[string]string{{"type": "text", "text": text}}}
	if level != "" {
		// Every caller prompt carries _meta.thinking, never a separate
		// session/set_config_option call.
		params["_meta"] = map[string]string{"thinking": level}
	}
	go func() {
		var reply struct {
			StopReason string `json:"stopReason"`
		}
		err := p.Call(ctx, "session/prompt", params, &reply)
		switch {
		case err == nil:
			t.markAdmitted()
		case errors.Is(err, acp.ErrNotSent):
			t.markNotSent()
		}
		t.done <- turnReply{reply.StopReason, err}
	}()
	return t, nil
}

func (t *acpTurn) Wait(ctx context.Context) (kit.TurnResult, error) {
	var reply turnReply
	select {
	case reply = <-t.done:
	case <-ctx.Done():
		return kit.TurnResult{}, ctx.Err()
	}
	t.w.mu.Lock()
	body, dropped := t.text.String(), t.dropped
	if t.w.active == t {
		t.w.active = nil
	}
	t.w.mu.Unlock()
	if reply.err != nil {
		return kit.TurnResult{}, reply.err
	}
	// OpenClaw refuses a model-unsupported reasoning_effort per turn, as ordinary assistant
	// text with no execution — recognised by the exact prefix for the level this turn sent.
	if t.reasoningEffort != "" {
		if prefix := thinkingUnsupportedPrefix(t.reasoningEffort); strings.HasPrefix(body, prefix) {
			return cappedResultDropped(kit.TurnResult{Outcome: "failed", Result: fmt.Sprintf("unsupported value reasoning_effort=%s (%s)", t.reasoningEffort, body)}, dropped), nil
		}
	}
	outcome := "failed"
	switch reply.stop {
	case "end_turn":
		outcome = "completed"
	case "cancelled":
		outcome = "interrupted"
	}
	return cappedResultDropped(kit.TurnResult{Outcome: outcome, Result: body, NativeStopReason: reply.stop}, dropped), nil
}

// Interrupt sends session/cancel once, as a notification: the fake (and 8.2) never reply to it,
// so a Call here would hang. host.Handoff guarantees at most one call per turn.
func (t *acpTurn) Interrupt(context.Context) error {
	return t.process.Notify("session/cancel", map[string]any{"sessionId": t.sid})
}

// charByteOffset returns the largest byte offset <= upperBound at which s[:offset] contains at
// most maxChars runes (a rune boundary by construction).
func charByteOffset(s string, upperBound, maxChars int) int {
	if upperBound > len(s) {
		upperBound = len(s)
	}
	if maxChars <= 0 {
		return 0
	}
	count := 0
	for i := range s[:upperBound] {
		if count == maxChars {
			return i
		}
		count++
	}
	return upperBound
}

func cappedResult(result kit.TurnResult) kit.TurnResult { return cappedResultDropped(result, 0) }

// cappedResultDropped is cappedResult for a result whose source already omitted dropped bytes at
// receive time: they count in the truncation note and force Truncated.
func cappedResultDropped(result kit.TurnResult, dropped int) kit.TurnResult {
	original := result.Result
	keep := len(original)
	if keep > 900<<10 {
		keep = 900 << 10
	}
	// Cut to the character budget directly instead of discovering it by
	// halving, but only when a cut is actually needed — otherwise an answer that already fits
	// (within a few characters of protocol.MaxTextRunes) gets clipped for no reason. Use
	// len(original) as the note's omitted-byte-count estimate: an upper bound on its digit
	// count, so the budget below is conservative (never overshoots protocol.MaxTextRunes) even
	// though the real note (computed after keep is chosen) may end up one digit shorter.
	if keep < len(original) || dropped > 0 || utf8.RuneCountInString(original) > protocol.MaxTextRunes {
		note := fmt.Sprintf("\n[truncated by openclaw-peer: %d bytes omitted]", len(original)+dropped)
		budget := protocol.MaxTextRunes - utf8.RuneCountInString(note)
		keep = charByteOffset(original, keep, budget)
	}
	for {
		for keep > 0 && !utf8.ValidString(original[:keep]) {
			keep--
		}
		result.Result = original[:keep]
		result.Truncated = keep < len(original) || dropped > 0
		if result.Truncated {
			result.Result += fmt.Sprintf("\n[truncated by openclaw-peer: %d bytes omitted]", len(original)-keep+dropped)
		}
		if keep == 0 {
			return result
		}
		frame, _ := json.Marshal(protocol.RunStatus{Result: &result})
		// The turn.status frame-size cap is necessary but not sufficient: TurnRunResult.result
		// also carries the protocol's own maxLength (in Unicode characters, not bytes), which
		// ASCII-heavy text can exceed well under the frame-size ceiling.
		if len(frame) < protocol.MaxFrameBytes-4096 && protocol.ValidateTurnResult(result) == nil {
			return result
		}
		keep /= 2
	}
}

func (w *Wrapper) Run(ctx context.Context, run *kit.Run, input kit.RunInput) (kit.TurnResult, error) {
	if err := w.beginRun(run); err != nil {
		return kit.TurnResult{}, err
	}
	defer w.endRun(run)
	if input.Delivery != nil {
		return w.runDelivery(ctx, run, *input.Delivery)
	}
	if input.Text == nil {
		return kit.TurnResult{}, errCallbackPending
	}
	// A Text run with a slash command is refused outright, never reaching the agent; Delivery
	// is only neutralized (see runDelivery below).
	if refusesSlash(*input.Text) {
		return kit.TurnResult{}, errors.New("slash commands are not accepted over the lane")
	}
	text := neutralizeDirectives(*input.Text)
	result, err := w.handoff.Run(ctx, run, text, w.startTurn)
	w.markNativeExit(err)
	return result, err
}

// runDelivery drives a wake run's rendered message like Run(Text) — through the same Handoff, so
// Interrupt (which only ever looks at run.Native, set by Handoff.Run) works for wake runs too
// — but also reports the delivery receipt out of band as soon as admission is known,
// ahead of the turn's own completion.
//
// exactly one goroutine ever calls run.ReportDelivery for this delivery, and runDelivery
// blocks until it has (or until it's clear none ever will) before returning. That guarantees the
// report always beats the SDK's own fallback (runs.go's ReportDelivery call right after
// product.Run returns — which is w.Run, which is this function), instead of racing it, and it
// never leaves the reporting goroutine running past this call.
func (w *Wrapper) runDelivery(ctx context.Context, run *kit.Run, request kit.DeliveryRequest) (kit.TurnResult, error) {
	text, err := host.RenderNativeMessage(request)
	if err != nil {
		_ = run.ReportDelivery(kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil)
		return kit.TurnResult{}, err
	}
	// A Delivery is never refused for an inline directive; it is neutralized after rendering.
	text = neutralizeDirectives(text)
	finished := make(chan struct{})
	// reported starts pre-closed: if start is never called (Handoff.Run's own early-interrupt
	// path) or startTurn itself fails, nothing here owns the receipt, and the SDK's own fallback
	// (ReportDelivery is a sync.Once, so at most one of the two ever wins) is the safety net.
	reported := closedChan
	start := func(ctx context.Context, text string) (host.Turn, error) {
		native, err := w.startTurn(ctx, text)
		if err != nil {
			_ = run.ReportDelivery(kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil)
			return nil, err
		}
		t := native.(*acpTurn)
		owned := make(chan struct{})
		reported = owned
		go reportDelivery(run, t, finished, owned)
		return native, nil
	}
	result, err := w.handoff.Run(ctx, run, text, start)
	close(finished)
	<-reported
	w.markNativeExit(err)
	return result, err
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// reportDelivery is the single owner of this delivery's receipt: it races admitted/notSent
// against finished (closed once the turn itself is done, per Wait's synchronization with
// startTurn's goroutine — see acpTurn) so the receipt goes out the moment admission is known, and
// falls back to a definite classification once the turn has ended either way.
func reportDelivery(run *kit.Run, t *acpTurn, finished, reported chan struct{}) {
	defer close(reported)
	select {
	case <-t.admitted:
		run.Admitted()
		_ = run.ReportDelivery(kit.DeliveryReceipt{Disposition: "injected"}, nil)
		return
	case <-t.notSent:
		_ = run.ReportDelivery(kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil)
		return
	case <-finished:
	}
	select {
	case <-t.admitted:
		run.Admitted()
		_ = run.ReportDelivery(kit.DeliveryReceipt{Disposition: "injected"}, nil)
	case <-t.notSent:
		_ = run.ReportDelivery(kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil)
	default:
		// Sent, but the turn ended without ever reaching a streamed update or a successful
		// response: admission is genuinely unknown.
		_ = run.ReportDelivery(kit.DeliveryReceipt{}, &kit.ProtocolError{Code: protocol.Internal, Data: json.RawMessage(`"uncertain_native_admission"`)})
	}
}

// A child exit makes native work permanently unavailable. endRun closes native
// resources after run.Done, leaving SDK records available until lane retirement.
func (w *Wrapper) markNativeExit(err error) {
	if errors.Is(err, acp.ErrChildExited) {
		w.mu.Lock()
		w.nativeLost = true
		w.mu.Unlock()
	}
}

func (w *Wrapper) Interrupt(ctx context.Context, run *kit.Run) error {
	return w.handoff.Interrupt(ctx, run)
}

const (
	steerRetries    = 3
	steerRetryDelay = 100 * time.Millisecond
)

var errUncertainAdmission = &kit.ProtocolError{Code: protocol.Internal, Data: json.RawMessage(`"uncertain_native_admission"`)}

// Deliver answers a message.deliver that arrives while the lane may be running (D2/D13).
// No ACP turn (open but idle, or between w.active being set/cleared) → NotRunning: the daemon
// starts a wake run. Active turn → the text, directives neutralized, is steered into it through
// the plugin (`lane.steer`): `written` only after the plugin said eligible; a frame that was not
// written (bridge lost) is rejected/not_submitted. A steer the plugin
// cannot take is a safety net, not BUSY-MID (D21): a short retry, then NotRunning so the message
// waits for the next run. Nothing here waits for the turn boundary.
func (w *Wrapper) Deliver(ctx context.Context, req kit.DeliveryRequest, _ *kit.Run) (kit.DeliveryReceipt, error) {
	w.mu.Lock()
	p, active, key, lost := w.process, w.active, w.key, w.bridgeLost
	w.mu.Unlock()
	if p == nil {
		return kit.DeliveryReceipt{Disposition: "rejected", Reason: "native_unavailable"}, nil
	}
	if lost {
		return kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil
	}
	if active == nil {
		return kit.DeliveryReceipt{}, host.NotRunning()
	}
	text, err := host.RenderNativeMessage(req)
	if err != nil {
		return kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil
	}
	text = neutralizeDirectives(text)
	var reason string
	for attempt := 0; ; attempt++ {
		var eligible bool
		eligible, reason, err = w.bridge.Steer(ctx, key, req.MessageID, text)
		var refused *BridgeError
		switch {
		case errors.Is(err, ErrNotWritten):
			// Since a write error closes the connection, "not written" means the bridge is lost
			// (or was): same definite answer as the bridgeLost check above, never a wake run.
			return kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil
		case errors.Is(err, ErrNoReply):
			return kit.DeliveryReceipt{}, errUncertainAdmission
		case errors.As(err, &refused):
			return kit.DeliveryReceipt{Disposition: "rejected", Reason: "not_submitted"}, nil
		case err != nil:
			return kit.DeliveryReceipt{}, err
		case eligible:
			return kit.DeliveryReceipt{Disposition: "written"}, nil
		}
		if w.turnEnded(active) || attempt == steerRetries {
			break
		}
		select {
		case <-time.After(steerRetryDelay):
		case <-ctx.Done():
			return kit.DeliveryReceipt{}, host.NotRunning()
		}
		if w.turnEnded(active) {
			break
		}
	}
	if !w.turnEnded(active) {
		fmt.Fprintf(os.Stderr, "steer fallback (not BUSY-MID): reason=%s — message deferred to the next run\n", reason)
	}
	return kit.DeliveryReceipt{}, host.NotRunning()
}

// turnEnded reports that t is no longer the lane's active turn or has its prompt answer.
func (w *Wrapper) turnEnded(t *acpTurn) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active != t || len(t.done) > 0
}

// closeJoinBound bounds how long a Close waits for an in-flight Close. The closer's own 3 s
// budget does not bound it: its session/close write ignores ctx and blocks on a child that stops
// reading stdin (DSH-F-01). A var so tests can shorten it.
var closeJoinBound = 10 * time.Second

// Close is joining: a Close that arrives while another is in flight (the post-bridge-loss close
// and Serve's own closeProduct) waits for it, so the worker never reports gone while the ACP
// child is still exiting (flake-steer-tempdir). The joiner returns nil once the closer is done —
// the closer reports the errors — or an error after closeJoinBound. The caller's ctx is not a
// bound: Serve passes its connection context, already done by then.
func (w *Wrapper) Close(context.Context, kit.SessionCloseRequest) error {
	w.mu.Lock()
	if inFlight := w.closing; inFlight != nil {
		w.mu.Unlock()
		select {
		case <-inFlight:
			return nil
		case <-time.After(closeJoinBound):
			return fmt.Errorf("acp close: an in-flight close is still running after %s", closeJoinBound)
		}
	}
	p, sid, lock := w.process, w.acpSessionID, w.lock
	w.process, w.lock, w.acpSessionID, w.key, w.active = nil, nil, "", "", nil
	done := make(chan struct{})
	w.closing = done
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.closing = nil
		w.mu.Unlock()
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var closeErrors []error
	if p != nil {
		// permissionMode stays live until here: the fake may raise a permission request while
		// handling session/close itself, and it must still see the caller's actual mode, not a
		// premature reset to "". Any request still pending once Close reaches the process is
		// answered "cancelled" by Client.closePending, independent of Decide.
		if err := p.Call(ctx, "session/close", map[string]string{"sessionId": sid}, nil); err != nil && !strings.Contains(err.Error(), "not found") {
			closeErrors = append(closeErrors, fmt.Errorf("session/close: %w", err))
		}
		if err := p.Close(ctx); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("process: %w", err))
		}
	}
	w.mu.Lock()
	w.permissionMode, w.reasoningEffort = "", ""
	w.mu.Unlock()
	if lock != nil {
		if err := lock.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("lock: %w", err))
		}
	}
	if len(closeErrors) == 0 {
		return nil
	}
	message := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, errors.Join(closeErrors...).Error())
	return fmt.Errorf("acp close: %s", acp.SanitizeGatewayText(message))
}
