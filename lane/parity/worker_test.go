// Package parity drives the openclaw-peer worker through a private sessionbus daemon at the wire
// `sessionbus --socket <tmp>/p.sock --table <tmp>/table --host lanetest` with the built
// openclaw-peer and the hermetic fake as openclaw on PATH, driven only by `sessionbus-call` against that socket. The shared
// daemon, its socket and anything on another host are never touched.
package parity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imortkz/openclaw-peer/lane/internal/fakebridge"
)

var (
	binDir        string // openclaw-peer + fakeacp, built once
	sessionbusBin string
	callBin       string
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("/tmp", "ocpb.")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	binDir = filepath.Join(dir, "bin")
	for pkg, out := range map[string]string{"../cmd/openclaw-peer": "openclaw-peer", "../internal/fakeacp": "fakeacp"} {
		cmd := exec.Command("go", "build", "-mod=vendor", "-o", filepath.Join(binDir, out), pkg)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n", pkg, err)
			return 1
		}
	}
	if err := os.Symlink(filepath.Join(binDir, "fakeacp"), filepath.Join(binDir, "openclaw")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Resolved here; a missing binary fails every test by name (never a skip).
	sessionbusBin, _ = exec.LookPath("sessionbus")
	callBin, _ = exec.LookPath("sessionbus-call")
	return m.Run()
}

func needBinaries(t *testing.T) {
	t.Helper()
	var missing []string
	if sessionbusBin == "" {
		missing = append(missing, "sessionbus")
	}
	if callBin == "" {
		missing = append(missing, "sessionbus-call")
	}
	if len(missing) > 0 {
		t.Fatalf("parity needs %s on PATH (not found)", strings.Join(missing, " and "))
	}
}

// ---------------------------------------------------------------- private daemon

