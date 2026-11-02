import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, readdirSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { Compile } from "typebox/schema";
import { createFakeHost } from "./support/fake-openclaw.js";
import { loadPluginEntry } from "./support/openclaw-sdk.js";

const ROOT = path.dirname(path.dirname(fileURLToPath(import.meta.url)));

function readJson(relativePath) {
  return JSON.parse(readFileSync(path.join(ROOT, relativePath), "utf8"));
}

const pkg = readJson("package.json");
const lockfile = readJson("package-lock.json");
const manifest = readJson("openclaw.plugin.json");

test("package name is openclaw-sessionbus-peer", () => {
  assert.equal(pkg.name, "openclaw-sessionbus-peer");
});

test("package metadata points to the owner repository", () => {
  assert.deepEqual(pkg.repository, { type: "git", url: "git+https://github.com/imortkz/openclaw-peer.git" });
  assert.equal(pkg.homepage, "https://github.com/imortkz/openclaw-peer#readme");
  assert.deepEqual(pkg.bugs, { url: "https://github.com/imortkz/openclaw-peer/issues" });
});

test("package declares MIT and the root license contains its text", () => {
  assert.equal(pkg.license, "MIT");
  assert.equal(lockfile.packages[""].license, "MIT");
  assert.ok(readFileSync(path.join(ROOT, "LICENSE"), "utf8").startsWith("MIT License\n"));
});

test("package and lockfile declare RELEASE_VERSION", () => {
  const release = readFileSync(path.join(ROOT, "RELEASE_VERSION"), "utf8").trim();
  assert.match(release, /^\d+\.\d+\.\d+$/);
  assert.equal(pkg.version, release);
  assert.equal(lockfile.version, release);
  assert.equal(lockfile.packages[""].version, release);
});

test("openclaw.compat.pluginApi targets >=2026.8.2", () => {
  assert.equal(pkg.openclaw.compat.pluginApi, ">=2026.8.2");
});

test("engines.node matches the 8.2 range", () => {
  assert.equal(pkg.engines.node, ">=22.22.3 <23 || >=24.15.0 <25 || >=25.9.0");
});

test("every openclaw.extensions entry exists on disk", () => {
  assert.ok(pkg.openclaw.extensions.length > 0);
  for (const entry of pkg.openclaw.extensions) {
    assert.ok(existsSync(path.join(ROOT, entry)), `${entry} should exist`);
  }
});

test("manifest id is sessionbus", () => {
  assert.equal(manifest.id, "sessionbus");
});

// OpenClaw 8.2 refuses registerTool unless the manifest declares the tool name in contracts.tools
// (src/plugins/registry-registrars-tools-hooks.ts:219-243) — without it the agent never sees the tool.
// contracts.tools is ownership, not a host-trusted tier; nothing else may be declared.
test("manifest contracts declare exactly the sessionbus tool and nothing host-trusted", () => {
  assert.deepEqual(manifest.contracts, { tools: ["sessionbus"] });
});

