package lane_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	kit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/bus/sdk/go/protocol"
	"github.com/sessionbus/peer-common/host"
	"github.com/sessionbus/peer-common/testsocket"

	lane "github.com/imortkz/openclaw-peer/lane"
	"github.com/imortkz/openclaw-peer/lane/internal/fakebridge"
)

// Contract tests for the lane contract, one per truth or mitigation.
// Public surface only: the wire of the kit worker socket and the fake's log.

func (e *testEnv) since(mark int, name string) []logRec {
	all := e.events("")
	if mark > len(all) {
		return nil
	}
	var out []logRec
	for _, r := range all[mark:] {
		if name == "" || r["event"] == name {
			out = append(out, r)
		}
	}
	return out
}

func (e *testEnv) mark() int { return len(e.events("")) }

func gatewaySince(e *testEnv, mark int, method string) []logRec {
	var out []logRec
	for _, r := range e.since(mark, "gateway_call") {
		if r["method"] == method {
			out = append(out, r)
		}
	}
	return out
}

func recvSince(e *testEnv, mark int, method string) []map[string]any {
	var out []map[string]any
	for _, r := range e.since(mark, "recv") {
		if r["method"] == method {
			p, _ := r["params"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

func params(r logRec) map[string]any { p, _ := r["params"].(map[string]any); return p }

func mustContain(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("%s = %q, want it to contain %q", what, got, w)
		}
	}
}

// acpSessionID is the id the fake returned for session/new: the fake logs it.
func acpSessionID(t *testing.T, e *testEnv, key string) string {
	t.Helper()
	for _, r := range e.events("session_new") {
		if r["key"] == key {
			return str(r["sessionId"])
		}
	}
	t.Fatalf("no session/new for %s", key)
	return ""
}

var turnRe = regexp.MustCompile(`turn=(\d+)`)

func turnOf(t *testing.T, text string) int {
	t.Helper()
	m := turnRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no turn counter in %q", text)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ---------------------------------------------------------------- Hello and agent choice

func TestHelloOpenFields(t *testing.T) {
	h := newEnv(t).start()
	got := append([]string(nil), h.hello.SupportedOpenFields...)
	sort.Strings(got)
	if strings.Join(got, ",") != "arguments,cwd,model,permission_mode,reasoning_effort" {
		t.Fatalf("supported_open_fields = %v", h.hello.SupportedOpenFields)
	}
	if h.hello.Product != "openclaw-peer" || !h.hello.SupportsMessageRun {
		t.Fatalf("hello = %+v", h.hello)
	}
	if len(h.hello.ExtraArguments) != 1 || h.hello.ExtraArguments[0].Name != "--agent" || !h.hello.ExtraArguments[0].TakesValue {
		t.Fatalf("extra_arguments = %+v", h.hello.ExtraArguments)
	}
}

func TestHelloDescribesAgents(t *testing.T) {
	h := newEnv(t).start()
	if d := h.hello.ExtraArguments[0].Description; d != "OpenClaw agent for this lane (default: lane — Lane role)" {
		t.Fatalf("single-agent description = %q", d)
	}
	h2 := newEnv(t, "a=Alpha role", "b=Beta role").start()
	d := h2.hello.ExtraArguments[0].Description
	if !strings.HasPrefix(d, "OpenClaw agent for this lane, required: ") {
		t.Fatalf("multi-agent description = %q", d)
	}
	mustContain(t, "multi-agent description", d, "a — Alpha role", "b — Beta role", "; ")
}

func TestHelloDescribesAgentModels(t *testing.T) {
	var agents []lane.Agent
	entries := make([]string, 23)
	for i := range entries {
		entries[i] = fmt.Sprintf("mock/model-%02d", i)
	}
	raw, err := json.Marshal([]map[string]any{
		{"id": "a", "description": "Alpha", "models": map[string]any{"allow": []string{"openrouter/*", "glm"}, "default": "zai/glm-5.3"}},
		{"id": "b", "description": "Beta", "models": map[string]any{"allow": []string{}}},
		{"id": "c", "description": "Cap", "models": map[string]any{"allow": entries}},
		{"id": "d", "description": "Legacy"},
		{"id": "e", "description": "Missing allow", "models": map[string]any{"default": "mock/a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &agents); err != nil {
		t.Fatal(err)
	}
	hello, err := lane.New(lane.Config{Agents: agents}, nil).Hello(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "OpenClaw agent for this lane, required: a — Alpha [models: openrouter/*, glm; default: zai/glm-5.3]; b — Beta [models: any]; c — Cap [models: " + strings.Join(entries[:20], ", ") + ", … +3 more]; d — Legacy; e — Missing allow"
	if got := hello.ExtraArguments[0].Description; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func TestOpenModelRefusalListsAllowed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow []string
		def   string
	}{
		{"full list", strings.Split("mock/a,mock/b,mock/c,mock/d,mock/e,mock/f,mock/g,mock/h,mock/i,mock/j,mock/k,mock/l,mock/m,mock/n,mock/o,mock/p,mock/q,mock/r,mock/s,mock/t,glm,openrouter/*", ","), "mock/a"},
		{"any without default", []string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			raw, err := json.Marshal(map[string]any{"id": e.agents[0].ID, "workspace": e.agents[0].Workspace, "description": e.agents[0].Description, "models": map[string]any{"allow": tc.allow, "default": tc.def}})
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &e.agents[0]); err != nil {
				t.Fatal(err)
			}
			e.script = `{"gateway":{"sessions.patch":{"fail":"unknown_model"}}}`
			_, rpcErr := e.start().Open(kit.OpenOptions{Model: "bad/model"})
			allow := strings.Join(tc.allow, ", ")
			if allow == "" {
				allow = "any"
			}
			suffix := "; allowed for lane: " + allow
			if tc.def != "" {
				suffix += "; default: " + tc.def
			}
			mustContain(t, "open error", rpcText(rpcErr), "unsupported value model=bad/model (refused by OpenClaw: ", "model not allowed", suffix+")")
			if e.acpStarts() != 0 {
				t.Fatal("ACP started after refused model")
			}
		})
	}
}

func TestOpenFreshMintsAgentKey(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	if !mintedKey("lane").MatchString(key) {
		t.Fatalf("lane id %q, want agent:lane:sb-<uuid4>", key)
	}
	news := e.recv("session/new")
	if len(news) != 1 {
		t.Fatalf("session/new frames %v", news)
	}
	p := news[0]
	servers, ok := p["mcpServers"].([]any)
	m, _ := p["_meta"].(map[string]any)
	if !ok || len(servers) != 0 || m["sessionKey"] != key {
		t.Fatalf("session/new params %v", p)
	}
	if _, has := m["sessionLabel"]; has {
		t.Fatalf("session/new carries _meta.sessionLabel (D23a): %v", m)
	}
}

// Fresh and resume go through the same attach: exactly one lane.register per Open, carrying the
// key, the agent and the ACP session id.
func TestOpenRegistersLaneOnce(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	reg := func() []fakebridge.Event {
		var out []fakebridge.Event
		for _, ev := range e.bridge.Log() {
			if ev.Method == "lane.register" {
				out = append(out, ev)
			}
		}
		return out
	}
	r := reg()
	if len(r) != 1 || r[0].Params["sessionKey"] != key || r[0].Params["agentId"] != "lane" || r[0].Params["acpSessionId"] == "" {
		t.Fatalf("fresh register: %+v", r)
	}
	completed(t, h.Execute("one turn")) // the Gateway row exists after the first prompt
	h.Close()
	e.start().mustResume(key, kit.OpenOptions{})
	r = reg()
	if len(r) != 2 || r[1].Params["sessionKey"] != key || r[1].Params["acpSessionId"] != key {
		t.Fatalf("resume register: %+v", r)
	}
}

func TestOpenFailsWhenRegisterRefused(t *testing.T) {
	e := newEnv(t)
	e.bridge = fakebridge.Start(t, fakebridge.Options{
		Settings:   map[string]any{"agents": e.agents, "credentialDir": filepath.Join(e.dir, "cli")},
		OnRegister: func(string, string, string) string { return "key_in_use" },
	})
	_, err := e.start().Open(kit.OpenOptions{})
	mustContain(t, "open error", rpcText(err), "key_in_use")
}

func TestAgentDefaultSingle(t *testing.T) {
	for _, args := range [][]string{nil, {"--agent", "lane"}, {"--agent=lane"}} {
		e := newEnv(t)
		key := e.start().mustOpen(kit.OpenOptions{Arguments: args})
		if !mintedKey("lane").MatchString(key) {
			t.Fatalf("arguments %q: key %q", args, key)
		}
	}
}

func TestAgentRequiredWhenMany(t *testing.T) {
	e := newEnv(t, "a=Alpha role", "b=Beta role")
	_, err := e.start().Open(kit.OpenOptions{})
	mustContain(t, "open error", rpcText(err), "agent required", "a — Alpha role", "b — Beta role")
	e.noNativeWork(t)
	// Choosing one opens that agent.
	e2 := newEnv(t, "a=Alpha role", "b=Beta role")
	if key := e2.start().mustOpen(kit.OpenOptions{Arguments: []string{"--agent", "b"}}); !mintedKey("b").MatchString(key) {
		t.Fatalf("--agent b: key %q", key)
	}
}

func TestAgentUnknownRejected(t *testing.T) {
	e := newEnv(t, "a=Alpha role", "b=Beta role")
	_, err := e.start().Open(kit.OpenOptions{Arguments: []string{"--agent", "zzz"}})
	mustContain(t, "open error", rpcText(err), "unsupported value arguments=--agent zzz", "allowed:")
	e.noNativeWork(t)
}

func TestArgumentsOnlyAgent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", "x"}, "unsupported value arguments=--model x"},
		{[]string{"--agent", "lane", "--agent", "lane"}, "unsupported value arguments=--agent lane --agent lane"},
		{[]string{"--agent", "lane", "--verbose"}, "unsupported value arguments=--agent lane --verbose"},
	} {
		e := newEnv(t)
		_, err := e.start().Open(kit.OpenOptions{Arguments: tc.args})
		mustContain(t, "open error", rpcText(err), tc.want)
		e.noNativeWork(t)
	}
}

// ---------------------------------------------------------------- run, interrupt, close

func TestRunCompleted(t *testing.T) {
	e := newEnv(t)
	e.script = `{"chunks":["Hello",", world"],"extraUpdates":true}`
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	st := h.Execute("hi")
	if got := completed(t, st); got != "Hello, world" {
		t.Fatalf("result %q: thought, tool and foreign-session updates must be excluded", got)
	}
	ps := e.callerPrompts()
	if len(ps) != 1 || ps[0]["text"] != "hi" || ps[0]["blocks"] != float64(1) {
		t.Fatalf("caller prompts %v", ps)
	}
	if ps[0]["sessionId"] != acpSessionID(t, e, key) {
		t.Fatalf("prompt sessionId %v, want the ACP id from session/new", ps[0]["sessionId"])
	}
	// A max_tokens stop reason makes the run fail.
	e2 := newEnv(t)
	e2.script = `{"stopReason":"max_tokens"}`
	h2 := e2.start()
	h2.mustOpen(kit.OpenOptions{})
	if st := h2.Execute("hi"); outcome(st) != "failed" {
		t.Fatalf("max_tokens → %s, want failed", outcome(st))
	}
}

func TestInterruptThenRun(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	sid := acpSessionID(t, e, key)
	run := h.Start("please SLOW down")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
	if err := h.Interrupt(); err != nil {
		t.Fatalf("interrupt: %s", rpcText(err))
	}
	if st := h.Wait(run); outcome(st) != "interrupted" {
		t.Fatalf("interrupted run → %s", outcome(st))
	}
	cancels := e.recv("session/cancel")
	if len(cancels) != 1 || cancels[0]["sessionId"] != sid {
		t.Fatalf("session/cancel frames %v, want exactly one for %s", cancels, sid)
	}
	mustContain(t, "run after interrupt", completed(t, h.Execute("after")), "after")
	// An interrupt that lands right after turn.execute (before or as the prompt is sent).
	run = h.Start("SLOW again")
	if err := h.Interrupt(); err != nil {
		t.Fatalf("early interrupt: %s", rpcText(err))
	}
	if st := h.Wait(run); outcome(st) != "interrupted" {
		t.Fatalf("pre-interrupted run → %s", outcome(st))
	}
	mustContain(t, "run after early interrupt", completed(t, h.Execute("later")), "later")
}

func TestCloseReleases(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	sid := acpSessionID(t, e, key)
	completed(t, h.Execute("hi"))
	if h.lockFree(key) {
		t.Fatal("lane lock is free while the lane is open")
	}
	h.Close()
	closes := e.recv("session/close")
	if len(closes) != 1 || closes[0]["sessionId"] != sid {
		t.Fatalf("session/close frames %v", closes)
	}
	eventually(t, "the ACP child to see stdin closed", func() bool { return len(e.events("stdin_closed")) == 1 })
	if !h.lockFree(key) {
		t.Fatal("lane lock still held after close")
	}
}

func TestCloseReportsACPCleanupFailure(t *testing.T) {
	e := newEnv(t)
	e.script = `{"fail":{"session/close":"close exploded"}}`
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	if err := h.wrapper.Close(context.Background(), kit.SessionCloseRequest{}); err == nil || !strings.Contains(err.Error(), "close exploded") {
		t.Fatalf("Close error = %v, want ACP failure cause", err)
	} else if strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("Close error spans lines: %q", err)
	}
	if !h.lockFree(key) {
		t.Fatal("lane lock still held after failed ACP close")
	}
	eventually(t, "the ACP child to see stdin closed", func() bool { return len(e.events("stdin_closed")) == 1 })
	h.Close()
}

// ---------------------------------------------------------------- resume

func TestResumeExact(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	first := turnOf(t, completed(t, h.Execute("remember PAPAYA")))
	h.Close()

	mark := e.mark()
	h2 := e.start()
	h2.mustResume(key, kit.OpenOptions{})
	rs := recvSince(e, mark, "session/resume")
	if len(rs) != 1 || rs[0]["sessionId"] != key || rs[0]["cwd"] != e.root("lane") {
		t.Fatalf("session/resume frames %v, want sessionId=%s cwd=%s", rs, key, e.root("lane"))
	}
	if servers, ok := rs[0]["mcpServers"].([]any); !ok || len(servers) != 0 {
		t.Fatalf("session/resume mcpServers %v", rs[0]["mcpServers"])
	}
	if n := len(recvSince(e, mark, "session/new")); n != 0 {
		t.Fatalf("resume sent %d session/new", n)
	}
	if g := gatewaySince(e, mark, "sessions.create"); len(g) != 0 {
		t.Fatalf("resume called sessions.create: %v", g)
	}
	if g := gatewaySince(e, mark, "sessions.patch"); len(g) != 0 {
		t.Fatalf("resume called sessions.patch: %v", g)
	}
	text := completed(t, h2.Execute("which word"))
	if turnOf(t, text) <= first {
		t.Fatalf("resumed lane lost history: %q after turn %d", text, first)
	}
	ps := e.since(mark, "prompt")
	if last := ps[len(ps)-1]; last["sessionId"] != key {
		t.Fatalf("prompt after resume uses sessionId %v, want the key", last["sessionId"])
	}
}

func TestResumeRejectsForeignKey(t *testing.T) {
	ghost := "agent:ghost:sb-" + newUUID4(t)
	for _, key := range []string{"agent:advisor:main", "agent:lane:main", ghost, "agent:lane:sb-not-a-uuid"} {
		e := newEnv(t)
		_, err := e.start().Resume(key, kit.OpenOptions{})
		mustContain(t, "resume "+key, rpcText(err), "resume key not admitted")
		e.noNativeWork(t)
	}
}

func TestResumeAgentFromKey(t *testing.T) {
	e := newEnv(t, "a=Alpha role", "b=Beta role")
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{Arguments: []string{"--agent", "b"}})
	completed(t, h.Execute("hi"))
	h.Close()

	mark := e.mark()
	if _, err := e.start().Resume(key, kit.OpenOptions{Arguments: []string{"--agent", "a"}}); err == nil {
		t.Fatal("resume with a mismatched --agent was admitted")
	}
	if n := len(e.since(mark, "start")); n != 0 {
		t.Fatalf("mismatched --agent started %d ACP child(ren)", n)
	}
	e.start().mustResume(key, kit.OpenOptions{Arguments: []string{"--agent", "b"}})
	// With several agents and no --agent, the agent comes from the key.
	e.start().mustResume(key, kit.OpenOptions{})
}

