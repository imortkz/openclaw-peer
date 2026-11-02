# Architecture

This repository ships two products that connect OpenClaw to Sessionbus:

- **The plugin** (`index.js`, `src/`) makes an OpenClaw agent an ordinary Sessionbus peer and adds a
  `sessionbus` channel to OpenClaw. A bus message becomes an inbound channel message in the bus session
  `busSessionBinding` picks (by default one per sender name); the agent replies with its `sessionbus` tool.
- **The lane worker** (`lane/`, binary `openclaw-peer`) runs an allowed OpenClaw agent as a
  headless lane on `spawn`, like `claude-peer` or `codex-peer`.

Setup and limits are in the [README](../README.md), the [plugin guide](GUIDE.md) and [lane/README.md](../lane/README.md). This
page shows how the parts fit together.

## Who talks to whom

<img src="architecture/overview.svg" alt="Processes and connections between the Sessionbus daemons, the plugin and its sessionbus channel inside the OpenClaw Gateway, the lane worker, its bridge connection to the plugin and its openclaw acp child." width="100%">

The plugin lives inside the Gateway and keeps one daemon connection per enabled agent. The daemon
launches a separate worker process for each lane. The worker drives the agent through a child
`openclaw acp` that runs under the lane's own paired device.

Worker and plugin also talk to each other over the **bridge**, a unix socket the plugin listens on
(`$SESSIONBUS_OPENCLAW_BRIDGE`, else `$XDG_RUNTIME_DIR/openclaw-sessionbus/bridge.sock`, else
`/tmp/openclaw-sessionbus-<uid>/bridge.sock`; JSON Lines, one JSON object per line). It carries four things:

- the worker asks for its settings (`settings.get`) instead of reading a published file;
- the worker registers the lane's session key once its session exists (`lane.register`);
- the worker asks the plugin to steer a bus message into the lane's running turn (`lane.steer`);
- when a lane agent calls the `sessionbus` tool, the plugin hands the action to that lane's worker
  (`bus.action`) and the worker performs it on the bus as the lane; `rpc.cancel` aborts it.

