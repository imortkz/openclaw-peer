# OpenClaw peer — plugin guide

The full operator guide for the `openclaw-sessionbus-peer` plugin. Installation and the short overview are in the
[README](../README.md); the lane worker has its own [guide](../lane/README.md).

Connects an OpenClaw agent to [Sessionbus](https://github.com/antst/sessionbus) as an
ordinary peer, on the same footing as `claude-peer` / `codex-peer` / `opencode-peer`, through a
`sessionbus` channel of its own. Off by default, per agent.

Diagrams of the plugin and the lane worker: [docs/ARCHITECTURE.md](ARCHITECTURE.md).

## Behaviour

- **A bus message is a channel message.** The plugin registers a `sessionbus` channel. A message is dispatched
  into a bus session `agent:<id>:sessionbus:direct:<peer>`, chosen by `busSessionBinding`: `name` (default) gives
  one session per sender name, compared case-insensitively, so peers with the same name share it and a restarted
  peer keeps its context ('%', ':' and whitespace in a name are encoded as `%XXXX`; a sender without a name falls
  back to its session_id); `sessionId` gives one session per sender session, the behaviour before 0.2.0;
  `singleton` gives one bus session per agent, `…:direct:%all`. Changing the value starts new sessions; existing
  ones are left as they are, not merged. If the agent is idle it starts a turn at once (no heartbeat involved). If
  a turn of that session is already running, OpenClaw steers the new message into it, so the agent sees it before
  the current task ends. The agent replies with the sessionbus tool to the sender's session_id shown in the
  message. The owner's Telegram or main session, and the global `session.dmScope`, are unchanged.
- **Receipts.** The sender gets `queued_for_next_turn` once the dispatch has started, or `rejected` with a
  reason: message over the 64 KiB inbound limit, loop guard, 16 unfinished dispatches for the agent, the channel
  not enabled or not started, or a route that resolves to another agent. `queued_for_next_turn` means "will be
  processed", not "read". A retransmission of the same `message_id` gets the original receipt and no second
  turn; refusals are not remembered, so a retransmission of a refused message is evaluated again. Delivery follows
  the Sessionbus
  [`message.deliver` protocol](https://github.com/antst/sessionbus/blob/main/bus/docs/PROTOCOL.md):
  one receipt, no retry or replay, and no later status to the sender. OpenClaw-to-OpenClaw messages over 64 KiB are
  rejected on receipt even though the sending tool advertises a 262144-character wire limit.
- **Explicit reply only.** The channel never delivers the agent's final text anywhere. The agent replies only by
  calling the `sessionbus` tool's `send` action itself, in the same turn or a later one. To write to the owner
  on Telegram from a bus session, see "Telegram from a bus session" below.
- **Bus text is text.** A message such as `/status`, `/model x` or `stop` reaches the agent as ordinary text; it is
  never run as an OpenClaw command, and an operator's `commands.allowFrom` (including `*`) does not apply to bus
  input.
- **Trusted bus content.** Sessionbus is one federation of the owner's own machines — one
  trust boundary, like OpenClaw's own "one trust boundary per gateway". Inbound bus text is
  attributed to its sender and handed to the agent as-is, not wrapped as untrusted external
  content. OpenClaw's own tool policy and sandbox for the turn are unchanged by this plugin.

## Requirements

- OpenClaw ≥ 2026.8.2 (plugin and lane tested on 2026.8.2 and 2026.9.1–2026.9.8).
- On OpenClaw 2026.9.6+, Tool Search is on by default: the agent reaches `sessionbus` through
  tool search. Set `tools.toolSearch: false` to expose it directly.
- Node must meet the installed OpenClaw release's requirement: 2026.8.2 accepts
  `>=22.22.3 <23 || >=24.15.0 <25 || >=25.9.0`; 2026.9.5+ requires
  `>=24.16.0 <25 || >=26.1.0`.
- Sessionbus 0.5.12 (at least 0.5.8 on every host that runs lanes: an older daemon still sends
  `idle_message`, which the worker refuses, so no lane opens).
- The OpenClaw Gateway and the Sessionbus daemon run under the same OS user (the daemon socket is
  reached in-process; no network hop).

## Install

### Prebuilt release

```sh
curl -fsSL https://raw.githubusercontent.com/imortkz/openclaw-peer/main/scripts/install.sh | sh
```

Set `OPENCLAW_PEER_VERSION=v0.2.0` to pin a tag. The installer verifies the archive checksum,
installs the worker in `~/.local/libexec/sessionbus/openclaw` with a `~/.local/bin/openclaw-peer`
link, and places the packed plugin beside it. It prints the plugin install, Gateway restart, and
Sessionbus daemon setup commands; it does not change OpenClaw or `service.env` itself.

### Build from source

### Plugin

From the repository root, pack and install the plugin:

```sh
npm pack
openclaw plugins install ./openclaw-sessionbus-peer-*.tgz
```

The packed `.tgz` is ignored by git. Pack first: a plain directory install skips runtime
dependencies on OpenClaw 8.2. The interactive install asks you to review and accept the plugin's capabilities.
For a scripted, non-interactive install, add `--accept-capabilities` after reviewing the package;
local files have no recorded integrity, so OpenClaw asks again on each install
(OpenClaw 8.2 `docs/plugins/manage-plugins.md:131-147`). Verify the package loaded:

```sh
openclaw plugins inspect sessionbus --runtime --json
```

### Lane worker (optional)

Hosts that offer OpenClaw lanes also need the Go worker. Use Go 1.24 and the vendored modules;
the build needs no network. From the repository root:

```sh
cd lane
mkdir -p "$HOME/.local/bin"
go build -mod=vendor -trimpath \
  -ldflags "-X github.com/sessionbus/peer-common/peerversion.Release=v$(cat ../RELEASE_VERSION) -X github.com/sessionbus/peer-common/peerversion.Revision=$(git rev-parse HEAD)" \
  -o "$HOME/.local/bin/openclaw-peer" ./cmd/openclaw-peer
openclaw-peer --version
```

The version check prints `openclaw-peer v0.2.0 (<commit>)`. Sessionbus's Linux and macOS
installers put `~/.local/bin` on the daemon service's `PATH`; otherwise add the binary's
directory to that service `PATH`. The `openclaw` command must be on the same service `PATH`.
In `${XDG_CONFIG_HOME:-$HOME/.config}/sessionbus/service.env`, append `openclaw-peer` to
the existing product list, for example:

```sh
SESSIONBUS_PRODUCTS=claude-peer,codex-peer,openclaw-peer
```

Restart the Sessionbus daemon with `systemctl --user restart sessionbus` on Linux, or
`launchctl kickstart -k gui/$(id -u)/net.antst.sessionbus` on macOS. Then follow
[Pairing the device](../lane/README.md#pairing-the-device-install-step) and
[Rollout checks](../lane/README.md#rollout-checks) for the worker credential. Set `lanes` and the channel as shown
in [Configure](#configure) below. If the Gateway or the daemon uses a non-default bridge path, set
`SESSIONBUS_OPENCLAW_BRIDGE` (an absolute path) to the same value in both environments.

### First message

After [Configure](#configure), start a Sessionbus session in the agent peer's group and use its
`sessionbus` tool to `send` a message to that peer. The agent should wake and can reply with
the same tool; see [Tool](#tool) for delivery behaviour.

## Configure

Enable the plugin for one agent at a time under `plugins.entries.sessionbus.config`:

```json5
{
  agents: {
    entries: {
      advisor: {
        // For coding or messaging tool profiles:
        tools: { alsoAllow: ["sessionbus"] },
      },
    },
  },
  channels: {
    sessionbus: { enabled: true }, // the bus channel; without it bus messages are refused with this hint
  },
  plugins: {
    entries: {
      sessionbus: {
        config: {
          // socket: "/run/user/1000/sessionbus/presence.sock", // optional; see Socket below
          // busSessionBinding: "name", // name | sessionId | singleton; per agent too
          agents: {
            advisor: {
              enabled: true,
              groups: ["family"], // must match a group a sending CC session launches with (-g)
              sessionKey: "agent:advisor:telegram:direct:123456789",
              name: "advisor", // optional; else falls back to the agent's own identity.name
            },
          },
          lanes: {
            agents: {
              advisor: { description: "Research and review" },
            },
            credentialDir: "/srv/openclaw/lane-cli",
            // auditLog: "/srv/openclaw/lane-permissions.log",
          },
        },
      },
    },
  },
}
```

- **`enabled`** — default `false`. Nothing connects to the bus until you opt an agent in.
- **`groups`** — required for an enabled agent (Sessionbus peer visibility is shared-group gated).
  Launch the sending CC session with a matching `-g <group>` flag.
- **`sessionKey`** — the session this peer lives in. Defaults to the agent's main session
  (`agent:<id>:main`), which is enough for chat-first OpenClaw setups. When the owner talks to the
  agent over a specific channel DM instead (for example, when the agent's main session has no external delivery route), set it explicitly to that session's key — the same key OpenClaw's own
  session listing shows for that conversation (e.g. `agent:advisor:telegram:direct:<chatId>` for a
  per-channel-peer Telegram DM). Use the key exactly as `openclaw sessions` lists it — OpenClaw
  keys are lowercase; a key with capitals is refused.
- **`busSessionBinding`** — where inbound bus messages go: `name` (default), `sessionId` or `singleton` (see
  Behaviour). Set it at the config root for every agent, or under `agents.<id>` to override it for one agent.
  Any other value is a config error and the affected agents do not connect. It is a different setting from
  `sessionKey`: `sessionKey` is the peer's owner session, `busSessionBinding` picks the bus session a message
  from another peer lands in.
- **`channels.sessionbus.enabled: true`** — turns the bus channel on. It is a separate switch from the
  per-agent `enabled` above: a delivery is refused while it is false. On OpenClaw 8.2 and 9.x set it after the
  plugin is installed (the channel id is unknown to config validation before that).
- **`agents.entries.<id>.tools.alsoAllow: ["sessionbus"]`** — exposes the tool when the agent
  uses a `coding` or `messaging` tool profile.
- **`lanes.agents`** — one or more OpenClaw agent IDs allowed in the lane product. Each ID must
  exist in `agents.entries`; `description` is optional, one line and at most 120 characters.
  The plugin resolves each agent's workspace from OpenClaw's config. For nonnumeric IDs, key
  order is `describe` order. Purely numeric IDs that JavaScript treats as array indexes (such
  as `1`) appear before nonnumeric IDs even if declared later.
- **`lanes.credentialDir`** — absolute directory holding the lane's separate, non-admin paired
  device and tokenless `openclaw.json`. The worker uses it for every OpenClaw child.
- **`lanes.auditLog`** — optional absolute path for permission decisions.
- **`SESSIONBUS_OPENCLAW_BRIDGE`** (environment, optional) — absolute path of the worker bridge socket. Default:
  `$XDG_RUNTIME_DIR/openclaw-sessionbus/bridge.sock`, else `/tmp/openclaw-sessionbus-<uid>/bridge.sock`. Set it
  only for a non-default path, and then identically in the Gateway's and the daemon's environment. Its directory
  must be private to the user (mode 0700): the plugin creates a missing directory, never changes an existing
  one, and refuses to start the bridge in an existing directory that others can access.

### Several agents on the bus

A bus peer needs **no** OpenClaw binding, and the bus address is the agent: a message to the peer `openclaw-<agent>` is
always routed to that agent. For each delivery the plugin adds a synthetic binding
`{ agentId: <addressee>, match: { channel: "sessionbus", peer: { kind: "direct", id: "*" } } }` to the config it hands to
OpenClaw's route resolver (a per-dispatch copy; your config is not touched). Its tier (`peer.wildcard`) outranks the
channel-wide and account-wide bindings, including the one OpenClaw materializes when a roster uses the legacy
default-agent marker (`agents.entries.<id>.default: true` becomes
`{ agentId: <default>, match: { channel: "sessionbus", accountId: "*" } }` for every channel without a binding). So the
legacy marker no longer breaks non-default agents, and `agents.ownership: "explicit"` works too.

An operator peer-wildcard binding for `sessionbus` (`peer: { kind: "direct", id: "*" }`) is overridden the same way, session
override included: the synthetic binding is first in its tier, so the bus address stays the agent. Only a binding for a
specific peer stays effective: an operator binding `{ agentId: other, match: { channel: "sessionbus", peer: { kind:
"direct", id: "<peer>" } } }` is treated as intent, and a delivery from that peer to another agent is refused as `rejected` /
`route_mismatch` (the receipt says nothing more: local routing is not sent over the bus; the Gateway error log names the
binding's agent). Remove the binding to lift the refusal. `<peer>` is the bound peer id, not always the sender's session_id:
the encoded name in `name` mode (`review:one` → `review%003aone`), the session_id in `sessionId` mode, `%all` in `singleton`
mode. The same goes for a `session.identityLinks` entry `sessionbus:<peer>`: OpenClaw honours it in every mode and then
uses its canonical id as the session's peer.

Sharing a session by name is deliberate: any federation peer can pick a name, so a peer that takes another's name joins
its conversation. Each message still carries its sender's session_id, and its reply footer targets that session_id;
deduplication and the loop guard stay per session_id, so same-named peers do not swallow each other's messages or share a
loop budget. The agent chooses the recipient when it sends, and the plugin does not enforce it: in a shared session the
agent can address any peer it has seen. Use `sessionId` where names cannot be trusted.

On OpenClaw 9.x a multi-agent roster with explicit ownership also has to name the agents that own the ambient surfaces:
set `agents.defaults.systemAgent.agentId` (and `agents.defaults.heartbeat.agentId`), or the Gateway reports "Multi-agent config
has no ambient heartbeat owner" and skips workspace plugin discovery. These are service targets, not channel bindings. Beware
`openclaw doctor --fix` on a roster that still has the legacy marker: it materializes that marker into channel-wide bindings
for every channel (9.x `docs/concepts/multi-agent.md`), including `sessionbus`, which brings back the refusal above — check
`openclaw agents list --bindings` after a doctor run.

### Socket

Resolved in this order: `config.socket` (absolute path), `$SESSIONBUS_SOCKET`,
`$XDG_RUNTIME_DIR/sessionbus/presence.sock`, `/tmp/sessionbus-<uid>/presence.sock`. Only set
`socket` explicitly if the Gateway service's environment doesn't already match the daemon's
default discovery path.

### Applying config changes

On OpenClaw 8.2, while the plugin is loaded, a change under `plugins.entries.sessionbus` restarts
the Gateway (the plugin declares it as a restart prefix). OpenClaw defers this config-triggered
restart until active work drains, up to 5 minutes, then forces it; a `config set` run from inside
an agent's own turn may still cut that turn short, so schedule it outside the turn (for example
with a delayed `systemd-run`).

A disabled plugin registers no reload rules, so enabling it (`plugins.entries.sessionbus.enabled`
false → true) is still an in-place reload: run `openclaw gateway restart` right after. That
explicit restart is not deferred.

A restart closes the native sessions of open lanes: a worker that loses the plugin's bridge closes its native session (after a
running turn finishes). The worker retains results until the daemon's ordinary lane retirement policy or an
explicit close; neither bridge loss nor an ACP child exit adds a deadline. A child exit ends the active Run unavailable and closes native resources while preserving that record and earlier unacknowledged results. Further Runs are unavailable with `lane native session exited`, or `lane bridge lost` if both faults occurred. Close the old lane before resuming afterwards (see
[lane lifetime](../lane/README.md#losing-the-bridge)). A change
under `channels.sessionbus` is applied by OpenClaw as a restart of that channel only.

On OpenClaw 8.2, after an in-place `plugins.*` reload, we observed a claude-cli agent's
heartbeat fail with "prepared model runtime plugin generation was superseded" until the
Gateway restarted. The cause inside OpenClaw has not been established. After any
`plugins.*` change, run `openclaw gateway restart`.

## Chat command

`/sessionbus [roster|status|help]` answers in the chat without a model turn.

- `roster` — a table of the bus (host, kind, state, product, name — the columns of `sessionbus roster`) as this chat's agent sees it: the agent's shared
  groups, not the operator roster. Your own agent is marked "(self)"; at most 40 rows are shown.
- `status` — the agent's bus id, name, groups, connection, OpenClaw version and number of visible sessions.
- If another federated host does not answer within 5 s (a laptop asleep keeps its link open for minutes), `roster`
  shows this host only and ends with `incomplete: other hosts did not answer within 5 s`; `status` marks the session
  count `(this host only; …)`.
- `help` — the subcommands. With no argument the command shows a button menu (plain text on channels without buttons).

On Telegram the menu buttons are callback buttons handled by the plugin, which resubmits `/sessionbus <sub>` as text
(command buttons of plugins fall into a model turn on OpenClaw ≥9.6). The resubmitted text is handled only while
`commands.text` is not `false`; otherwise type `/sessionbus roster` instead. A press by a non-owner in a group is
answered "owner only" and the core then removes the buttons from the menu message.

Owner only: `commands.ownerAllowFrom` must list the owner, otherwise nobody passes. Telegram renders the table
natively with `channels.telegram.richMessages: true` and as a monospace block otherwise. Text that arrives over the
bus never runs the command.

## Tool

In the owner's bound session, in the bus sessions of the channel (`agent:<id>:sessionbus:direct:<peer>`, once a
message has been delivered into them) and in lane sessions whose worker has registered on the bridge, the agent
gets one `sessionbus` tool with 13 actions: `list`, `send`,
`describe`, `spawn`, `trace`, `run`, `start`, `status`, `wait`, `ack`, `interrupt`, `close`, and
`forget`. Outside those sessions the tool is absent. In a lane session the action runs in the lane's worker (over
the bridge) and is performed on the bus as the lane. `list` includes federated peers and lanes
visible through shared groups.

For a lane, call `spawn` (optionally with `host`), then `start` and end the turn. A
“Lane … is done” message wakes the agent; collect with `wait` using the returned `run_id`,
then `ack` and `close`. The tool sends `auto_close_ms: 600000` (10 minutes) when spawn omits it,
including on resume. Set a longer value, or `0` and close the lane yourself, if collection may
take longer. The plugin adds no time limit to `run` or `wait`. It does bound lookups that a sleeping federated host
would otherwise stall: before a `send` the recipients are looked up — a target on its own host, a bare name on this
host, a group on every host unless `host` is given — and if that takes over 5 s the tool answers `message NOT sent` and
nothing is delivered. An unfiltered `list` fails after 10 s. A filtered `list` has no plugin limit; `list {host}` for an
awake host answers at once. A spawned lane receives only its
private groups plus `extra_groups`; pass `extra_groups` when other sessions must reach it.

## Telegram from a bus session

The agent's bus session is separate from the owner's Telegram session, so `message` to Telegram from it crosses
providers. On OpenClaw 8.2 that needs `agents.entries.<id>.tools.message.crossContext.allowAcrossProviders: true`
for the agent; 9.5 and later allow it by default. The plugin never sets the flag.

## Known semantics

- A bus turn is an ordinary turn of the channel session. Its final text goes nowhere; the log shows
  `sessionbus: reply suppressed <message_id> kind=<kind>`. The sender gets no notice of a failed turn; failures are
  visible in `openclaw logs`.
- Same-sender messages are steered into that sender's running turn, so one sender's second message does not start a
  second concurrent turn. Messages from different senders run in their own sessions, in parallel, up to the agent's
  16 unfinished dispatches.
- The dedupe ledger, the unfinished-dispatch counter and the recorded bus sessions (the ones that get the tool) are in
  memory, kept with the channel runtime. A Gateway restart empties them and drops turns that were running. A plugin
  generation swap or an in-place restart of the service keeps them, because the channel stays attached and the old
  dispatches keep running; a restart of the channel itself starts clean. After a Gateway restart a bus session gets the
  tool again once its sender writes again.
- `commands.allowFrom` does not apply to bus input (the plugin overrides the sessionbus entry per dispatch and the
  channel enforces owner-only commands). If you rely on an operator allowlist for other channels, nothing changes
  for them.
- Inbound bodies from older plugin versions that start with `[openclaw-peer service]` receive the
  no-reply footer. The marker is a convention, not a protocol flag; the pair loop guard (below)
  still bounds agent-initiated sends.
- Lanes: a message to a lane that is working is steered into its running turn and acknowledged `written`. If
  OpenClaw declines the steer (reason `registry_absent` or `not_eligible`; OpenClaw does not say why) the worker
  answers `not_running` after three short retries and logs `steer fallback (not BUSY-MID)`; the daemon then runs the
  message right after the current run, without loss. That is a safety net, not BUSY-MID. The receipts are listed in
  [docs/ARCHITECTURE.md](ARCHITECTURE.md#a-message-to-a-lane-that-is-working).

### Conformance with Sessionbus 0.5.12 (BUSY-MID)

The protocol requires a message that arrives while the agent works to join that work before it ends. Status per
runtime; "smoke" names the clause that proves it (peer: `smoke/docker/run.sh`, lane: `smoke/docker/lane/run.sh`,
each on OpenClaw 2026.8.2 and 2026.9.8). Results come from the docker matrix (mock model, and a fake codex backend
for the codex runtime); the rows marked "Live" come from one supervised run on a real install, OpenClaw 2026.8.2 and
again after the upgrade to 2026.9.8; a row without a result is not yet claimed.

| Case | Behaviour | Smoke clause | 2026.8.2 | 2026.9.8 |
| --- | --- | --- | --- | --- |
| Peer, embedded runtime, idle agent | the message starts a turn, no heartbeat | `idle-wake` | PASS | PASS |
| Peer, embedded runtime, busy agent | steered into the running turn | `busy-mid` | PASS | PASS |
| Peer, message in the turn's finalization window | delivered in the same turn or as the next turn, never lost | `busy-mid-boundary` (x5) | PASS, 5/5 (2 same-run, 3 followup-after-end_turn) | PASS, 5/5 (1 same-run, 4 followup-after-end_turn) |
| Peer, `/status`, `/stop`, `stop`, inline `/<skill>` | reaches the agent as text, run not interrupted | `slash-as-text` (default config and `commands.allowFrom["*"]`) | PASS | PASS |
| Peer, bus turn next to the owner's Telegram turn | separate sessions, no interruption | `tg-split` | PASS | PASS |
| Peer, codex runtime | steered into the running codex turn | `busy-mid-codex` (fake codex backend) | PASS | PASS |
| Lane, embedded runtime | `written`, reaction inside the run's result | `busy-mid` | PASS | PASS |
| Lane, codex runtime | `written`, reaction inside the run's result | `busy-mid-codex` (fake codex backend) | PASS | PASS |
| Lane voice | the lane's `sessionbus` tool reaches the bus through the bridge | `lane-voice` | PASS | PASS |
| Live, codex peer on a real install | a six-step task, a message mid-way | not automated (2026-10-06) | PASS: one turn, reaction in step 4 | PASS: one turn, reaction in step 3 |
| Live, codex lane on a real install | the same task in a lane, a message mid-way | not automated (2026-10-06) | PASS: `written`, `steer … eligible=true`, one run | PASS: same |
| Live, `claude-cli` peer | a message mid-turn | not automated (2026-10-06) | — | next turn, not lost (known gap, see below) |
| Live, Telegram next to the bus | the owner's Telegram turn next to a bus turn; a bus session writes to the owner's Telegram | not automated (U-11, 2026-10-06) | PASS: message to Telegram (`crossContext.allowAcrossProviders`) | PASS: turns run in parallel, neither interrupted; message to Telegram |

Runs behind the table:

- Peer smoke, both versions: export `4d6edd9`, 24 clause lines PASS, summary `ALL CLAUSES PASS`, including `busy-mid-codex`.
- Peer smoke after P-ROUTE-1, both versions and both roster modes (explicit ownership, legacy `default: true` marker): export
  `42647d1`, 24 clause lines PASS per version and mode, summary `ALL CLAUSES PASS` per run, including `busy-mid-codex`.
- Lane smoke, both versions: export `650f08d`, 23 of 23 clauses PASS, including `busy-mid`, `busy-mid-codex` and `lane-voice`.
- Codex probe: export `dcf4291`, both versions: egress blocked, codex binary present, `config.toml` honoured, no API key
  needed, the second message steered into the running codex turn (`turn=same`); streaming deltas are not needed.
- Commits after those exports that the smokes did not cover: `492d955` (a dead wait removed from `busy-mid-codex`;
  the clause logic is unchanged) and the test fixes merged at `995ff79` (tests only).

Measured window: `busy-mid-boundary` sends the second message about 500 ms before the end of the first turn's last
model chunk, five times per version. Only the observed split counts: the message joined the running turn
(same-run) or ran as the next turn (followup-after-end_turn); no message was lost in either version. OpenClaw's
documentation says a message that cannot be steered becomes a followup turn (`queue.md`), which the clause accepts.

Status of P-ROUTE-1 (several agents on the bus): fixed by PLAN Amendment 3 (owner decision, option B). The plugin's
synthetic peer-wildcard binding keeps the bus address the agent, so a roster with the legacy `default: true` marker
routes a message to a non-default agent to that agent. `route_mismatch` remains only for a peer-specific operator
binding of the sender to another agent (see "Several agents on the bus"). Verified by the peer smoke from export `42647d1`
in both roster modes, `agents.ownership: "explicit"` and the legacy marker (`SMOKE_PEER_OWNERSHIP=marker`), on both
versions: 24 clause lines PASS per version and mode, summary `ALL CLAUSES PASS`, no FAIL, `busy-mid-codex` included
(in marker mode the message to the non-default agent `codexsmoke` reaches it). The commits after `42647d1` change tests
and docs only.

Known gap (accepted by the owner for these runtimes only): agents on the `claude-cli` runtime. OpenClaw 8.2 and 9.8
offer no way to feed input into a running claude-cli turn (the steering API reports it as not eligible), so a message
to a busy claude-cli agent is processed right after the turn, automatically and without loss — the safety net above,
not BUSY-MID. An upstream request for steering support in claude-cli is open with the OpenClaw project. Forced abort
or restart is not used as a substitute. Moving agents to another runtime is outside this plugin.

Platform dependency: lane steering and the busy peer path rely on OpenClaw's `queueAgentHarnessMessage` (deprecated,
no removal date; see docs/ARCHITECTURE.md, Upstream coupling), and the peer path on the channel runtime's own steer
rule for a busy session.

## Security

- This plugin is unsandboxed in the Gateway process, like any other native OpenClaw plugin.
  OpenClaw's own tool policy and sandbox for the woken turn are unaffected by this plugin.
- Loop protection reads OpenClaw's own `channels.defaults.botLoopProtection`
  (`enabled`/`maxEventsPerWindow`/`windowSeconds`/`cooldownSeconds`) and applies a sliding-window,
  both-directions budget per counterpart, on canonical `id@host` ids, including group fan-out.
- Sessionbus peers and lanes are treated as one trust boundary — see "Trusted bus content"
  above. Any shared-group member can ask the agent to spawn a lane on any federation host,
  including with elevated native `open.permission_mode`, `open.arguments`, or `open.model`.
  Sessionbus group visibility is the access boundary; this plugin adds no spawn cap.
- The worker bridge is a unix socket of the Gateway's OS user (directory 0700, socket 0600). Any process of that
  user can connect to it, as it can to the Sessionbus daemon socket; there is no secret on the wire. The plugin
  accepts a lane registration only for a session key of the form the worker mints, for an agent in `lanes.agents`,
  one live registration per key, and steers only into the key its connection registered. The limits (256
  connections, 64 pending requests per connection, 1 MiB per line, 10 seconds for a partial line) bound a flooding
  process.
- Bus text can neither run a command nor re-authorize itself: see "Bus text is text".

## Restart behaviour

A Gateway restart or a plugin generation swap restarts the bridge: connected workers lose it and close their native sessions
(see "Applying config changes"). Headless lane workers retain results even if the restart kills their ACP child: native cleanup does not retire the worker. New native work is refused; close the old lane explicitly before resuming. A daemon connection loss re-hellos the peer with the same identity once the socket reconnects.
Stopping and starting the plugin (a Gateway restart, or OpenClaw's in-place reload after another
plugin's config change) reconnects every enabled agent;
if an agent's `groups`/`name`/`sessionKey` changed, the next connection carries the new identity —
this plugin never re-hellos a live connection with a different identity, since a peer has no
durable rows on the daemon side and groups are immutable on a live connection. Every terminal
peer close is logged with its session id and close code.

For the headless Sessionbus lane worker, see [lane/README.md](../lane/README.md).