func TestResumeReplayedCwd(t *testing.T) {
	e := newEnv(t)
	root := e.root("lane")
	sub := e.mkdir(root, "sub")
	link := filepath.Join(root, "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{Cwd: link})
	completed(t, h.Execute("hi"))
	h.Close()

	mark := e.mark()
	h2 := e.start()
	h2.mustResume(key, kit.OpenOptions{Cwd: link}) // the daemon replays the original open
	if g := gatewaySince(e, mark, "sessions.list"); len(g) != 1 {
		t.Fatalf("resume drift check: %d sessions.list calls, want exactly one", len(g))
	}
	if g := gatewaySince(e, mark, "sessions.create"); len(g) != 0 {
		t.Fatalf("resume re-created the session: %v", g)
	}
	if g := gatewaySince(e, mark, "sessions.get"); len(g) != 0 {
		t.Fatalf("resume used sessions.get (sessions.list): %v", g)
	}
	rs := recvSince(e, mark, "session/resume")
	if len(rs) != 1 || rs[0]["cwd"] != sub {
		t.Fatalf("session/resume frames %v, want cwd=%s (the realpath)", rs, sub)
	}
	if got := completed(t, h2.Execute("EXEC:pwd")); got != sub {
		t.Fatalf("pwd after resume %q, want %s", got, sub)
	}
	for _, p := range e.callerPrompts() {
		if meta(p)["prefixCwd"] == false {
			t.Fatalf("caller prompt disables the cwd prefix: %v", p)
		}
	}
	if g := e.gatewayCalls("sessions.create"); len(g) != 1 || params(g[0])["cwd"] != sub {
		t.Fatalf("sessions.create over the lane's life %v, want one with cwd=%s", g, sub)
	}
}

func TestResumeCwdDrift(t *testing.T) {
	open := func(t *testing.T, e *testEnv) (string, string) {
		sub := e.mkdir(e.root("lane"), "sub")
		h := e.start()
		key := h.mustOpen(kit.OpenOptions{Cwd: sub})
		completed(t, h.Execute("hi"))
		h.Close()
		return key, sub
	}
	t.Run("declared root changed", func(t *testing.T) {
		e := newEnv(t)
		key, sub := open(t, e)
		e.agents[0].Workspace = e.mkdir(e.dir, "ws", "moved")
		mark := e.mark()
		_, err := e.start().Resume(key, kit.OpenOptions{Cwd: sub})
		mustContain(t, "resume error", rpcText(err), "unsupported value cwd="+sub, "outside the workspace of agent lane")
		if n := len(e.since(mark, "start")); n != 0 {
			t.Fatalf("ACP started %d time(s)", n)
		}
	})
	t.Run("spawnedCwd changed", func(t *testing.T) {
		e := newEnv(t)
		key, sub := open(t, e)
		e.script = `{"spawnedCwd":{"*":"/elsewhere"}}`
		mark := e.mark()
		_, err := e.start().Resume(key, kit.OpenOptions{Cwd: sub})
		mustContain(t, "resume error", rpcText(err), "unsupported value cwd="+sub, "lane cwd changed since creation", "/elsewhere")
		if n := len(e.since(mark, "start")); n != 0 {
			t.Fatalf("ACP started %d time(s)", n)
		}
	})
	t.Run("row missing", func(t *testing.T) {
		e := newEnv(t)
		key, sub := open(t, e)
		if err := os.Remove(filepath.Join(e.state, "sessions.json")); err != nil {
			t.Fatal(err)
		}
		mark := e.mark()
		_, err := e.start().Resume(key, kit.OpenOptions{Cwd: sub})
		mustContain(t, "resume error", rpcText(err), "unsupported value cwd="+sub, "session not found")
		if n := len(e.since(mark, "start")); n != 0 {
			t.Fatalf("ACP started %d time(s)", n)
		}
	})
}

// ---------------------------------------------------------------- model and thinking

func TestModelViaSessionsPatch(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{Model: "mock/mock-b"})
	patches := e.gatewayCalls("sessions.patch")
	if len(patches) != 1 || params(patches[0])["key"] != key || params(patches[0])["model"] != "mock/mock-b" {
		t.Fatalf("sessions.patch calls %v", patches)
	}
	patchAt := e.index(func(r logRec) bool { return r["event"] == "gateway_call" && r["method"] == "sessions.patch" })
	acpAt := e.index(func(r logRec) bool { return r["event"] == "start" })
	if patchAt < 0 || acpAt < 0 || patchAt > acpAt {
		t.Fatalf("sessions.patch (log #%d) must precede the ACP child (log #%d)", patchAt, acpAt)
	}
	mustContain(t, "reply", completed(t, h.Execute("which model")), "model=mock/mock-b")
	if trips := e.events("tripwire"); len(trips) != 0 {
		t.Fatalf("a prompt carried a live /model directive: %v", trips)
	}
	h.Close()
	mark := e.mark()
	e.start().mustResume(key, kit.OpenOptions{Model: "mock/mock-b"}) // replayed open
	if g := gatewaySince(e, mark, "sessions.patch"); len(g) != 0 {
		t.Fatalf("resume sent a model patch: %v", g)
	}

	e2 := newEnv(t)
	e2.script = `{"gateway":{"sessions.patch":{"fail":"unknown_model"}}}`
	_, err := e2.start().Open(kit.OpenOptions{Model: "bad/model"})
	mustContain(t, "open error", rpcText(err), "unsupported value model=bad/model (refused by OpenClaw: ", "model not allowed")
	if n := e2.acpStarts(); n != 0 {
		t.Fatalf("ACP started after a refused model")
	}

	e3 := newEnv(t)
	_, err = e3.start().Open(kit.OpenOptions{Model: "/model"})
	mustContain(t, "open error", rpcText(err), "unsupported value model=/model")
	e3.noNativeWork(t)
}