test("every tool name registered in index.js is declared in contracts.tools", () => {
  const source = readFileSync(path.join(ROOT, "index.js"), "utf8");
  const registered = [...source.matchAll(/registerTool\(.*?\{\s*name:\s*"([^"]+)"/g)].map((m) => m[1]);
  assert.ok(registered.length > 0, "index.js should register at least one named tool");
  for (const name of registered) assert.ok(manifest.contracts?.tools?.includes(name), `${name} must be in contracts.tools`);
});

test("index.js registers the sessionbus chat command", async () => {
  const registration = createFakeHost({}).register(await loadPluginEntry());
  assert.deepEqual(registration.commands.map((command) => command.name), ["sessionbus"]);
});

test("index.js registers the sessionbus telegram button handler", async () => {
  const registration = createFakeHost({}).register(await loadPluginEntry());
  assert.deepEqual(registration.interactiveHandlers.map((entry) => [entry.channel, entry.namespace]), [["telegram", "sessionbus"]]);
  assert.deepEqual(await registration.interactiveHandlers[0].handler({ callback: { payload: "roster" } }), { submitText: "/sessionbus roster" });
});

test("configSchema, compiled with typebox's own Compile, accepts valid configs", () => {
  const validator = Compile(manifest.configSchema);
  assert.equal(
    validator.Check({
      agents: { advisor: { enabled: true, groups: ["g"], sessionKey: "agent:advisor:telegram:direct:123456789" } },
    }),
    true,
  );
  assert.equal(validator.Check({ socket: "/tmp/s.sock", agents: { home: { enabled: false } } }), true);
});

test("configSchema rejects an unknown root key", () => {
  const validator = Compile(manifest.configSchema);
  assert.equal(validator.Check({ bogus: true }), false);
});

test("configSchema rejects relative socket and lane paths", () => {
  const validator = Compile(manifest.configSchema);
  for (const value of [
    { socket: "relative.sock" },
    { lanes: { agents: { advisor: {} }, credentialDir: "relative" } },
    { lanes: { agents: { advisor: {} }, credentialDir: "/tmp/creds", auditLog: "relative" } },
  ]) assert.equal(validator.Check(value), false, JSON.stringify(value));
});

test("configSchema rejects an unknown per-agent field", () => {
  const validator = Compile(manifest.configSchema);
  assert.equal(validator.Check({ agents: { advisor: { enabled: true, bogus: true } } }), false);
});

test("configSchema accepts an enabled agent and rejects its removed legacy setting", () => {
  const validator = Compile(manifest.configSchema);
  const agent = { enabled: true, groups: ["g"] };
  assert.equal(validator.Check({ agents: { advisor: agent } }), true);
  assert.equal(validator.Check({ agents: { advisor: { ...agent, mirror: true } } }), false);
});

test("configSchema rejects a non-array groups", () => {
  const validator = Compile(manifest.configSchema);
  assert.equal(validator.Check({ agents: { advisor: { enabled: true, groups: "g" } } }), false);
});

test("kit is pinned to exactly 0.5.12 in package.json and the lockfile", () => {
  assert.equal(pkg.dependencies["@sessionbus/kit"], "0.5.12");
  assert.equal(lockfile.packages["node_modules/@sessionbus/kit"].version, "0.5.12");
});

// Identifiers that would indicate this plugin bypasses an OpenClaw trust
// boundary instead of working inside it: an opt-out from the
// external-untrusted-content wrapper, a hook that can dispatch an agent
// turn directly, a pairing-approval shortcut, or a prompt-injection
// permission flag. None of these belong in src/ — the plugin only uses
// enqueueSystemEvent + requestHeartbeat + agent_end, all reachable without
// touching any of them.
const BYPASS_IDENTIFIERS = ["EXTERNAL_UNTRUSTED_CONTENT", "before_agent_run", "approveDevicePairing", "allowPromptInjection"];

function collectFiles(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) out.push(...collectFiles(full));
    else if (entry.endsWith(".js")) out.push(full);
  }
  return out;
}

test("src/ contains none of the OpenClaw trust-boundary bypass identifiers", () => {
  const files = collectFiles(path.join(ROOT, "src"));
  assert.ok(files.length > 0);
  for (const file of files) {
    const content = readFileSync(file, "utf8");
    for (const identifier of BYPASS_IDENTIFIERS) {
      assert.equal(content.includes(identifier), false, `${path.relative(ROOT, file)} should not contain "${identifier}"`);
    }
  }
});

test("configSchema rejects the removed laneVoice and lanes.voiceReadiness keys (#11)", () => {
  const props = manifest.configSchema.properties;
  assert.equal("laneVoice" in props, false);
  assert.equal("voiceReadiness" in props.lanes.properties, false);
  const validator = Compile(manifest.configSchema);
  assert.equal(validator.Check({ lanes: { agents: { advisor: {} }, credentialDir: "/c" } }), true);
  assert.equal(validator.Check({ laneVoice: { enabled: true, dir: "/old" } }), false);
  assert.equal(validator.Check({ lanes: { agents: { advisor: {} }, credentialDir: "/c", voiceReadiness: "eager" } }), false);
});

test("npm pack runs build-info before packing and the file is git-ignored", () => {
  assert.equal(pkg.scripts.prepack, "node scripts/build-info.mjs");
  assert.ok(pkg.files.includes("src"));
  assert.ok(readFileSync(path.join(ROOT, ".gitignore"), "utf8").split("\n").includes("/src/build-info.json"));
});

test("release packaging passes release and revision to npm pack", () => {
  const script = readFileSync(path.join(ROOT, "scripts/package"), "utf8");
  assert.match(script, /OPENCLAW_PEER_RELEASE="\$release" OPENCLAW_PEER_REVISION="\$revision" npm pack/);
});

test("npm pack ships src/build-info.json but leaves no copy in the source tree (postpack)", () => {
  assert.equal(pkg.scripts.postpack, "node scripts/build-info.mjs --clean");
  const dir = mkdtempSync(path.join(tmpdir(), "pack-"));
  try {
    execFileSync("npm", ["pack", "--pack-destination", dir, "--silent"], { cwd: ROOT, stdio: "pipe", env: { ...process.env, npm_config_cache: path.join(dir, "cache") } });
    const tgz = readdirSync(dir).find((name) => name.endsWith(".tgz"));
    assert.ok(tgz);
    const listing = execFileSync("tar", ["-tzf", path.join(dir, tgz)], { encoding: "utf8" }).split("\n");
    assert.ok(listing.includes("package/src/build-info.json"));
    assert.equal(existsSync(path.join(ROOT, "src/build-info.json")), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
