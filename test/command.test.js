import assert from "node:assert/strict";
import { cpSync, mkdtempSync, rmSync, symlinkSync, writeFileSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import test from "node:test";
import kit from "@sessionbus/kit";
import { createFakeDaemon } from "./support/fake-daemon.js";
import { createFakeRuntime, createStrictLogger } from "./support/fake-openclaw.js";
import { createPeerService } from "../src/service.js";
import { createSessionbusCommand, createSessionbusInteractiveHandler } from "../src/command.js";

const OWNER = { senderIsOwner: true, agentId: "advisor" };

function row(session_id, extra = {}) {
  return { session_id, kind: "peer", product: "openclaw-peer", groups: ["g"], connected: true, running: false, ...extra };
}

function fakeService({ list, peer = true, binding = { agentId: "advisor", name: "advisor", groups: ["openclaw-peer"] } } = {}) {
  const calls = [];
  const fake = {
    started: true,
    runtime: { version: "2026.9.8" },
    peerFor: (agentId) =>
      peer && agentId === "advisor"
        ? { action: async (action, args, signal) => { calls.push({ action, args, signal }); return typeof list === "function" ? list() : list; } }
        : undefined,
    bindingFor: (agentId) => (agentId === "advisor" ? binding : undefined),
  };
  return { service: fake, calls };
}

const run = (service, args, ctx = OWNER) => createSessionbusCommand(service).handler({ ...ctx, args });

test("roster renders a host-sorted markdown table as the agent sees the bus", async () => {
  const list = {
    self_info: { session_id: "openclaw-advisor@host-b", product: "openclaw-peer", groups: ["g"] },
    sessions: [
      row("openclaw-peer@host-a", { name: "openclaw-peer@host-a", product: "claude-peer", running: true }),
      row("busymid-lane-host-b@host-b", { kind: "lane", name: "busymid-lane-host-b@host-b", product: "openclaw-peer", running: true }),
      row("openclaw-advisor@host-b", { name: "advisor@host-b" }),
      row("zed@host-b", { name: "zed@host-b", connected: false, product: "codex-peer" }),
    ],
  };
  const { service, calls } = fakeService({ list });
  const result = await run(service, "roster");
  assert.equal(result.isError, undefined);
  assert.equal(
    result.text,
    [
      "**sessionbus roster** · advisor · 4 sessions",
      "",
      "| HOST | KIND | STATE | PRODUCT | NAME |",
      "|---|---|---|---|---|",
      "| host-b | peer | ○ idle | openclaw-peer | advisor (self) |",
      "| host-b | peer | ✕ disconnected | codex-peer | zed |",
      "| host-b | lane | ● running | openclaw-peer | busymid-lane-host-b |",
      "| host-a | peer | ● running | claude-peer | openclaw-peer |",
    ].join("\n"),
  );
  assert.deepEqual(calls.map((call) => [call.action, call.args]), [["list", {}]]);
  assert.ok(calls[0].signal instanceof AbortSignal);
});

test("roster cells escape markdown and never leak info or session keys", async () => {
  const list = {
    self_info: { session_id: "me@h", product: "openclaw-peer", groups: ["secret-group"] },
    sessions: [
      row("evil@h", {
        name: "a|b*c_d`e[f]<g>\\h\nnext\u0007x@h",
        product: "x|y-peer",
        info: { session_key: "agent:advisor:SECRET", openclaw_version: "9" },
        groups: ["secret-group"],
        policy: { max: 1 },
      }),
    ],
  };
  const { service } = fakeService({ list });
  const { text } = await run(service, "roster");
  assert.match(text, /\| h \| peer \| ○ idle \| x\\\|y-peer \| a\\\|b\\\*c\\_d\\`e\\\[f\\\]\\<g\\>\\\\h next x \|/);
  assert.equal(text.split("\n").length, 5);
  for (const secret of ["SECRET", "secret-group", "session_key", "openclaw_version", "max"]) assert.ok(!text.includes(secret), secret);
});

test("roster caps the table at 40 rows", async () => {
  const sessions = Array.from({ length: 45 }, (_, i) => row(`p${String(i).padStart(2, "0")}@h`, { name: `p${String(i).padStart(2, "0")}@h` }));
  const { service } = fakeService({ list: { self_info: { session_id: "p00@h" }, sessions } });
  const { text } = await run(service, "roster");
  const lines = text.split("\n");
  assert.equal(lines.filter((line) => line.startsWith("| h |")).length, 40);
  assert.ok(text.includes("| h | peer | ○ idle | openclaw-peer | p39 |"));
  assert.ok(!text.includes("p40"));
  assert.equal(lines.at(-1), "… +5 more");
  assert.equal(lines.at(-2), "");
  assert.match(lines[0], /45 sessions/);
});

test("roster for an agent without a bus peer says it is not connected", async () => {
  const { service, calls } = fakeService({ list: {}, peer: false });
  const result = await run(service, "roster");
  assert.equal(result.isError, true);
  assert.equal(result.text, "sessionbus: agent advisor not connected (plugins.entries.sessionbus.config.agents.advisor.enabled)");
  assert.deepEqual(calls, []);
});

test("a failed list becomes a short error reply", async () => {
  const { service } = fakeService({ list: () => { throw new Error(`not connected\n${"x".repeat(500)}`); } });
  const result = await run(service, "roster");
  assert.equal(result.isError, true);
  assert.match(result.text, /^sessionbus: bus unavailable: not connected x{100,}/);
  assert.ok(!result.text.includes("\n"));
  assert.ok(result.text.length < 260);
  const status = await run(service, "status");
  assert.match(status.text, /- session: openclaw-advisor · error: not connected/);
});

test("status reports identity, groups and connection", async () => {
  const list = { self_info: { session_id: "openclaw-advisor@host-b", product: "openclaw-peer", groups: ["x"] }, sessions: [row("a@host-b"), row("b@host-b"), row("c@h"), row("d@h")] };
  const { service } = fakeService({ list });
  const result = await createSessionbusCommand(service, { buildInfo: null }).handler({ ...OWNER, args: " STATUS " });
  assert.equal(
    result.text,
    [
      "**sessionbus status** · advisor",
      "- session: openclaw-advisor@host-b · connected",
      "- name: advisor",
      "- groups: openclaw-peer",
      "- sessions: 4",
      "- plugin: 0.2.1 (build unknown)",
      "- OpenClaw: 2026.9.8",
    ].join("\n"),
  );
});

test("status shows the plugin build", async () => {
  const sha = "ca426d2025d9aaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  const { service } = fakeService({ list: {} });
  const line = async (buildInfo) => (await createSessionbusCommand(service, { buildInfo }).handler({ ...OWNER, args: "status" })).text
    .split("\n").find((text) => text.startsWith("- plugin:"));
  assert.equal(await line({ version: "0.2.0", release: "v0.2.0", revision: sha }), "- plugin: v0.2.0 (ca426d2025d9)");
  assert.equal(await line({ version: "0.2.0", release: "development", revision: sha }), "- plugin: 0.2.0 development (ca426d2025d9)");
  assert.equal(await line(null), "- plugin: 0.2.1 (build unknown)");
  assert.equal(await line({ version: "0.2.0", release: "v0.2.0\n| x", revision: "*bold*" }), "- plugin: v0.2.0 \\| x (\\*bold\\*)");
});

test("menu buttons are callbacks on telegram and commands elsewhere", async () => {
  const { service } = fakeService({ list: {} });
  const buttonsOf = async (channel) => (await run(service, "", { ...OWNER, channel })).presentation.blocks.find((block) => block.type === "buttons").buttons;
  assert.deepEqual(
    (await buttonsOf("telegram")).map((button) => [button.label, button.action]),
    [
      ["👥 Roster", { type: "callback", value: "sessionbus:roster" }],
      ["📡 Status", { type: "callback", value: "sessionbus:status" }],
      ["❓ Help", { type: "callback", value: "sessionbus:help" }],
    ],
  );
  for (const channel of ["discord", undefined]) {
    assert.deepEqual(
      (await buttonsOf(channel)).map((button) => button.action),
      [
        { type: "command", command: "/sessionbus roster" },
        { type: "command", command: "/sessionbus status" },
        { type: "command", command: "/sessionbus help" },
      ],
    );
  }
});

test("the telegram button handler resubmits the subcommand as text and ignores unknown payloads", async () => {
  const entry = createSessionbusInteractiveHandler();
  assert.equal(entry.channel, "telegram");
  assert.equal(entry.namespace, "sessionbus");
  const press = (payload) => entry.handler({ callback: { payload } });
  assert.deepEqual(await press("roster"), { submitText: "/sessionbus roster" });
  assert.deepEqual(await press(" Status "), { submitText: "/sessionbus status" });
  assert.deepEqual(await press("help"), { submitText: "/sessionbus help" });
  for (const payload of ["reset", "roster extra", "", undefined]) assert.deepEqual(await press(payload), { handled: true });
  assert.deepEqual(await entry.handler({}), { handled: true });
});

test("no argument returns a button menu with a text fallback", async () => {
  const { service, calls } = fakeService({ list: {} });
  const result = await run(service, "");
  assert.deepEqual(result.presentation, {
    title: "sessionbus",
    blocks: [
      { type: "text", text: "/sessionbus roster · status · help" },
      {
        type: "buttons",
        buttons: [
          { label: "👥 Roster", action: { type: "command", command: "/sessionbus roster" } },
          { label: "📡 Status", action: { type: "command", command: "/sessionbus status" } },
          { label: "❓ Help", action: { type: "command", command: "/sessionbus help" } },
        ],
      },
    ],
  });
  assert.equal(result.presentationTextMode, "fallback");
  assert.equal(result.text, "**sessionbus**\n/sessionbus roster\n/sessionbus status\n/sessionbus help");
  assert.equal((await run(service, undefined)).presentation.title, "sessionbus");
  assert.deepEqual(calls, []);
});

test("help and unknown list the subcommands", async () => {
  const { service } = fakeService({ list: {} });
  const help = await run(service, "help");
  assert.equal(
    help.text,
    [
      "**sessionbus**",
      "- /sessionbus roster — peers on the bus",
      "- /sessionbus status — this agent's connection",
      "- /sessionbus help — this help",
    ].join("\n"),
  );
  const unknown = await run(service, "Frob_it now");
  const [first, ...rest] = unknown.text.split("\n");
  assert.equal(first, "sessionbus: unknown subcommand \"frob\\_it\"");
  assert.equal(rest.join("\n"), help.text);
});

test("a sender that is not the owner gets a refusal and no bus call", async () => {
  const { service, calls } = fakeService({ list: {} });
  for (const ctx of [{ agentId: "advisor" }, { agentId: "advisor", senderIsOwner: false }, { agentId: "advisor", senderIsOwner: "true" }]) {
    for (const args of ["roster", "status", "help", ""]) {
      const result = await run(service, args, ctx);
      assert.deepEqual(result, { text: "sessionbus: owner only", isError: true });
    }
  }
  assert.deepEqual(calls, []);
});

test("command definition fits OpenClaw and Telegram menu rules", () => {
  const command = createSessionbusCommand({});
  assert.match(command.name, /^[a-z0-9_]{1,32}$/);
  assert.equal(command.name, "sessionbus");
  assert.equal(command.acceptsArgs, true);
  assert.deepEqual(command.requiredScopes, ["operator.read"]);
  assert.ok(command.description.length <= 40);
  assert.equal(typeof command.handler, "function");
});

test("roster through a started service lists via the chat agent's peer", async (t) => {
  const list = () => ({
    self_info: { session_id: "openclaw-advisor@testhost", product: "openclaw-peer", groups: ["g"] },
    sessions: [row("openclaw-advisor@testhost", { name: "advisor@testhost" }), row("bob@testhost", { name: "bob@testhost", running: true })],
  });
  const daemon = createFakeDaemon({ list });
  const runtime = createFakeRuntime({
    version: "2026.8.2",
    config: {
      agents: { entries: { advisor: {} } },
      plugins: { entries: { sessionbus: { config: { socket: "/tmp/fake.sock", agents: { advisor: { enabled: true, groups: ["g"] } } } } } },
    },
  });
  const service = createPeerService({ runtime }, { connect: (path) => daemon.connect(path), env: {}, uid: 1000 });
  service.start({ logger: createStrictLogger() });
  t.after(() => service.stop());
  await service.peerFor("advisor").ready;

  const result = await run(service, "roster");
  assert.equal(daemon.lists.length, 1);
  assert.match(result.text, /^\*\*sessionbus roster\*\* · advisor · 2 sessions\n\n\| HOST/);
  assert.match(result.text, /\| testhost \| peer \| ○ idle \| openclaw-peer \| advisor \(self\) \|/);
  assert.match(result.text, /\| testhost \| peer \| ● running \| openclaw-peer \| bob \|/);
  assert.equal(daemon.hellos[0].session_id, "openclaw-advisor");

  const other = await run(service, "roster", { ...OWNER, agentId: "nobody" });
  assert.match(other.text, /^sessionbus: agent nobody not connected/);
  assert.equal(daemon.lists.length, 1);
});

test("roster cuts a long participant label to 40 characters and keeps the (self) mark", async () => {
  const long = "x".repeat(60);
  const list = {
    self_info: { session_id: `${long}@h` },
    sessions: [row(`${long}@h`, { name: `${long}@h` }), row("lane1@h", { kind: "lane", name: `${long}@h2` , session_id: "lane1@h" })],
  };
  const { service } = fakeService({ list });
  const { text } = await run(service, "roster");
  assert.ok(text.includes(`| h | peer | ○ idle | openclaw-peer | ${"x".repeat(39)}… (self) |`), text);
  assert.ok(text.includes(`| h | lane | ○ idle | openclaw-peer | ${"x".repeat(39)}… |`), text);
});

test("an unstarted service gets a plugin-not-started reply, not a misleading not-connected one", async () => {
  const service = { started: false, peerFor: () => undefined };
  const result = await run(service, "roster");
  assert.deepEqual(result, { text: "sessionbus: plugin not started", isError: true });
});

test("a protocol error code prefixes the message in roster and status", async () => {
  const failing = new kit.ProtocolError({ code: -32001, message: "daemon down" });
  const { service } = fakeService({ list: () => { throw failing; } });
  assert.equal((await run(service, "roster")).text, "sessionbus: bus unavailable: -32001: daemon down");
  assert.match((await run(service, "status")).text, /· error: -32001: daemon down\n/);
});

test("a timeout or socket error carries no code prefix", async () => {
  for (const failing of [
    new DOMException("The operation was aborted due to timeout", "TimeoutError"),
    Object.assign(new Error("connect ENOENT /x.sock"), { code: "ENOENT" }),
  ]) {
    const { service } = fakeService({ list: () => { throw failing; } });
    assert.equal((await run(service, "roster")).text, `sessionbus: bus unavailable: ${failing.message}`);
    assert.ok((await run(service, "status")).text.includes(`· error: ${failing.message}\n`));
  }
});

test("status reads the build-info.json shipped next to the module", async () => {
  const root = path.dirname(path.dirname(new URL(import.meta.url).pathname));
  const dir = mkdtempSync(path.join(tmpdir(), "status-build-"));
  try {
    // the package test packs the same tree concurrently; prepack may drop a build-info.json there
    cpSync(path.join(root, "src"), path.join(dir, "src"), { recursive: true, filter: (source) => path.basename(source) !== "build-info.json" });
    cpSync(path.join(root, "package.json"), path.join(dir, "package.json"));
    symlinkSync(realpathSync(path.join(root, "node_modules")), path.join(dir, "node_modules"));
    const statusLine = async () => {
      const module = await import(`${pathToFileURL(path.join(dir, "src/command.js")).href}?t=${Math.random()}`);
      const { service } = fakeService({ list: {} });
      return (await module.createSessionbusCommand(service).handler({ ...OWNER, args: "status" })).text.split("\n").find((text) => text.startsWith("- plugin:"));
    };
    assert.match(await statusLine(), /^- plugin: \d+\.\d+\.\d+ \(build unknown\)$/);
    writeFileSync(path.join(dir, "src/build-info.json"), JSON.stringify({ version: "0.2.0", release: "v0.2.0", revision: "ca426d2025d9aaaaaaaa" }));
    assert.equal(await statusLine(), "- plugin: v0.2.0 (ca426d2025d9)");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("no Cyrillic in any reply", async () => {
  const { service } = fakeService({ list: {
    self_info: { session_id: "openclaw-advisor@h" },
    sessions: [row("openclaw-advisor@h", { name: "advisor@h" })],
  } });
  const replies = [];
  for (const args of ["", "help", "frob", "roster", "status"]) {
    replies.push(await run(service, args));
  }
  replies.push(await run(service, "roster", { ...OWNER, senderIsOwner: false }));
  replies.push(await run({ started: false, peerFor: () => undefined }, "roster"));
  const { service: disconnected } = fakeService({ peer: false });
  replies.push(await run(disconnected, "roster"));
  const { service: failing } = fakeService({ list: () => { throw new Error("daemon down"); } });
  for (const args of ["roster", "status"]) replies.push(await run(failing, args));
  for (const reply of replies) {
    assert.doesNotMatch(reply.text, /[\u0410-\u042f\u0430-\u044f\u0401\u0451]/);
    assert.doesNotMatch(JSON.stringify(reply.presentation) ?? "", /[\u0410-\u042f\u0430-\u044f\u0401\u0451]/);
  }
});

test("roster product and kind cells are cleaned and cut like other bus values; a missing kind shows ?", async () => {
  const list = {
    self_info: { session_id: "me@h" },
    sessions: [
      row("a@h", { name: "a@h", product: "claude-peer" }),
      row("b@h", { name: "b@h", kind: "pe\ner|x", product: "bad\nline\u0007-peer" }),
      row("c@h", { name: "c@h", product: "p".repeat(60) }),
      row("d@h", { name: "d@h", kind: "k".repeat(60) }),
      row("e@h", { name: "e@h", kind: undefined }),
    ],
  };
  const { service } = fakeService({ list });
  const { text } = await run(service, "roster");
  assert.ok(text.includes("| h | peer | ○ idle | claude-peer | a |"), text);
  assert.ok(text.includes("| h | pe er\\|x | ○ idle | bad line -peer | b |"), text);
  assert.ok(text.includes(`| h | peer | ○ idle | ${"p".repeat(39)}… | c |`), text);
  assert.ok(text.includes(`| h | ${"k".repeat(39)}… | ○ idle | openclaw-peer | d |`), text);
  assert.ok(text.includes("| h | ? | ○ idle | openclaw-peer | e |"), text);
  assert.equal(text.split("\n").length, 9);
});