func TestModelViaSessionsPatchAlias(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{Model: "glm"})
	patches := e.gatewayCalls("sessions.patch")
	if len(patches) != 1 || params(patches[0])["key"] != key || params(patches[0])["model"] != "glm" {
		t.Fatalf("alias sessions.patch calls %v", patches)
	}
}

// A sessions.patch that fails in transport (Gateway restart, CLI timeout) must fail the
// spawn as spawn_failed "gateway call failed" — never open the lane on the agent's default model,
// and never read as a model refusal. Nothing ACP-side starts, the lock is released, and the
// Gateway child's output (marker) does not reach the caller or the worker's stderr.
func TestModelPatchTransportFailure(t *testing.T) {
	marker := secretMarker(t)
	e := newEnv(t)
	e.script = `{"marker":"` + marker + `","gateway":{"sessions.patch":{"fail":"transport"}}}`
	h := e.startBinary()
	_, err := h.Open(kit.OpenOptions{Model: "mock/mock-b"})
	if err == nil || err.Code != protocol.SpawnFailed {
		t.Fatalf("open error %s, want spawn_failed", rpcText(err))
	}
	text := rpcText(err)
	mustContain(t, "open error", text, "gateway call failed")
	if strings.Contains(text, marker) || strings.Contains(text, "1006") || strings.Contains(text, "unsupported value") {
		t.Fatalf("transport cause not redacted / misclassified: %q", text)
	}
	if n := e.acpStarts(); n != 0 {
		t.Fatalf("ACP started after a failed sessions.patch")
	}
	patches := e.gatewayCalls("sessions.patch")
	if len(patches) != 1 {
		t.Fatalf("sessions.patch calls %v", patches)
	}
	if key := str(params(patches[0])["key"]); !h.lockFree(key) {
		t.Fatalf("lock of %s still held after the failed open", key)
	}
	if strings.Contains(h.workerStderr(), marker) {
		t.Fatalf("gateway child output reached worker stderr")
	}
}

func TestThinkingPerPrompt(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{ReasoningEffort: "high"})
	completed(t, h.Execute("one"))
	completed(t, h.Execute("two"))
	_, _, _, st := h.Wake("three")
	completed(t, st)
	ps := e.callerPrompts()
	if len(ps) != 3 {
		t.Fatalf("caller prompts %d, want 3", len(ps))
	}
	for _, p := range ps {
		if meta(p)["thinking"] != "high" {
			t.Fatalf("prompt without _meta.thinking=high: %v", p)
		}
	}
	for _, m := range []string{"session/set_config_option", "session/set_mode"} {
		if n := len(e.recv(m)); n != 0 {
			t.Fatalf("%s sent %d time(s)", m, n)
		}
	}
	e2 := newEnv(t)
	_, err := e2.start().Open(kit.OpenOptions{ReasoningEffort: "turbo"})
	mustContain(t, "open error", rpcText(err), "unsupported value reasoning_effort=turbo")
	e2.noNativeWork(t)
}

// The resumed lane is the same lane — the daemon replays the original open, and
// every caller prompt after the resume, Text and wake alike, still carries _meta.thinking.
func TestThinkingAfterResume(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{ReasoningEffort: "high"})
	completed(t, h.Execute("before close"))
	h.Close()
	h2 := e.start()
	h2.mustResume(key, kit.OpenOptions{ReasoningEffort: "high"})
	completed(t, h2.Execute("after resume"))
	_, _, _, st := h2.Wake("wake after resume")
	completed(t, st)
	ps := e.callerPrompts()
	if len(ps) != 3 {
		t.Fatalf("caller prompts %d, want 3", len(ps))
	}
	for _, p := range ps[1:] {
		if p["key"] != key || meta(p)["thinking"] != "high" {
			t.Fatalf("resumed prompt without _meta.thinking=high: %v", p)
		}
	}
}

func TestThinkingUnsupportedByModel(t *testing.T) {
	e := newEnv(t)
	e.script = `{"rejectThinking":{"xhigh":"mock/mock"}}`
	h := e.start()
	h.mustOpen(kit.OpenOptions{ReasoningEffort: "xhigh"})
	st := h.Execute("hi")
	if outcome(st) != "failed" || !strings.HasPrefix(result(st), "unsupported value reasoning_effort=xhigh") {
		t.Fatalf("run %s %q", outcome(st), result(st))
	}
	mustContain(t, "result", result(st), `Thinking level "xhigh" is not supported for mock/mock`)
}

// ---------------------------------------------------------------- cwd

func TestCwdCreatesSession(t *testing.T) {
	e := newEnv(t)
	root := e.root("lane")
	sub := e.mkdir(root, "sub")
	link := filepath.Join(root, "via-link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{Cwd: link})
	creates := e.gatewayCalls("sessions.create")
	if len(creates) != 1 || params(creates[0])["key"] != key || params(creates[0])["cwd"] != sub {
		t.Fatalf("sessions.create calls %v, want {key:%s cwd:%s}", creates, key, sub)
	}
	createAt := e.index(func(r logRec) bool { return r["event"] == "gateway_call" && r["method"] == "sessions.create" })
	acpAt := e.index(func(r logRec) bool { return r["event"] == "start" })
	if createAt < 0 || acpAt < 0 || createAt > acpAt {
		t.Fatalf("sessions.create (log #%d) must precede the ACP child (log #%d)", createAt, acpAt)
	}
	if news := e.recv("session/new"); len(news) != 1 || news[0]["cwd"] != sub {
		t.Fatalf("session/new %v, want cwd=%s", news, sub)
	}
	if got := completed(t, h.Execute("EXEC:pwd")); got != sub {
		t.Fatalf("exec pwd %q, want %s", got, sub)
	}
}

func TestCwdAbsentUsesAgentRoot(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	completed(t, h.Execute("hi"))
	// Check the invariant this test actually names: no
	// sessions.create/sessions.patch call for a cwd-less open — rather than zero Gateway calls of
	// any kind. The runtime admin guard now makes exactly one exec.approvals.get
	// probe call per Open, for a reason unrelated to cwd, which the original "" (any method)
	// filter no longer distinguishes from an actual cwd-triggered call.
	if g := e.gatewayCalls("sessions.create"); len(g) != 0 {
		t.Fatalf("sessions.create call without a cwd: %v", g)
	}
	if g := e.gatewayCalls("sessions.patch"); len(g) != 0 {
		t.Fatalf("sessions.patch call without a cwd: %v", g)
	}
	if news := e.recv("session/new"); len(news) != 1 || news[0]["cwd"] != e.root("lane") {
		t.Fatalf("session/new %v, want cwd=%s", news, e.root("lane"))
	}
	ps := e.callerPrompts()
	if len(ps) != 1 {
		t.Fatalf("caller prompts %v", ps)
	}
	for _, p := range ps {
		if meta(p)["prefixCwd"] == false {
			t.Fatalf("caller prompt disables the cwd prefix: %v", p)
		}
	}
}

func TestCwdOutsideAgentWorkspace(t *testing.T) {
	base := newEnv(t, "a=Alpha role", "b=Beta role")
	ra := base.root("a")
	outside := base.mkdir(base.dir, "outside")
	base.mkdir(base.dir, "ws", "x")
	if err := os.Symlink(outside, filepath.Join(ra, "escape")); err != nil {
		t.Fatal(err)
	}
	sub := base.mkdir(ra, "sub")
	args := []string{"--agent", "a"}
	for _, cwd := range []string{base.root("b"), outside, filepath.Join(ra, "escape"), ra + "/../x"} {
		e := *base
		e.t = t
		mark := e.mark()
		_, err := e.start().Open(kit.OpenOptions{Cwd: cwd, Arguments: args})
		mustContain(t, "open cwd="+cwd, rpcText(err), "unsupported value cwd="+cwd, "outside the workspace of agent a")
		if n := len(e.since(mark, "")); n != 0 {
			t.Fatalf("cwd=%s: refused open touched the native side: %v", cwd, e.since(mark, ""))
		}
	}
	for _, cwd := range []string{ra, sub} {
		e := *base
		e.t = t
		h := e.start()
		h.mustOpen(kit.OpenOptions{Cwd: cwd, Arguments: args})
		h.Close()
	}
}

func TestCwdRelativeRejected(t *testing.T) {
	e := newEnv(t)
	e.mkdir(e.root("lane"), "sub")
	_, err := e.start().Open(kit.OpenOptions{Cwd: "sub"})
	mustContain(t, "open error", rpcText(err), "unsupported value cwd=sub (must be absolute)")
	e.noNativeWork(t)
}

func TestCwdMissingRejected(t *testing.T) {
	e := newEnv(t)
	missing := filepath.Join(e.root("lane"), "nope")
	_, err := e.start().Open(kit.OpenOptions{Cwd: missing})
	mustContain(t, "open error", rpcText(err), "unsupported value cwd="+missing+" (does not exist)")
	e.noNativeWork(t)
}

func TestCwdRefusedMapsError(t *testing.T) {
	e := newEnv(t)
	e.script = `{"gateway":{"sessions.create":{"fail":"missing_admin"}}}`
	sub := e.mkdir(e.root("lane"), "sub")
	h := e.start()
	_, err := h.Open(kit.OpenOptions{Cwd: sub})
	mustContain(t, "open error", rpcText(err), "unsupported value cwd="+sub+" (refused by OpenClaw: ", "operator.admin")
	if n := e.acpStarts(); n != 0 {
		t.Fatalf("ACP started after a refused cwd")
	}
	creates := e.gatewayCalls("sessions.create")
	if len(creates) != 1 {
		t.Fatalf("sessions.create calls %v", creates)
	}
	if key := str(params(creates[0])["key"]); !h.lockFree(key) {
		t.Fatalf("lock of %s still held after the refused open", key)
	}
}

func TestCwdGatewayTransportFailure(t *testing.T) {
	marker := secretMarker(t)
	e := newEnv(t)
	e.script = `{"marker":"` + marker + `","gateway":{"sessions.create":{"fail":"transport"}}}`
	sub := e.mkdir(e.root("lane"), "sub")
	_, err := e.start().Open(kit.OpenOptions{Cwd: sub})
	if err == nil || err.Code != protocol.SpawnFailed {
		t.Fatalf("open error %s, want spawn_failed", rpcText(err))
	}
	text := rpcText(err)
	mustContain(t, "open error", text, "gateway call failed")
	if strings.Contains(text, marker) || strings.Contains(text, "1006") || strings.Contains(text, "unsupported value") {
		t.Fatalf("transport cause not redacted / misclassified: %q", text)
	}
	if n := e.acpStarts(); n != 0 {
		t.Fatalf("ACP started after a transport failure")
	}
}

// ---------------------------------------------------------------- admin guard

const adminGuardText = "lane credential has operator.admin; pair a read/write/approvals device (lane/README.md) and point the lane at its state dir"

// heldLocks lists the lane locks under the worker socket's lock dir that are still held.
func (h *harness) heldLocks() []string {
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(h.sock), "locks", "openclaw-peer"))
	var held []string
	for _, d := range entries {
		if !h.lockFree(d.Name()) {
			held = append(held, d.Name())
		}
	}
	return held
}