The bridge directory is private (mode 0700, this uid) and the socket is 0600. The plugin creates a missing
directory; it never changes the permissions of one that already exists, and refuses to start the bridge in
an existing directory that is accessible to the group or others (an operator's `$HOME`, for example): put the
socket in a private directory. A stale socket file is removed only if it is a socket of this uid and nothing
listens on it. Limits: 256 connections, 64 pending requests per connection, 1 MiB per line, 10 seconds to
complete a partially received line, 128 KiB of steered text. The trust boundary is "same OS user": any
process of that user can reach the bridge, as it could the Sessionbus daemon socket.

## Plugin: an inbound message

<img src="architecture/plugin-delivery.svg" alt="Sequence of an inbound message through the daemon, the plugin's ledger and limits, the sessionbus channel dispatch and the agent turn, with the receipt and the tool reply." width="100%">

A bus message is an inbound message of the `sessionbus` channel, in the bus session
`agent:<id>:sessionbus:direct:<peer>` chosen by `busSessionBinding`: `name` (default; one per sender name,
compared case-insensitively, so same-named peers share it and a restarted peer keeps its context; '%', ':' and
whitespace in a name are encoded as `%XXXX`; a sender without a name falls back to its session_id),
`sessionId` (one per sender session, the behaviour before 0.2.0) or `singleton` (one per agent, `…:direct:%all`).
Changing the value starts new sessions;
existing ones are left as they are, not merged. The reply address, deduplication and the loop guard stay the
sender's session_id. The channel sets its own grouping, so the global `session.dmScope`
and the owner's Telegram or main session are untouched. If the agent is idle the dispatch starts a turn; if
a turn of that session is already running, OpenClaw steers the new message into it (OpenClaw's own queue rule for
a channel message to a busy session). No system event, heartbeat or watchdog is involved.

The receipt is `queued_for_next_turn`, issued as soon as the dispatch has started. The checks run in this order:

1. A retransmission (same sender and `message_id`) gets the original receipt, with no second dispatch and no
   charge to the loop guard. The ledger keeps a started dispatch while it runs and for 20 minutes after it
   settles (at most 5000 entries, oldest settled first). **Refusals are not remembered**: a retransmission of a
   refused message is evaluated again, so a transient `backlog full` or "channel not started" does not stick.
   A dispatch that fails after the receipt is dropped from the ledger, so the retransmission dispatches again.
2. A body over 64 KiB is refused. 3. An agent with 16 unfinished dispatches refuses more (the slot is reserved
in the same step as the check, so concurrent deliveries cannot exceed it; the counter and the ledger are kept with
the channel runtime, so they survive a service restart or a plugin generation swap while the old dispatches keep running). 4. The pair loop guard. 5. `channels.sessionbus.enabled`
must be true; if the channel account has not started yet the plugin waits up to 10 seconds for it, then refuses
with a hint. 6. The route must resolve to this agent. The plugin prepends a synthetic peer-wildcard binding for the addressee to the
per-dispatch config, so channel- and account-wide bindings (the legacy default marker's included) cannot take the message;
a peer-specific operator binding of this sender to another agent still wins and the delivery is refused (`route_mismatch`). 7. Dispatch.

Bus text is never command input. Each dispatch carries `CommandInterpretationSuppressed` and an unauthorized
command flag, the plugin passes OpenClaw a per-dispatch copy of the config in which `commands.allowFrom.sessionbus`
is empty (an operator's `*` wildcard cannot re-authorize it), and the channel declares owner-only command
authorization. A text such as `/status` or `stop` reaches the agent as ordinary text.

The channel never delivers the agent's final text: its `deliver` only logs `reply suppressed`. The agent replies
only by calling the tool's `send` action in the channel session. The channel runtime that OpenClaw hands to
`startAccount` is held in a process-wide slot, not in the service: OpenClaw replaces every plugin's service on any
`plugins.*` hot reload but restarts only the channels whose own entry changed, so a new service must find the
runtime the running channel attached.

## Worker: the lane lifecycle

<img src="architecture/lane-lifecycle.svg" alt="Sequence of a lane: spawn and open with the admin check, session creation, the acp child and the bridge registration; then a run, a steered delivery, a bridge action and the result." width="100%">

The worker implements the `openclaw-peer` product on the Sessionbus Go kit. It is the trust
boundary on the lane side:

- It talks to the Gateway only under a paired device scoped to `operator.read`, `operator.write`
  and `operator.approvals`. Every Open checks that credential with `exec.approvals.get`: success
  means admin, and the Open is refused. A confirmed non-admin refusal is cached for the worker
  process; an inconclusive result is not cached and fails the Open.
- Every OpenClaw child runs with `lanes.credentialDir` as its state and config. The Gateway token,
  password and URL and the Sessionbus launch variables are removed from its environment.
- Fresh and resume Open share one attach: start the `openclaw acp` child, open or resume the ACP session,
  then register the session key on the bridge. The tool is available from the first prompt; there is no
  readiness probe. A fresh Open calls `sessions.create` only when the caller passes `cwd`, and
  `sessions.patch` only when it passes `model`. A caller's `cwd` must resolve inside the selected agent's
  workspace. Resume makes neither call, re-checks the recorded `cwd`, and admits only keys the worker minted.
- Text that starts with a slash command is refused, and built-in inline directives are neutralized.
- Every permission request that reaches the worker is answered by the caller's `permission_mode`.
  A claude-cli agent's native tools are approved by OpenClaw's own exec policy instead and never
  reach the worker, so tool access is part of the agent profile, not of the lane.
- ACP output is bounded as it arrives (900 KiB per run, cut on a character boundary); a truncated result
  carries the usual note with the number of omitted bytes.

### A message to a lane that is working

The daemon asks the worker to deliver a bus message (`Deliver`). The worker answers at once, never holds the
sender's call until the turn ends:

| State | Worker's action | Receipt |
| --- | --- | --- |
| no ACP child (lane not open) | none | `rejected` / `native_unavailable` |
| bridge lost | none | `rejected` / `not_submitted` |
| no active turn | none: the daemon starts a run | `not_running` |
| active turn | `lane.steer` with the neutralized text; OpenClaw accepts | `written` |
| active turn, the bridge answers with an error, or the request could not be written | none | `rejected` / `not_submitted` |
| active turn, request written, no answer | none | error `-32603` `uncertain_native_admission` |
| active turn, OpenClaw does not accept | up to 3 retries 100 ms apart, then `not_running` | `not_running` |

The last row is a **safety net, not BUSY-MID**: OpenClaw reports no reason when it declines (a runtime without
steering, the end of the turn, compaction), so the message is processed in the run the daemon starts next. The
worker logs `steer fallback (not BUSY-MID): reason=<registry_absent|not_eligible> — message deferred to the next
run`; on the embedded and codex runtimes the smoke treats that line as a failure. `written` means OpenClaw
accepted the text for the running turn, not that the model has read it. Steering into a claude-cli turn is not
possible in OpenClaw today (see the conformance log in the [plugin guide](GUIDE.md)).

### Losing the bridge

Without the bridge the plugin can neither steer into the lane nor serve its tool, so new native work is
refused. An idle native session is closed at once. A running native session finishes its run first and is
closed once the daemon has seen the outcome. The worker stays connected to Sessionbus to serve retained
results through `status`, `wait` and `ack`; bridge loss adds no retirement deadline. The daemon's existing
`auto_close_ms`, owner-lifetime policy, explicit `close`, or loss of the daemon connection retires the worker.
Acknowledging a result does not close the worker.

A refused new Run returns `unavailable` with `lane bridge lost`; it does not start a new automatic-close
grace. Admitting that Run clears any previous deadline, so an explicit close may be needed even with
nonzero `auto_close_ms`. The same applies when `auto_close_ms` is zero or no native terminal has armed a
deadline (a nonpersistent lane also closes when its owner leaves). Close the old lane before resuming after
the bridge is restored; this does not automatically reattach its native session. Pending bridge actions are
cancelled. A change under `plugins.entries.sessionbus` restarts the Gateway and closes native lane sessions
this way. An actual ACP child exit follows the same retention policy: native resources close after the Run outcome, while the SDK worker stays connected. Subsequent Runs return `unavailable` with `lane native session exited`; `lane bridge lost` takes priority when both faults occurred. An idle child exit is detected by the next Run, which ends unavailable. Explicitly close the old lane before resuming; child exit adds no retirement deadline and does not discard older unacknowledged results.

## Where settings come from

<img src="architecture/settings.svg" alt="Settings flow from openclaw.json to the plugin and over the bridge to the worker, from lanes.credentialDir to the worker's children, and from the daemon's service.env to the daemon." width="100%">

Lane settings live in `openclaw.json` next to the plugin's. The worker does not parse the OpenClaw
config and no file is published: the worker asks the running plugin over the bridge (`settings.get`) once at
process start. Without a reachable bridge the worker still starts and `describe` works, but its description says
`lane settings unavailable: …` and Open refuses with the cause (start the Gateway with the plugin, or set
`SESSIONBUS_OPENCLAW_BRIDGE` to the same path for the Gateway and the daemon). The plugin declares
`plugins.entries.sessionbus` as a restart prefix, so a change there restarts the Gateway and applies to new lanes
without a daemon restart.

## Conformance log (Sessionbus 0.5.12) and smoke evidence

The plugin guide's "Known semantics" holds the conformance table. The evidence comes from the Docker smokes
(`smoke/docker/README.md`, `smoke/docker/lane/README.md`), run on OpenClaw 2026.8.2 and 2026.9.8. Each clause
prints `[<version>] <clause>: PASS|FAIL — <evidence>`; the table in the plugin guide names the clause behind every
row. Results of the latest run are recorded there, not here.

## Upstream coupling

These are OpenClaw details the code depends on that OpenClaw does not promise as a contract. Check
them first when you raise the supported OpenClaw version; the Docker smoke matrix is the only
automatic guard.

- Plugin SDK subpaths imported by `index.js`: `channel-core`, `channel-inbound`, `agent-harness-runtime`,
  `state-paths`, `agent-runtime`, `health`.
- `queueAgentHarnessMessage` (`agent-harness-runtime`) is how the plugin steers into a running turn. OpenClaw marks
  it `@deprecated` (registry entry `agent-harness-sdk-alias`, deprecated 2026-04-25, no removal date; replacement:
  "none yet; retain until a harness subpath ships"). It returns only immediate eligibility; an asynchronous refusal is
  only logged by OpenClaw. If OpenClaw removes it, lane BUSY-MID and its acceptance smoke fail first.
- `resolveChannelInboundRouteEnvelope` with `defaultAgentId`, the channel context fields `CommandInterpretationSuppressed`
  and `access.commands.authorized` (on 8.2 they gate the fast-abort and skill command paths), and the channel adapter's
  `commands.enforceOwnerForCommands`; on 9.x also `inbound.ingress.resolveStable`.
- Plugin SDK subpaths imported from the targeted install by the pairing helper
  `lane/pair-device.mjs`: `state-paths` (`resolveStateDir`), `gateway-runtime` (`GatewayClient`),
  `core` (`resolveGatewayPort`), `health` (`readConfigFileSnapshot`).
- Gateway text the worker matches literally: `device token scope mismatch` (`lane/wrapper.go`),
  `Thinking level "<level>" is not supported for ` (`lane/prompt.go`), the `details.missingScope`
  error shape (`lane/internal/acp/process.go`).
- Rules copied from OpenClaw sources:
  - in `lane/prompt.go`: the `reasoning_effort` set (`AgentThinkingLevel`,
    `types.agent-defaults.ts`), the inline directive names, and the directive parser's JS `\s`
    whitespace class (`directives.ts`);
  - in `src/lanes.js`: the model-policy allowlist source selection with its migration fallback
    (agent `allow`, then defaults `allow`, then `agents.defaults.models` keys until
    `meta.migrations.modelPolicyAllowlist`), and the description limit counted in grapheme
    clusters as OpenClaw's TypeBox config validation counts `maxLength`.

## Where it lives in the code

| File | Role |
| --- | --- |
| `index.js` | Plugin entry: channel, service, tool, restart prefix. |
| `src/channel.js` | The `sessionbus` channel adapter; `startAccount` attaches the channel runtime. |
| `src/service.js` | Daemon connection per agent, per-agent ledger and backlog counter, bridge lifecycle. |
| `src/delivery.js` | `message.deliver`: ledger, limits, route, channel dispatch, receipt. |
| `src/bridge.js`, `src/bridge-path.js` | The worker bridge server (settings, register, steer, actions) and its endpoint rules. |
| `src/tool.js` | The `sessionbus` tool: its own peer in the owner session and in recorded channel sessions, the worker's voice (over the bridge) in a lane session. |
| `src/guard.js` | Per-pair message budget in both directions. |
| `src/config.js`, `src/lanes.js` | Plugin and lane settings, agent workspace resolution. |
| `lane/wrapper.go` | Open (one attach), Run, Deliver (steer), Interrupt, Close; the admin check; `sessions.*` calls; the `openclaw acp` child; bridge loss. |
| `lane/bridge.go` | The worker side of the bridge: settings, register, steer, actions. |
| `lane/internal/acp` | The `openclaw acp` child and `openclaw gateway call`, with the child environment scrubbed. |
| `lane/policy.go`, `lane/prompt.go` | Permission answers by `permission_mode`; slash refusal and directive neutralization. |
| `lane/config.go`, `lane/cwd.go`, `lane/agent.go` | Settings from the bridge, child environment, agent selection, `cwd` checks. |
| `lane/pair-device.mjs` | Install step: mints the paired device with exactly three scopes. |

The diagrams are hand-written SVG in [`architecture/`](architecture/). Update them in the same
change as the code they describe.