type bus struct {
	t      *testing.T
	dir    string
	sock   string
	log    string
	script string
	roots  map[string]string
	env    []string
	bridge *fakebridge.Server
	daemon *exec.Cmd
	out    *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// newBus starts a private daemon whose environment carries the lane and fake configuration
// (the daemon passes its environment to every worker it launches).
func newBus(t *testing.T) *bus {
	t.Helper()
	needBinaries(t)
	dir, err := os.MkdirTemp("/tmp", "ocp.") // Darwin socket paths: ≤103 bytes
	if err != nil {
		t.Fatal(err)
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	b := &bus{t: t, dir: dir, sock: filepath.Join(dir, "p.sock"), log: filepath.Join(dir, "fake.log"),
		script: filepath.Join(dir, "script.json"), roots: map[string]string{}, out: &lockedBuffer{}}
	agents := []map[string]string{}
	for _, a := range [][2]string{{"a", "Alpha role"}, {"b", "Beta role"}} {
		root := filepath.Join(dir, "ws", a[0])
		b.must(os.MkdirAll(filepath.Join(root, "sub"), 0o700))
		b.roots[a[0]] = root
		agents = append(agents, map[string]string{"id": a[0], "workspace": root, "description": a[1]})
	}
	b.setScript(`{}`)
	// The plugin's side of the worker bridge: serves the lane settings and accepts lane.register / lane.steer.
	bridge := fakebridge.Start(t, fakebridge.Options{Settings: map[string]any{"agents": agents, "credentialDir": filepath.Join(dir, "cli")}})
	b.bridge = bridge
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SESSIONBUS_") && !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "OPENCLAW_") && !strings.HasPrefix(kv, "FAKEACP_") {
			b.env = append(b.env, kv)
		}
	}
	b.env = append(b.env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_STATE_HOME="+filepath.Join(dir, "xdg"),
		"SESSIONBUS_OPENCLAW_BRIDGE="+bridge.Endpoint,
		"FAKEACP_LOG="+b.log,
		"FAKEACP_STATE="+filepath.Join(dir, "state"),
		"FAKEACP_SCRIPT="+b.script,
	)
	b.daemon = exec.Command(sessionbusBin, "--socket", b.sock, "--table", filepath.Join(dir, "table"), "--host", "lanetest")
	b.daemon.Env = b.env
	b.daemon.Stdout, b.daemon.Stderr = b.out, b.out
	b.must(b.daemon.Start())
	t.Cleanup(func() {
		_ = b.daemon.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = b.daemon.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = b.daemon.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("daemon output:\n%s", b.out.String())
		}
		_ = os.RemoveAll(dir)
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(b.sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("private daemon did not create %s: %s", b.sock, b.out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return b
}

func (b *bus) must(err error) {
	b.t.Helper()
	if err != nil {
		b.t.Fatal(err)
	}
}

func (b *bus) setScript(s string) { b.must(os.WriteFile(b.script, []byte(s), 0o600)) }

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func (e *rpcError) String() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%d %s %s %s", e.Code, e.Message, e.Data, e.Error)
}

// call runs one `sessionbus-call -g lanetest <method> <params>` against the private socket.
func (b *bus) call(method string, params any) (json.RawMessage, *rpcError) {
	b.t.Helper()
	raw, _ := json.Marshal(params)
	cmd := exec.Command(callBin, "-timeout", "30s", "-g", "lanetest", "-socket", b.sock, method, string(raw))
	cmd.Env = append(b.env, "SESSIONBUS_SOCKET="+b.sock)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		b.t.Fatal(err)
	}
	out := bytes.TrimSpace(so.Bytes())
	if err != nil {
		e := &rpcError{}
		if json.Unmarshal(out, e) != nil {
			e.Error = string(out) + " " + se.String()
		}
		return nil, e
	}
	return out, nil
}

func (b *bus) mustCall(method string, params any, into any) {
	b.t.Helper()
	out, e := b.call(method, params)
	if e != nil {
		b.t.Fatalf("%s %v: %s", method, params, e)
	}
	if into != nil {
		b.must(json.Unmarshal(out, into))
	}
}

type spawnResult struct {
	SessionID string `json:"session_id"`
}

type turnResult struct {
	Outcome string `json:"outcome"`
	Result  string `json:"result"`
}

type runStatus struct {
	SessionID string      `json:"session_id"`
	RunID     string      `json:"run_id"`
	State     string      `json:"state"`
	Reason    string      `json:"reason"`
	Result    *turnResult `json:"result"`
}

func (b *bus) spawn(open map[string]any) (string, *rpcError) {
	b.t.Helper()
	out, e := b.call("lane.spawn", map[string]any{"product": "openclaw-peer", "name": fmt.Sprintf("lane%d", time.Now().UnixNano()%1e6),
		"persistent": true, "notify": false, "extra_groups": []string{"lanetest"}, "open": open})
	if e != nil {
		return "", e
	}
	var r spawnResult
	b.must(json.Unmarshal(out, &r))
	return r.SessionID, nil
}

func (b *bus) mustSpawn(open map[string]any) string {
	b.t.Helper()
	id, e := b.spawn(open)
	if e != nil {
		b.t.Fatalf("spawn %v: %s", open, e)
	}
	return id
}

func (b *bus) runText(lane, text string) runStatus {
	b.t.Helper()
	var st runStatus
	b.mustCall("turn.run", map[string]any{"session_id": lane, "input": text}, &st)
	return st
}

func completed(t *testing.T, st runStatus) string {
	t.Helper()
	if st.State != "done" || st.Result == nil || st.Result.Outcome != "completed" {
		t.Fatalf("run %s: state=%s reason=%q result=%+v, want completed", st.RunID, st.State, st.Reason, st.Result)
	}
	return st.Result.Result
}

type logRec map[string]any

func (b *bus) events(name string) []logRec {
	b.t.Helper()
	raw, err := os.ReadFile(b.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		b.t.Fatal(err)
	}
	var out []logRec
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r logRec
		b.must(json.Unmarshal(line, &r))
		if r["event"] == name {
			out = append(out, r)
		}
	}
	return out
}

// callerEvents are the prompt/prompt_done events of caller turns (the worker sends no prompts of its own).
func (b *bus) callerEvents(name string) []logRec {
	return b.events(name)
}