// adminRefusedOpen checks what every refused-by-the-guard Open leaves behind: exactly one probe
// (exec.approvals.get, empty params, through the Gateway command), no sessions.create/patch, no
// ACP child, no held lane lock.
func adminRefusedOpen(t *testing.T, e *testEnv, h *harness, err *protocol.RPCError, wants ...string) string {
	t.Helper()
	if err == nil || err.Code != protocol.SpawnFailed {
		t.Fatalf("open error %s, want spawn_failed", rpcText(err))
	}
	text := rpcText(err)
	mustContain(t, "open error", text, wants...)
	probes := e.gatewayCalls("exec.approvals.get")
	if len(probes) != 1 || len(params(probes[0])) != 0 {
		t.Fatalf("admin probes %v, want exactly one exec.approvals.get with empty params", probes)
	}
	if g := append(e.gatewayCalls("sessions.create"), e.gatewayCalls("sessions.patch")...); len(g) != 0 {
		t.Fatalf("session Gateway calls after a refused credential: %v", g)
	}
	if n := e.acpStarts(); n != 0 {
		t.Fatalf("ACP child started %d time(s) for a refused credential", n)
	}
	if held := h.heldLocks(); len(held) != 0 {
		t.Fatalf("lane lock(s) still held after the refused open: %v", held)
	}
	return text
}

// The probe answering means the lane's Gateway credential is admin → Open refused
// with the guard text, before any session Gateway call, lock or ACP child — also when the caller
// asked for a cwd and a model (their Gateway calls must not run for a credential being refused).
func TestAdminCredentialRefused(t *testing.T) {
	e := newEnv(t)
	e.script = `{"admin":true}`
	sub := e.mkdir(e.root("lane"), "sub")
	h := e.start()
	_, err := h.Open(kit.OpenOptions{Cwd: sub, Model: "mock/mock-b"})
	adminRefusedOpen(t, e, h, err, adminGuardText)
}

// The replayed (resume) open is guarded too, and releases the lock of the key it would resume.
func TestAdminCredentialRefusedOnResume(t *testing.T) {
	e := newEnv(t)
	e.script = `{"admin":true}`
	key := "agent:lane:sb-" + newUUID4(t)
	h := e.start()
	_, err := h.Resume(key, kit.OpenOptions{})
	adminRefusedOpen(t, e, h, err, adminGuardText)
	if !h.lockFree(key) {
		t.Fatalf("lock of %s still held after the refused resume", key)
	}
}

// The guard fails closed. A probe that neither answers nor is refused for the
// missing operator.admin scope — a transport failure, or another Gateway error such as unpaired
// credentials — refuses the Open with "lane credential check failed: <sanitized reason>".
func TestAdminCheckFailsClosed(t *testing.T) {
	for _, fail := range []string{"transport", "credentials_required"} {
		t.Run(fail, func(t *testing.T) {
			marker := secretMarker(t)
			e := newEnv(t)
			e.script = `{"marker":"` + marker + `","gateway":{"exec.approvals.get":{"fail":"` + fail + `"}}}`
			h := e.startBinary()
			_, err := h.Open(kit.OpenOptions{})
			text := adminRefusedOpen(t, e, h, err, "lane credential check failed: ")
			if strings.Contains(text, marker) || strings.Contains(text, "operator.admin;") {
				t.Fatalf("check failure not sanitized / misclassified as admin: %q", text)
			}
			if strings.Contains(h.workerStderr(), marker) {
				t.Fatalf("gateway child output reached worker stderr")
			}
		})
	}
}

// Caching, on one Wrapper (the kit worker takes one session.open per process, so
// only the Wrapper can be asked twice): an inconclusive check is not cached — once the Gateway
// recovers the next Open gets past the guard; a confirmed non-admin refusal is cached — no
// further probe. An admin answer is cached too (once per worker process).
func TestAdminCheckCaching(t *testing.T) {
	probeOpens := func(t *testing.T, scripts ...string) (*testEnv, []error) {
		e := newEnv(t)
		e.script = filepath.Join(e.dir, "script.json")
		for k, v := range e.vars() {
			t.Setenv(k, v)
		}
		t.Setenv(host.SocketEnv, filepath.Join(testsocket.Directory(t), "bus.sock"))
		t.Setenv(host.TokenEnv, "lane-test-token")
		cfg, bridge, err := lane.LoadConfig(os.Environ())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		t.Cleanup(bridge.Close)
		w := lane.New(cfg, bridge)
		w.SetCaller(kit.NewWorker(w).Caller()) // bus.action needs one; never served, never called
		t.Cleanup(func() { _ = w.Close(context.Background(), kit.SessionCloseRequest{}) })
		var errs []error
		for _, s := range scripts {
			if err := os.WriteFile(e.script, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := w.Open(context.Background(), kit.OpenRequest{})
			errs = append(errs, err)
			_ = w.Close(context.Background(), kit.SessionCloseRequest{})
		}
		return e, errs
	}
	credential := func(err error) string {
		switch {
		case err == nil:
			return "passed"
		case strings.Contains(err.Error(), "lane credential check failed: "):
			return "check failed"
		case strings.Contains(err.Error(), adminGuardText):
			return "admin"
		}
		return "refused later: " + err.Error()
	}

	t.Run("inconclusive not cached, non-admin cached", func(t *testing.T) {
		e, errs := probeOpens(t, `{"gateway":{"exec.approvals.get":{"fail":"transport"}}}`, `{}`, `{"admin":true}`)
		if got := []string{credential(errs[0]), credential(errs[1]), credential(errs[2])}; strings.Join(got, ",") != "check failed,passed,passed" {
			t.Fatalf("opens %v (%v), want check failed, passed, passed", got, errs)
		}
		if n := len(e.gatewayCalls("exec.approvals.get")); n != 2 {
			t.Fatalf("probes %d, want 2 (retry after the failure, none after the confirmed refusal)", n)
		}
	})
	t.Run("admin cached", func(t *testing.T) {
		e, errs := probeOpens(t, `{"admin":true}`, `{}`)
		if got := []string{credential(errs[0]), credential(errs[1])}; strings.Join(got, ",") != "admin,admin" {
			t.Fatalf("opens %v (%v), want admin, admin", got, errs)
		}
		if n := len(e.gatewayCalls("exec.approvals.get")); n != 1 {
			t.Fatalf("probes %d, want 1", n)
		}
	})
	for _, fail := range []string{"unrelated_internal", "unrelated_missing_scope"} {
		t.Run(fail+" not cached", func(t *testing.T) {
			script := `{"gateway":{"exec.approvals.get":{"fail":"` + fail + `"}}}`
			e, errs := probeOpens(t, script, script)
			for _, err := range errs {
				if got := credential(err); got != "check failed" {
					t.Fatalf("probe outcome %q (%v), want check failed", got, err)
				}
			}
			if n := len(e.gatewayCalls("exec.approvals.get")); n != 2 {
				t.Fatalf("probes %d, want 2: an unrelated refusal must not be cached", n)
			}
		})
	}
}

// ---------------------------------------------------------------- slash and directives

func TestRunRefusesSlashTextCallback(t *testing.T) {
	w := &lane.Wrapper{}
	for _, text := range []string{"/x", "  /x", "\n\n/x", "\ufeff/x", "\u200b/x", "\t\u00a0/status",
		"\u2003/x", "\u2028/x", "\u3000/x", "\u1680\u202f\u205f/x"} {
		result, err := w.Run(context.Background(), nil, kit.RunInput{Text: &text})
		if err == nil || err.Error() != "slash commands are not accepted over the lane" || result != (kit.TurnResult{}) {
			t.Fatalf("text %q → result=%+v err=%v", text, result, err)
		}
	}
}

func TestRunRefusesSlashText(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	// Non-ASCII JS \s (U+2003 from U+2000–U+200A, U+2028, U+3000, U+1680,
	// U+202F, U+205F) before the slash.
	for _, text := range []string{"/x", "  /x", "\n\n/x", "\ufeff/x", "\u200b/x", "\t\u00a0/status",
		"\u2003/x", "\u2028/x", "\u3000/x", "\u1680\u202f\u205f/x"} {
		st := h.Execute(text)
		if st.State != "unavailable" || st.Reason != "slash commands are not accepted over the lane" || st.Result != nil {
			t.Fatalf("text %q → state=%s reason=%q result=%+v", text, st.State, st.Reason, st.Result)
		}
	}
	if ps := e.callerPrompts(); len(ps) != 0 {
		t.Fatalf("refused slash text reached the agent: %v", ps)
	}
	req, receipt, rerr, st := h.Wake("/x")
	if rerr != nil || receipt.Disposition != "injected" {
		t.Fatalf("delivery /x: receipt %+v err %s", receipt, rpcText(rerr))
	}
	completed(t, st)
	rendered, _ := host.RenderNativeMessage(req)
	if ps := e.callerPrompts(); len(ps) != 1 || ps[0]["text"] != rendered {
		t.Fatalf("delivery prompt %v, want the rendered envelope %q", ps, rendered)
	}
}

func TestDirectivesNeutralized(t *testing.T) {
	const zw = "\u200b"
	cases := []struct{ in, want string }{
		{"hello /model x", "hello " + zw + "/model x"},
		{"hello /status", "hello " + zw + "/status"},
		{"x\n/whoami", "x\n" + zw + "/whoami"},
		{"please /think high", "please " + zw + "/think high"},
		{"a /t: x", "a " + zw + "/t: x"},
		{"run /exec now", "run " + zw + "/exec now"},
		{"q\t/queue", "q\t" + zw + "/queue"},
		{"v /Verbose on", "v " + zw + "/Verbose on"},
		{"r /reasoning on and /fast", "r " + zw + "/reasoning on and " + zw + "/fast"},
		{"h /HELP", "h " + zw + "/HELP"},
		{"i /id", "i " + zw + "/id"},
		{"e /elevated /elev /commands /trace /v /thinking /reason", "e " + zw + "/elevated " + zw + "/elev " + zw + "/commands " + zw + "/trace " + zw + "/v " + zw + "/thinking " + zw + "/reason"},
		// Non-ASCII JS \s on either side of the directive name.
		{"a\u2003/status", "a\u2003" + zw + "/status"},
		{"b\u3000/model x", "b\u3000" + zw + "/model x"},
		{"c\u2028/whoami", "c\u2028" + zw + "/whoami"},
		{"d /status\u2003now", "d " + zw + "/status\u2003now"},
		{"e /help\u3000", "e " + zw + "/help\u3000"},
		{"f /id\u2029g", "f " + zw + "/id\u2029g"},
		// byte-identical
		{"see /path/like/this", "see /path/like/this"},
		{"see /tmp/x", "see /tmp/x"},
		{"x /models are fun", "x /models are fun"},
		{"a/model", "a/model"},
		{"go /modelx and /statuses", "go /modelx and /statuses"},
		{"hello/status", "hello/status"},
	}
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	for _, c := range cases {
		completed(t, h.Execute(c.in))
		ps := e.callerPrompts()
		if got := str(ps[len(ps)-1]["text"]); got != c.want {
			t.Fatalf("text %q reached the agent as %q, want %q", c.in, got, c.want)
		}
	}
	for _, body := range []string{"/path/like/this", "hello /status", "/models are fun"} {
		req, _, _, st := h.Wake(body)
		completed(t, st)
		rendered, _ := host.RenderNativeMessage(req)
		want := rendered
		if body == "hello /status" {
			want = strings.Replace(rendered, "hello /status", "hello "+zw+"/status", 1)
		}
		ps := e.callerPrompts()
		if got := str(ps[len(ps)-1]["text"]); got != want {
			t.Fatalf("delivery %q reached the agent as %q, want %q", body, got, want)
		}
	}
}

// ---------------------------------------------------------------- crash and delivery

// Exercise the real fake-process exit, not a simulated bridge callback. Neither
// a failed child nor a refused next Run may retire the SDK's retained records.
func assertChildExitRetained(t *testing.T, e *testEnv, h *harness, run, key string) {
	t.Helper()
	select {
	case <-h.gone:
		t.Fatal("ACP child exit retired worker before explicit close")
	case <-time.After(1200 * time.Millisecond):
	}
	eventually(t, "native session lock released after child exit", func() bool { return h.lockFree(key) })
	first, ok := h.request("turn.status", protocol.ReadRequest{SessionID: h.laneID, RunID: run})
	var original protocol.RunStatus
	if !ok || first.Error != nil {
		t.Fatalf("crashed record status: %+v", first)
	}
	if err := protocol.UnmarshalResult("turn.status", first.Result, &original); err != nil {
		t.Fatal(err)
	}
	if original.State != "unavailable" || original.Reason == "" {
		t.Fatalf("crashed record = %+v", original)
	}
	prompts := len(e.events("prompt"))
	refusedRun := h.Start("must not reach exited native session")
	h.waitReady(refusedRun, waitFor)
	f, ok := h.request("turn.wait", protocol.WaitRequest{SessionID: h.laneID, RunID: refusedRun})
	var refused protocol.RunStatus
	if !ok || f.Error != nil {
		t.Fatalf("refused Run wait: %+v", f)
	}
	if err := protocol.UnmarshalResult("turn.wait", f.Result, &refused); err != nil {
		t.Fatal(err)
	}
	if refused.State != "unavailable" || refused.Reason != "lane native session exited" {
		t.Fatalf("post-exit Run = %+v", refused)
	}
	later, ok := h.request("turn.wait", protocol.WaitRequest{SessionID: h.laneID, RunID: run})
	if !ok || later.Error != nil || string(first.Result) != string(later.Result) {
		t.Fatalf("crashed record changed: status=%s wait=%+v", first.Result, later)
	}
	if n := len(e.events("prompt")); n != prompts {
		t.Fatalf("new prompt reached exited child: %d vs %d", n, prompts)
	}
	for _, ref := range []string{run, refusedRun} {
		if f, ok := h.request("turn.ack", protocol.RunRef{SessionID: h.laneID, RunID: ref}); !ok || f.Error != nil {
			t.Fatalf("ack: %+v", f)
		}
	}
	h.Close()
}

func TestChildCrashMidTurn(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	if h.lockFree(key) {
		t.Fatal("native session lock is free before child exit")
	}
	run := h.Start("please CRASH")
	var ready protocol.TurnReady
	for ready.RunID != run {
		select {
		case ready = <-h.ready:
		case <-h.eof:
			t.Fatal("connection closed before turn.ready")
		}
	}
	if ready.State != "unavailable" || ready.Reason == "" {
		t.Fatalf("crashed run ready %+v, want a run error (state unavailable)", ready)
	}
	assertChildExitRetained(t, e, h, run, key)
}

// At the wrapper level, a child that exits cleanly (code 0) in the middle
// of a turn ends the run unavailable and retains the worker, exactly like a crash.
func TestChildCleanExitMidTurn(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	if h.lockFree(key) {
		t.Fatal("native session lock is free before child exit")
	}
	run := h.Start("please CRASH0")
	ready := h.waitReady(run, waitFor)
	if ready.State != "unavailable" || !strings.Contains(ready.Reason, "code=0") {
		t.Fatalf("clean-exit run ready %+v, want a run error naming code=0", ready)
	}
	assertChildExitRetained(t, e, h, run, key)
}

// At the wrapper level, the child exits after a complete answer,
// while the lane is idle. The next run cannot reach it; that run ends unavailable and the worker
// retains that record while refusing further native work. The test waits until the worker has reaped
// the child (kill(pid, 0) → ESRCH), so the next prompt meets a closed client, not a racing write.
func TestChildExitWhileIdleRetainsWorker(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	if h.lockFree(key) {
		t.Fatal("native session lock is free before child exit")
	}
	completed(t, h.Execute("bye EXIT_AFTER"))
	eventually(t, "the child to exit", func() bool { return len(e.events("exit_after_reply")) == 1 })
	pid := int(e.events("exit_after_reply")[0]["pid"].(float64))
	eventually(t, "the worker to reap the child", func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	time.Sleep(50 * time.Millisecond)
	prompts := len(e.events("prompt"))
	run := h.Start("after the child died")
	if ready := h.waitReady(run, waitFor); ready.State != "unavailable" || ready.Reason == "" {
		t.Fatalf("run to a dead child: %+v, want unavailable with a reason", ready)
	}
	assertChildExitRetained(t, e, h, run, key)
	if n := len(e.events("prompt")); n != prompts {
		t.Fatalf("a prompt reached a child after it died")
	}
}

// The fake closes stdin after a completed turn, then exits while a helper keeps its output
// pipes open. This pins the next write to EPIPE before the client's reader records the exit.
func TestDeadChildWriteBeforeExitObservedRetainsWorker(t *testing.T) {
	deadChildWriteBeforeExitObserved(t, 0)
}

// Regression (flake-deadchild-deliver, CI run 37785684215): the fake's os.Stdin.Close does not
// close fd 0 while its reader is blocked in Read, so the pipe stays open until the fake has
// exited. When stdin_closed_for_exit was logged before that exit, a fake slow to exit (holdExitMs,
// a descheduled CI runner) took the session/prompt write into the pipe buffer: no EPIPE, no reply,
// and message.deliver got no receipt. The helper now logs the event after the real exit.
func TestDeadChildWriteBeforeSlowFakeExit(t *testing.T) {
	deadChildWriteBeforeExitObserved(t, 300)
}

func deadChildWriteBeforeExitObserved(t *testing.T, holdExitMs int) {
	e := newEnv(t)
	lockFile := filepath.Join(t.TempDir(), "hold-output")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	script, _ := json.Marshal(map[string]any{"holdOutputFile": lockFile, "holdExitMs": holdExitMs})
	e.script = string(script)
	release := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	var releaseOnce sync.Once
	h := e.start()
	t.Cleanup(func() { releaseOnce.Do(release) })
	key := h.mustOpen(kit.OpenOptions{})
	if h.lockFree(key) {
		t.Fatal("native session lock is free before child exit")
	}
	completed(t, h.Execute("bye EXIT_AFTER"))
	eventually(t, "fake stdin to close", func() bool { return len(e.events("stdin_closed_for_exit")) == 1 })
	prompts := len(e.events("prompt"))
	req, receipt, rerr := h.WakeStart("after stdin closed")
	if rerr != nil || receipt.Disposition != "rejected" || receipt.Reason != "not_submitted" {
		t.Fatalf("dead-child write: receipt %+v err %s, want rejected/not_submitted", receipt, rpcText(rerr))
	}
	ready := h.waitReady(req.RunID, waitFor)
	if ready.State != "unavailable" || !strings.Contains(ready.Reason, "acp write") {
		t.Fatalf("dead-child write ready %+v, want unavailable from acp write", ready)
	}
	if n := len(e.events("prompt")); n != prompts {
		t.Fatalf("a prompt reached the fake after stdin closed: %d vs %d", n, prompts)
	}
	releaseOnce.Do(release)
	assertChildExitRetained(t, e, h, req.RunID, key)
}

func TestDeliveryRun(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	req, receipt, rerr, st := h.Wake("authorized message")
	if rerr != nil || receipt.Disposition != "injected" {
		t.Fatalf("receipt %+v err %s", receipt, rpcText(rerr))
	}
	rendered, err := host.RenderNativeMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "wake result", completed(t, st), "authorized message", `from-session="parent@local"`)
	if ps := e.callerPrompts(); len(ps) != 1 || ps[0]["text"] != rendered {
		t.Fatalf("delivery prompt %v, want %q", ps, rendered)
	}
}

// deliveryReceipt decodes a message.deliver answer that carries a receipt.
func deliveryReceipt(t *testing.T, f protocol.Frame) kit.DeliveryReceipt {
	t.Helper()
	if f.Error != nil {
		t.Fatalf("deliver: %s", rpcText(f.Error))
	}
	var r kit.DeliveryReceipt
	if err := protocol.UnmarshalResult("message.deliver", f.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func steerEvents(e *testEnv) []fakebridge.Event {
	var out []fakebridge.Event
	for _, ev := range e.bridge.Log() {
		if ev.Method == "lane.steer" {
			out = append(out, ev)
		}
	}
	return out
}

// Busy lane, plugin eligible: the message is steered through the bridge (written) and never
// becomes a second ACP prompt.
func TestDeliverBusySteersViaBridge(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
	if r := deliveryReceipt(t, h.Deliver("while busy")); r.Disposition != "written" {
		t.Fatalf("receipt %+v, want written", r)
	}
	ev := steerEvents(e)
	if len(ev) != 1 || ev[0].Params["sessionKey"] != key || ev[0].Params["messageId"] != "m-idle" || !strings.Contains(str(ev[0].Params["text"]), "while busy") {
		t.Fatalf("steer frames %+v", ev)
	}
	if n := len(e.callerPrompts()); n != 1 {
		t.Fatalf("a steered delivery became a second prompt (%d prompts)", n)
	}
	_ = h.Interrupt()
	h.Wait(run)
}

func TestDeliverSteerNeutralizesDirectives(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
	deliveryReceipt(t, h.Deliver("/model x then /status"))
	ev := steerEvents(e)
	if len(ev) != 1 {
		t.Fatalf("steer frames %+v", ev)
	}
	text := str(ev[0].Params["text"])
	if !strings.Contains(text, "\u200b/model x") || !strings.Contains(text, "\u200b/status") || strings.Contains(text, " /model") {
		t.Fatalf("directives not neutralized in %q", text)
	}
	_ = h.Interrupt()
	h.Wait(run)
}

// The D2/D13/D21 table for a busy lane.
func TestDeliverBusyOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		steer    []string
		wantOK   string // receipt disposition
		wantCode int    // RPC error code
		wantData string
		steers   int
		fallback bool
	}{
		{name: "not eligible after retries → not_running", steer: []string{"false"}, wantCode: protocol.NotRunning, steers: 4, fallback: true},
		{name: "eligible on the second retry → written", steer: []string{"false", "false", "eligible"}, wantOK: "written", steers: 3},
		{name: "no reply → uncertain", steer: []string{"no-reply"}, wantCode: protocol.Internal, wantData: "uncertain_native_admission", steers: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.steerSeq = tc.steer
			h := e.startBinary()
			h.mustOpen(kit.OpenOptions{})
			run := h.Start("SLOW")
			eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
			start := time.Now()
			f := h.Deliver("while busy")
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("Deliver held %s", d)
			}
			if tc.wantOK != "" {
				if r := deliveryReceipt(t, f); r.Disposition != tc.wantOK {
					t.Fatalf("receipt %+v", r)
				}
			} else if f.Error == nil || f.Error.Code != tc.wantCode || !strings.Contains(string(f.Error.Data), tc.wantData) {
				t.Fatalf("answer %s / %s, want code %d data %q", f.Result, rpcText(f.Error), tc.wantCode, tc.wantData)
			}
			if n := len(steerEvents(e)); n != tc.steers {
				t.Fatalf("steer frames %d, want %d", n, tc.steers)
			}
			if tc.fallback {
				eventually(t, "the fallback line on stderr", func() bool {
					return strings.Contains(h.workerStderr(), "steer fallback (not BUSY-MID): reason=not_eligible")
				})
			} else if strings.Contains(h.workerStderr(), "steer fallback") {
				t.Fatalf("unexpected fallback line; stderr %q", h.workerStderr())
			}
			if n := len(e.callerPrompts()); n != 1 {
				t.Fatalf("a delivery reached the agent as a prompt (%d)", n)
			}
			_ = h.Interrupt()
			h.Wait(run)
		})
	}
}