func (b *bus) gatewayCalls(method string) int {
	n := 0
	for _, r := range b.events("gateway_call") {
		if r["method"] == method {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var turnRe = regexp.MustCompile(`turn=(\d+)`)

func turnOf(t *testing.T, s string) int {
	t.Helper()
	m := turnRe.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no turn counter in %q", s)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ---------------------------------------------------------------- tests

// Describe shows the five honoured fields and the --agent argument naming both roles.
func TestParityDescribe(t *testing.T) {
	b := newBus(t)
	var d struct {
		SupportsMessageRun bool     `json:"supports_message_run"`
		Product            string   `json:"product"`
		Fields             []string `json:"supported_open_fields"`
		Extra              []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			TakesValue  bool   `json:"takes_value"`
		} `json:"extra_arguments"`
	}
	b.mustCall("lane.describe", map[string]any{"product": "openclaw-peer"}, &d)
	if d.Product != "openclaw-peer" || !d.SupportsMessageRun || strings.Join(d.Fields, ",") != "cwd,permission_mode,model,reasoning_effort,arguments" {
		t.Fatalf("describe %+v", d)
	}
	if len(d.Extra) != 1 || d.Extra[0].Name != "--agent" || !d.Extra[0].TakesValue ||
		!strings.Contains(d.Extra[0].Description, "a — Alpha role") || !strings.Contains(d.Extra[0].Description, "b — Beta role") {
		t.Fatalf("extra_arguments %+v", d.Extra)
	}
}

// At the wire, an agent is required with two agents; unknown agents and unsupported values
// refused as spawn_failed with the reason in stderr_tail, and no child started.
func TestParityAgentChoiceAndRefusals(t *testing.T) {
	b := newBus(t)
	for _, tc := range []struct {
		open  map[string]any
		wants []string
	}{
		{map[string]any{}, []string{"agent required", "a — Alpha role", "b — Beta role"}},
		{map[string]any{"arguments": []string{"--agent", "zzz"}}, []string{"unsupported value arguments=--agent zzz"}},
		{map[string]any{"arguments": []string{"--model", "x"}}, []string{"unsupported value arguments=--model x"}},
		{map[string]any{"arguments": []string{"--agent", "a"}, "permission_mode": "acceptEdits"}, []string{"unsupported value permission_mode=acceptEdits"}},
		{map[string]any{"arguments": []string{"--agent", "a"}, "cwd": b.roots["b"]}, []string{"outside the workspace of agent a"}},
		{map[string]any{"arguments": []string{"--agent", "a"}, "cwd": "sub"}, []string{"(must be absolute)"}},
		// Invalid fields are refused at the wire too, before any Gateway call.
		{map[string]any{"arguments": []string{"--agent", "a"}, "model": "/model"}, []string{"unsupported value model=/model"}},
		{map[string]any{"arguments": []string{"--agent", "a"}, "reasoning_effort": "turbo"}, []string{"unsupported value reasoning_effort=turbo"}},
	} {
		_, e := b.spawn(tc.open)
		if e == nil {
			t.Fatalf("spawn %v succeeded", tc.open)
		}
		for _, w := range tc.wants {
			if !strings.Contains(e.String(), w) || !strings.Contains(e.String(), "stderr_tail") {
				t.Fatalf("spawn %v: %s, want spawn_failed stderr_tail containing %q", tc.open, e, w)
			}
		}
	}
	if n := len(b.events("start")); n != 0 {
		t.Fatalf("refused spawns started %d ACP child(ren)", n)
	}
	if g := b.events("gateway_call"); len(g) != 0 {
		t.Fatalf("refused spawns made gateway calls: %v", g)
	}
}

// The admin guard at the wire: an admin Gateway credential refuses the spawn as spawn_failed with
// the guard text in stderr_tail — one exec.approvals.get probe, no session Gateway call, no ACP
// child; with the default (non-admin) credential the next spawn opens.
func TestParityAdminCredentialRefused(t *testing.T) {
	b := newBus(t)
	b.setScript(`{"admin":true}`)
	_, e := b.spawn(map[string]any{"arguments": []string{"--agent", "a"}, "cwd": filepath.Join(b.roots["a"], "sub"), "model": "mock/mock-b"})
	if e == nil || !strings.Contains(e.String(), "stderr_tail") ||
		!strings.Contains(e.String(), "lane credential has operator.admin; pair a read/write/approvals device (lane/README.md) and point the lane at its state dir") {
		t.Fatalf("admin spawn: %s, want spawn_failed stderr_tail with the admin guard text", e)
	}
	if n := len(b.events("start")); n != 0 {
		t.Fatalf("refused spawn started %d ACP child(ren)", n)
	}
	if g := b.events("gateway_call"); len(g) != 1 || g[0]["method"] != "exec.approvals.get" {
		t.Fatalf("refused spawn gateway calls %v, want exactly the exec.approvals.get probe", g)
	}
	b.setScript(`{}`)
	b.mustSpawn(map[string]any{"arguments": []string{"--agent", "a"}})
	if n := b.gatewayCalls("exec.approvals.get"); n != 2 {
		t.Fatalf("probes %d, want one per spawn", n)
	}
}

// At the wire: spawn --agent b with a cwd inside b's root, run, interrupt, run
// again, close, resume (the daemon replays the cwd; exactly one sessions.create), history kept.
func TestParityLifecycle(t *testing.T) {
	b := newBus(t)
	sub := filepath.Join(b.roots["b"], "sub")
	lane := b.mustSpawn(map[string]any{"arguments": []string{"--agent", "b"}, "cwd": sub})
	if !regexp.MustCompile(`^agent:b:sb-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}@lanetest$`).MatchString(lane) {
		t.Fatalf("lane id %q", lane)
	}
	if got := completed(t, b.runText(lane, "EXEC:pwd")); got != sub {
		t.Fatalf("exec pwd %q, want %s", got, sub)
	}
	first := turnOf(t, completed(t, b.runText(lane, "remember PAPAYA")))

	var ref struct {
		RunID string `json:"run_id"`
	}
	b.mustCall("turn.start", map[string]any{"session_id": lane, "input": "please SLOW down"}, &ref)
	eventually(t, "the SLOW prompt", func() bool {
		for _, p := range b.events("prompt") {
			if p["text"] == "please SLOW down" {
				return true
			}
		}
		return false
	})
	b.mustCall("turn.interrupt", map[string]any{"session_id": lane}, nil)
	var st runStatus
	b.mustCall("turn.wait", map[string]any{"session_id": lane, "run_id": ref.RunID, "timeout_ms": 10000}, &st)
	if st.State != "done" || st.Result == nil || st.Result.Outcome != "interrupted" {
		t.Fatalf("interrupted run %+v", st)
	}
	completed(t, b.runText(lane, "after the interrupt"))

	b.mustCall("session.close", map[string]any{"session_id": lane}, nil)
	eventually(t, "the ACP child to exit", func() bool { return len(b.events("stdin_closed")) == 1 })

	var res spawnResult
	b.mustCall("lane.spawn", map[string]any{"resume_session_id": lane, "persistent": true, "notify": false}, &res)
	if res.SessionID != lane {
		t.Fatalf("resume returned %q, want exactly %q", res.SessionID, lane)
	}
	if got := completed(t, b.runText(lane, "EXEC:pwd")); got != sub {
		t.Fatalf("exec pwd after resume %q, want %s (replayed cwd)", got, sub)
	}
	if later := turnOf(t, completed(t, b.runText(lane, "which word"))); later <= first {
		t.Fatalf("resumed lane lost history: turn %d after %d", later, first)
	}
	if n := b.gatewayCalls("sessions.create"); n != 1 {
		t.Fatalf("sessions.create called %d times over create+resume, want exactly 1", n)
	}
	if n := b.gatewayCalls("sessions.list"); n != 1 {
		t.Fatalf("resume drift check: %d sessions.list calls, want 1", n)
	}
	b.mustCall("session.close", map[string]any{"session_id": lane}, nil)
}

// At the wire, a bus message to an idle lane is admitted (injected) and processed by a
// wake run that sees the rendered envelope. Ten sends in a row, so a receipt that races the
// SDK fallback ("rejected/no_receipt" at the wire) cannot pass by luck.
func TestParityWakeRun(t *testing.T) {
	b := newBus(t)
	lane := b.mustSpawn(map[string]any{"arguments": []string{"--agent", "a"}})
	completed(t, b.runText(lane, "hello"))
	var bad []string
	for i := 1; i <= 10; i++ {
		body := fmt.Sprintf("wake up please %d", i)
		var sent struct {
			Deliveries []struct {
				Disposition string `json:"disposition"`
				Reason      string `json:"reason"`
			} `json:"deliveries"`
		}
		b.mustCall("message.send", map[string]any{"target": lane, "message": body}, &sent)
		eventually(t, "wake run "+strconv.Itoa(i), func() bool {
			return len(b.callerEvents("prompt")) == i+1 && len(b.callerEvents("prompt_done")) == i+1
		})
		prompts := b.callerEvents("prompt")
		if text, _ := prompts[len(prompts)-1]["text"].(string); !strings.Contains(text, "<cross-session-message ") || !strings.Contains(text, body) {
			t.Fatalf("wake %d prompt %q, want the rendered envelope with %q", i, text, body)
		}
		if len(sent.Deliveries) != 1 || sent.Deliveries[0].Disposition != "injected" {
			bad = append(bad, fmt.Sprintf("wake %d: %+v", i, sent.Deliveries))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("%d/10 wake deliveries not injected: %s", len(bad), strings.Join(bad, "; "))
	}
}

// A tokenless start of the worker binary explains itself and exits 2.
func TestParityTokenlessExit(t *testing.T) {
	cmd := exec.Command(filepath.Join(binDir, "openclaw-peer"))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SESSIONBUS_LAUNCH_TOKEN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var se bytes.Buffer
	cmd.Stderr = &se
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(se.String(), "is the Sessionbus lane worker") {
		t.Fatalf("tokenless start: %v, stderr %q", err, se.String())
	}
}