// A steer frame the plugin refuses (error frame, e.g. unbound) admitted nothing.
func TestDeliverSteerRefusedIsNotSubmitted(t *testing.T) {
	e := newEnv(t)
	e.steer = fakebridge.Refuse
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
	if r := deliveryReceipt(t, h.Deliver("x")); r.Disposition != "rejected" || r.Reason != "not_submitted" {
		t.Fatalf("receipt %+v", r)
	}
	_ = h.Interrupt()
	h.Wait(run)
}

// Bridge gone mid-run: nothing can be steered; the delivery is definitely not submitted.
func TestDeliverAfterBridgeLossIsNotSubmitted(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
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
	_ = h.Interrupt()
	h.Wait(run)
	// Explicit close owns worker retirement and joins native cleanup.
	h.Close()
}

func TestDeliverOpenIdleIsNotRunning(t *testing.T) {
	e := newEnv(t)
	e.script = `{"bigReplyBytes":1572864,"bigReplyUnit":"\u0001<>&\u001f\""}`
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	const runs = 3
	for i := 0; i < runs; i++ {
		completed(t, h.Execute(fmt.Sprintf("big %d", i)))
		// between turns the lane is open but has no active turn: nothing to steer into
		f := h.Deliver("between turns")
		if f.Error == nil || f.Error.Code != protocol.NotRunning {
			t.Fatalf("after run %d: result %s error %s, want not_running", i, f.Result, rpcText(f.Error))
		}
	}
	if n := len(e.callerPrompts()); n != runs {
		t.Fatalf("caller prompts %d, want %d: a refused delivery reached the agent", n, runs)
	}
	if n := len(steerEvents(e)); n != 0 {
		t.Fatalf("%d steer frames while idle", n)
	}
}

// ---------------------------------------------------------------- secrets and child env

func TestChildEnvScrubsGatewaySecrets(t *testing.T) {
	e := newEnv(t)
	sub := e.mkdir(e.root("lane"), "sub")
	scrubbed := []string{"OPENCLAW_GATEWAY_TOKEN", "OPENCLAW_GATEWAY_PASSWORD", "OPENCLAW_GATEWAY_URL",
		host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv}
	h := e.startBinary("OPENCLAW_GATEWAY_TOKEN=tok-SECRET-1", "OPENCLAW_GATEWAY_PASSWORD=pw-SECRET-2",
		"OPENCLAW_GATEWAY_URL=ws://gw.invalid:1", host.SessionIDEnv+"=parent@local", host.NameEnv+"=lane", host.GroupsEnv+"=g", host.LocalKeyEnv+"=")
	h.mustOpen(kit.OpenOptions{Cwd: sub, Model: "mock/mock-b"})
	completed(t, h.Execute("hi"))
	children := append(e.events("start"), e.events("gateway_call")...)
	if len(children) < 3 {
		t.Fatalf("expected the ACP child and two gateway calls, got %v", children)
	}
	for _, c := range children {
		names, _ := c["envNames"].([]any)
		for _, n := range names {
			for _, s := range scrubbed {
				if n == s {
					t.Errorf("%s child sees %s", c["mode"], s)
				}
			}
		}
		if c["hideBanner"] != "1" || c["suppressNotes"] != "1" {
			t.Errorf("%s child without OPENCLAW_HIDE_BANNER/SUPPRESS_NOTES=1: %v %v", c["mode"], c["hideBanner"], c["suppressNotes"])
		}
		argv, _ := json.Marshal(c["argv"])
		for _, bad := range []string{"SECRET", "--url", "--token", "--password", "gw.invalid"} {
			if strings.Contains(string(argv), bad) {
				t.Errorf("%s child argv %s contains %q", c["mode"], argv, bad)
			}
		}
	}
}

func secretMarker(t *testing.T) string {
	t.Helper()
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "OCPSECRET-" + hex.EncodeToString(b[:])
}

func newUUID4(t *testing.T) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func TestNoSecretInLogs(t *testing.T) {
	marker := secretMarker(t)
	var seen []string // every caller-visible text: worker stderr, open errors, results, receipts
	noisy := `"marker":"` + marker + `","noise":["banner noise"],"stderr":["child warning"]`

	// A Gateway refusal whose message carries the marker.
	e1 := newEnv(t)
	e1.script = `{` + noisy + `,"gateway":{"sessions.create":{"fail":"missing_admin"}}}`
	sub := e1.mkdir(e1.root("lane"), "sub")
	h1 := e1.startBinary()
	_, err := h1.Open(kit.OpenOptions{Cwd: sub})
	mustContain(t, "refused open", rpcText(err), "operator.admin")
	seen = append(seen, rpcText(err))
	_ = h1.conn.Close()
	h1.waitGone()
	stderr1 := h1.workerStderr()
	mustContain(t, "worker stderr", stderr1, "gateway call sessions.create failed: code=FORBIDDEN missing_scope=operator.admin")
	seen = append(seen, stderr1)

	// A model refusal, a run, a wake delivery and a failing session/close.
	e2 := newEnv(t)
	e2.script = `{` + noisy + `,"fail":{"session/close":"close exploded"}}`
	h2 := e2.startBinary()
	h2.mustOpen(kit.OpenOptions{})
	st := h2.Execute("hi")
	seen = append(seen, result(st), st.Reason)
	_, receipt, rerr, st2 := h2.Wake("msg")
	seen = append(seen, receipt.Disposition+receipt.Reason, rpcText(rerr), result(st2), st2.Reason)
	h2.Close()
	stderr2 := h2.workerStderr()
	mustContain(t, "worker stderr", stderr2, "skipped non-JSON stdout line (")
	seen = append(seen, stderr2)

	e3 := newEnv(t)
	e3.script = `{` + noisy + `,"gateway":{"sessions.patch":{"fail":"unknown_model"}}}`
	h3 := e3.startBinary()
	_, err = h3.Open(kit.OpenOptions{Model: "bad/model"})
	mustContain(t, "refused model", rpcText(err), "unsupported value model=bad/model")
	seen = append(seen, rpcText(err))
	_ = h3.conn.Close()
	h3.waitGone()
	seen = append(seen, h3.workerStderr())

	for _, s := range seen {
		if strings.Contains(s, marker) || strings.Contains(s, marker[10:]) {
			t.Fatalf("credential marker leaked: %q", s)
		}
	}
}

// ---------------------------------------------------------------- transport and output

func TestNoiseStdoutTolerated(t *testing.T) {
	e := newEnv(t)
	e.script = `{"noise":["OpenClaw banner","{broken json"]}`
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	mustContain(t, "result", completed(t, h.Execute("hi")), "ECHO", "hi")
	mustContain(t, "second result", completed(t, h.Execute("again")), "again")
}

func TestOverlongLineFails(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("big OVERLONG:9437184")
	var ready protocol.TurnReady
	for ready.RunID != run {
		select {
		case ready = <-h.ready:
		case <-h.eof:
			t.Fatal("connection closed before turn.ready")
		}
	}
	if ready.State == "done" && ready.Outcome == "completed" {
		t.Fatalf("a >8 MiB line completed the run: %+v", ready)
	}
}

var truncRe = regexp.MustCompile(`\n\[truncated by openclaw-peer: (\d+) bytes omitted\]$`)

func TestRunResultTruncated(t *testing.T) {
	const total = 1572864
	e := newEnv(t)
	e.script = `{"bigReplyBytes":1572864}`
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	st := h.Execute("big")
	text := completed(t, st)
	if !st.Result.Truncated {
		t.Fatal("truncated flag not set")
	}
	m := truncRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no truncation note at the end: %q", text[max(0, len(text)-120):])
	}
	kept := len(text) - len(m[0])
	if kept > 900<<10 || kept == 0 {
		t.Fatalf("kept %d bytes, want ≤ 900 KiB", kept)
	}
	if omitted, _ := strconv.Atoi(m[1]); omitted != total-kept {
		t.Fatalf("note says %d bytes omitted, kept %d of %d", omitted, kept, total)
	}
}

func TestRunResultTruncatedEscapes(t *testing.T) {
	const total = 1572864
	e := newEnv(t)
	e.script = `{"bigReplyBytes":1572864,"bigReplyEscapes":true}`
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	st := h.Execute("big")
	text := completed(t, st)
	m := truncRe.FindStringSubmatch(text)
	if !st.Result.Truncated || m == nil {
		t.Fatalf("escape-heavy result not truncated with a note (truncated=%v)", st.Result.Truncated)
	}
	body := text[:len(text)-len(m[0])]
	full := strings.Repeat("\"\\\n\t\x01<>&", total/8+1)[:total]
	if body == "" || !strings.HasPrefix(full, body) {
		t.Fatalf("kept text (%d bytes) is not a prefix of the answer", len(body))
	}
	frame, err := protocol.ResultBytes(protocol.MaxRequestID, "turn.status", st)
	if err != nil || len(frame) > protocol.MaxFrameBytes {
		t.Fatalf("turn.status frame %d bytes (err %v), cap %d", len(frame), err, protocol.MaxFrameBytes)
	}
}

// ---------------------------------------------------------------- binary entry

func TestChildEnvUsesCredentialDir(t *testing.T) {
	e := newEnv(t)
	t.Setenv("OPENCLAW_STATE_DIR", "/admin-state")
	t.Setenv("OPENCLAW_CONFIG_PATH", "/admin-state/openclaw.json")
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	wantState := filepath.Join(e.dir, "cli")
	wantConfig := filepath.Join(wantState, "openclaw.json")
	for _, name := range []string{"gateway_call", "start"} {
		recs := e.events(name)
		if len(recs) == 0 {
			t.Fatalf("missing %s child", name)
		}
		for _, rec := range recs {
			if rec["stateDir"] != wantState || rec["configPath"] != wantConfig {
				t.Fatalf("%s child env: %+v", name, rec)
			}
		}
	}
}

func TestTokenlessExit(t *testing.T) {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, host.TokenEnv+"=") {
			env = append(env, kv)
		}
	}
	runBin := func(env []string, args ...string) (int, string, string) {
		cmd := exec.Command(workerBin, args...)
		cmd.Env = env
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, so.String(), se.String()
	}
	code, _, stderr := runBin(env)
	if code != 2 || !strings.Contains(stderr, `openclaw-peer (Go) is the Sessionbus lane worker; the interactive openclaw-peer is the OpenClaw plugin. Start a lane with spawn {product:"openclaw-peer"}`) {
		t.Fatalf("tokenless start: exit %d, stderr %q", code, stderr)
	}
	if code, stdout, stderr := runBin(env, "--version"); code != 0 || strings.TrimSpace(stdout+stderr) == "" {
		t.Fatalf("--version: exit %d, out %q %q", code, stdout, stderr)
	}
	code, _, stderr = runBin(append(env, host.TokenEnv+"=x"), "extra")
	if code != 1 || !strings.Contains(stderr, "lane worker arguments belong in session.open") {
		t.Fatalf("token + argv: exit %d, stderr %q", code, stderr)
	}
}

func TestLegacyVariablesExitOne(t *testing.T) {
	cmd := exec.Command(workerBin)
	cmd.Env = append(os.Environ(), host.TokenEnv+"=test-token", "OPENCLAW_LANE_AGENTS=[]")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "OPENCLAW_LANE_AGENTS no longer used") || !strings.Contains(string(out), "plugins.entries.sessionbus.config.lanes") {
		t.Fatalf("legacy start: %v, output %q", err, out)
	}
}

// An answer of 6-byte JSON escapes (control chars, <, >, &) plus quotes. The character
// cap alone keeps a result whose turn.status frame exceeds protocol.MaxFrameBytes; only the
// frame-size guard of the cap keeps the run completed ("oversized result kills the run").
func TestRunResultTruncatedFrameGuard(t *testing.T) {
	const total, unit = 1572864, "\x01<>&\x1f\""
	e := newEnv(t)
	e.script = `{"bigReplyBytes":1572864,"bigReplyUnit":"\u0001<>&\u001f\""}`
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	st := h.Execute("big")
	text := completed(t, st)
	m := truncRe.FindStringSubmatch(text)
	if !st.Result.Truncated || m == nil {
		t.Fatalf("result not truncated with a note (truncated=%v)", st.Result.Truncated)
	}
	body := text[:len(text)-len(m[0])]
	full := strings.Repeat(unit, total/len(unit)+1)[:total]
	if body == "" || !strings.HasPrefix(full, body) {
		t.Fatalf("kept text (%d bytes) is not a prefix of the answer", len(body))
	}
	if omitted, _ := strconv.Atoi(m[1]); omitted != total-len(body) {
		t.Fatalf("note says %d bytes omitted, kept %d of %d", omitted, len(body), total)
	}
	frame, err := protocol.ResultBytes(protocol.MaxRequestID, "turn.status", st)
	if err != nil || len(frame) > protocol.MaxFrameBytes {
		t.Fatalf("turn.status frame %d bytes (err %v), cap %d", len(frame), err, protocol.MaxFrameBytes)
	}
	if enc, _ := json.Marshal(body); len(enc) < 3*len(body) {
		t.Fatalf("payload is not escape-heavy: %d → %d bytes", len(body), len(enc))
	}
}

// A wake (Delivery) run is interruptible like a Text run: one session/cancel, outcome
// interrupted within a bound, and the lane keeps working.
func TestInterruptWakeRun(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	key := h.mustOpen(kit.OpenOptions{})
	sid := acpSessionID(t, e, key)
	req, receipt, rerr := h.WakeStart("please SLOW down")
	if rerr != nil || receipt.Disposition != "injected" {
		t.Fatalf("wake receipt %+v err %s", receipt, rpcText(rerr))
	}
	if err := h.Interrupt(); err != nil {
		t.Fatalf("interrupt: %s", rpcText(err))
	}
	ready := h.waitReady(req.RunID, 5*time.Second)
	if ready.State != "done" || ready.Outcome != "interrupted" {
		t.Fatalf("interrupted wake run ready %+v, want done/interrupted", ready)
	}
	cancels := e.recv("session/cancel")
	if len(cancels) != 1 || cancels[0]["sessionId"] != sid {
		t.Fatalf("session/cancel frames %v, want exactly one for %s", cancels, sid)
	}
	if f, ok := h.request("turn.ack", protocol.RunRef{SessionID: h.laneID, RunID: req.RunID}); !ok || f.Error != nil {
		t.Fatalf("turn.ack: %s", rpcText(f.Error))
	}
	mustContain(t, "run after the interrupted wake", completed(t, h.Execute("after")), "after")
}

// The receipt of an answered wake run is `injected` every time (200 wakes per script), never the SDK's
// fallback "native delivery receipt unavailable". The fake widens the race: with noChunks the
// only admission signal is the session/prompt response itself.
func TestWakeReceiptInjectedDeterministic(t *testing.T) {
	// Scheduler pressure widens the window between admission and the receipt report, so a
	// report that can lose to the SDK fallback does lose, also under -race.
	// The pressure is bounded (half the CPUs, yielding, self-stopping): unbounded 2×NumCPU
	// busy loops starved the rest of the suite into its timeout at GOMAXPROCS=16 under -race.
	stop := make(chan struct{})
	defer close(stop)
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < max(2, runtime.NumCPU()/2); i++ {
		go func() {
			for time.Now().Before(deadline) {
				select {
				case <-stop:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
	}
	for _, script := range []string{`{"noChunks":true}`, `{"chunks":["a","b"]}`} {
		e := newEnv(t)
		e.script = script
		h := e.start()
		h.mustOpen(kit.OpenOptions{})
		for i := 0; i < 200; i++ {
			_, receipt, rerr, st := h.Wake(fmt.Sprintf("message %d", i))
			if rerr != nil || receipt.Disposition != "injected" {
				t.Fatalf("script %s, wake %d: receipt %+v err %s", script, i, receipt, rpcText(rerr))
			}
			completed(t, st)
		}
	}
}

// When the ACP child died between turns, a wake prompt is never sent; the receipt is
// the definite rejected/not_submitted, never the uncertain -32603.
func TestWakeToDeadChildNotSubmitted(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	completed(t, h.Execute("bye EXIT_AFTER"))
	eventually(t, "the child to exit", func() bool { return len(e.events("exit_after_reply")) == 1 })
	time.Sleep(300 * time.Millisecond) // let the worker's reader see EOF
	prompts := len(e.events("prompt"))
	_, receipt, rerr := h.WakeStart("hello")
	if rerr != nil && rerr.Message == "connection closed" {
		t.Logf("lane shut down after its child died; the daemon keeps the message")
		return
	}
	if rerr != nil || receipt.Disposition != "rejected" || receipt.Reason != "not_submitted" {
		t.Fatalf("wake to a dead child: receipt %+v err %s, want rejected/not_submitted", receipt, rpcText(rerr))
	}
	if n := len(e.events("prompt")); n != prompts {
		t.Fatalf("a prompt reached a child after it died")
	}
}

// The prompt was written, then the child died before any update or response:
// admission is unknown, so the receipt is the -32603 uncertain_native_admission (the protocol).
func TestWakeUncertainAdmission(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	_, receipt, rerr := h.WakeStart("go CRASH_NOW")
	if rerr == nil || rerr.Code != protocol.Internal || !strings.Contains(string(rerr.Data), "uncertain_native_admission") {
		t.Fatalf("receipt %+v err %s, want -32603 uncertain_native_admission", receipt, rpcText(rerr))
	}
	if len(e.events("crash")) != 1 {
		t.Fatal("the prompt never reached the child")
	}
}

// sessions.list {search:key} is a substring match, so rows derived from
// the lane key come back too; the drift check uses only the row whose key equals the lane key.
func TestResumeCwdExactRow(t *testing.T) {
	open := func(t *testing.T, e *testEnv) (string, string) {
		sub := e.mkdir(e.root("lane"), "sub")
		h := e.start()
		key := h.mustOpen(kit.OpenOptions{Cwd: sub})
		completed(t, h.Execute("hi"))
		h.Close()
		return key, sub
	}
	t.Run("decoys with another cwd around a matching row", func(t *testing.T) {
		e := newEnv(t)
		key, sub := open(t, e)
		e.script = `{"decoyCwd":"/decoy"}`
		h := e.start()
		h.mustResume(key, kit.OpenOptions{Cwd: sub})
		if got := completed(t, h.Execute("EXEC:pwd")); got != sub {
			t.Fatalf("pwd after resume %q, want %s", got, sub)
		}
	})
	t.Run("drifted row with matching decoys", func(t *testing.T) {
		e := newEnv(t)
		key, sub := open(t, e)
		e.script = `{"decoyCwd":"=original","spawnedCwd":{"` + key + `":"/elsewhere"}}`
		mark := e.mark()
		_, err := e.start().Resume(key, kit.OpenOptions{Cwd: sub})
		mustContain(t, "resume error", rpcText(err), "lane cwd changed since creation", "/elsewhere")
		if n := len(e.since(mark, "start")); n != 0 {
			t.Fatalf("ACP started %d time(s)", n)
		}
	})
}

// An answer that already fits the protocol's character limit (MaxTextRunes) and the
// frame is returned whole: no truncation flag, no note, byte-identical.
func TestRunResultAtCharLimitIntact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		runes int
		unit  string
	}{
		{"ascii at the limit", protocol.MaxTextRunes, "a"},
		{"ascii 10 under", protocol.MaxTextRunes - 10, "a"},
		{"ascii 44 under", 262100, "a"},
		{"2-byte 44 under", 262100, "é"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			b, _ := json.Marshal(map[string]any{"bigReplyBytes": tc.runes * len(tc.unit), "bigReplyUnit": tc.unit})
			e.script = string(b)
			h := e.start()
			h.mustOpen(kit.OpenOptions{})
			st := h.Execute("big")
			text := completed(t, st)
			if st.Result.Truncated || truncRe.MatchString(text) {
				t.Fatalf("%d-character answer marked truncated (kept %d characters)", tc.runes, len([]rune(text)))
			}
			if text != strings.Repeat(tc.unit, tc.runes) {
				t.Fatalf("answer changed: %d characters returned, want %d", len([]rune(text)), tc.runes)
			}
		})
	}
}

// Containment uses the realpath of the declared root (a root declared through a
// symlink still contains its own subdirectories), and a cwd with NUL is refused as not absolute.
func TestCwdDeclaredRootSymlink(t *testing.T) {
	e := newEnv(t)
	real := e.root("lane")
	sub := e.mkdir(real, "sub")
	link := filepath.Join(e.dir, "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e.agents[0].Workspace = link
	for _, cwd := range []string{sub, filepath.Join(link, "sub"), link} {
		h := e.start()
		h.mustOpen(kit.OpenOptions{Cwd: cwd})
		h.Close()
	}
	creates := e.gatewayCalls("sessions.create")
	if len(creates) != 3 || params(creates[0])["cwd"] != sub || params(creates[1])["cwd"] != sub || params(creates[2])["cwd"] != real {
		t.Fatalf("sessions.create calls %v, want realpaths %s, %s, %s", creates, sub, sub, real)
	}
	outside := e.mkdir(e.dir, "outside")
	mark := e.mark()
	_, err := e.start().Open(kit.OpenOptions{Cwd: outside})
	mustContain(t, "open error", rpcText(err), "outside the workspace of agent lane")
	if n := len(e.since(mark, "")); n != 0 {
		t.Fatalf("refused open touched the native side")
	}
}

func TestCwdNulRejected(t *testing.T) {
	e := newEnv(t)
	for _, cwd := range []string{e.root("lane") + "/a\x00b", "\x00" + e.root("lane")} {
		_, err := e.start().Open(kit.OpenOptions{Cwd: cwd})
		mustContain(t, "open error", rpcText(err), "(must be absolute)")
	}
	e.noNativeWork(t)
}

// Bridge loss refuses native work; explicit close retires the idle SDK worker.
func TestBridgeLossIdleClosesNative(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	prompts := len(e.events("prompt"))
	e.bridge.Drop()
	eventually(t, "native session closure", func() bool { return len(e.events("session_close")) == 1 })
	h.Close()
	if n := len(e.events("prompt")); n != prompts {
		t.Fatalf("prompts grew %d -> %d after bridge loss", prompts, n)
	}
}

// Mid-run native cleanup waits for the terminal; explicit close retires the worker.
func TestBridgeLossMidRunClosesNativeAfterTerminal(t *testing.T) {
	e := newEnv(t)
	h := e.start()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW work")
	eventually(t, "the prompt to reach the child", func() bool { return len(e.events("prompt")) == 1 })
	e.bridge.Drop()
	select {
	case <-h.gone:
		t.Fatal("worker exited while its run was live")
	case <-time.After(500 * time.Millisecond):
	}
	if rerr := h.Interrupt(); rerr != nil {
		t.Fatalf("interrupt: %v", rerr)
	}
	h.Wait(run)
	eventually(t, "native session closure", func() bool { return len(e.events("session_close")) == 1 })
	h.Close()
	if n := len(e.events("prompt")); n != 1 {
		t.Fatalf("prompts after bridge loss: %d, want 1", n)
	}
}

// CR-L3-2: when the turn simply ends while the steer is being retried, the answer is NotRunning
// (the normal D13 path) and the "steer fallback (not BUSY-MID)" line — a smoke FAIL marker — is
// not printed.
func TestDeliverTurnEndedDuringRetriesPrintsNoFallback(t *testing.T) {
	e := newEnv(t)
	e.steer = "false"
	h := e.startBinary()
	h.mustOpen(kit.OpenOptions{})
	run := h.Start("SLOW")
	eventually(t, "the SLOW prompt", func() bool { return len(e.callerPrompts()) == 1 })
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = h.Interrupt()
	}()
	f := h.Deliver("while it ends")
	if f.Error == nil || f.Error.Code != protocol.NotRunning {
		t.Fatalf("answer %s / %s, want not_running", f.Result, rpcText(f.Error))
	}
	h.Wait(run)
	time.Sleep(150 * time.Millisecond)
	if strings.Contains(h.workerStderr(), "steer fallback") {
		t.Fatalf("fallback line printed for a turn that ended; stderr %q", h.workerStderr())
	}
	if n := len(steerEvents(e)); n < 2 || n > 3 {
		t.Fatalf("steer frames %d, want the turn to end after 1-2 retries", n)
	}
}
