# OpenClaw as a Sessionbus lane

The `openclaw-peer` Go worker lets a Sessionbus caller run an allowed OpenClaw agent as a headless lane. Install it with the plugin using the [root README's Install section](../README.md#install).

```json
{
  "action": "spawn",
  "arguments": {
    "product": "openclaw-peer",
    "host": "<host>",
    "name": "review",
    "open": {
      "arguments": ["--agent", "reviewer"],
      "cwd": "/absolute/path/inside/reviewer-workspace",
      "permission_mode": "default",
      "model": "provider/model",
      "reasoning_effort": "medium"
    }
  }
}
```

`describe` lists `cwd`, `permission_mode`, `model`, `reasoning_effort`, and `arguments`. Its `--agent`
description lists each allowed id and one-line role, followed by `[models: …; default: …]`. The
allowlist appears as the operator wrote it, with wildcards and aliases unexpanded; an empty
allowlist shows `any`. Lists longer than 20 entries show the first 20, then `… +N more`. The default
is the model OpenClaw resolves for that agent. With one allowed agent, omission selects it; with
several, `--agent <id>` is required. The operator writes the role descriptions, and they are visible
to anyone who can describe the product.

The list and default are read when the Gateway starts. After changing an agent's model policy,
restart the Gateway to refresh `describe`; `sessions.patch` always applies the live policy.

## Configuration

Configure lanes in the Gateway's `openclaw.json` at `plugins.entries.sessionbus.config.lanes`. See the [plugin guide](../docs/GUIDE.md#configure) for the full JSON5 example. `lanes.agents` lists at least one existing OpenClaw agent ID and optional one-line description (at most 120 characters). The plugin resolves each workspace using OpenClaw's own agent settings. Nonnumeric IDs retain their declared key order in `describe`; purely numeric IDs that JavaScript treats as array indexes (such as `1`) appear before nonnumeric IDs even if declared later. `lanes.credentialDir` is the absolute path to the lane's separate paired-device state and tokenless `openclaw.json`. `lanes.auditLog` is an optional absolute path.

The worker gets these settings from the running plugin over the **bridge**, a unix socket the plugin listens on: `$SESSIONBUS_OPENCLAW_BRIDGE`, else `$XDG_RUNTIME_DIR/openclaw-sessionbus/bridge.sock`, else `/tmp/openclaw-sessionbus-<uid>/bridge.sock`. Nothing is published to disk. The Gateway and the Sessionbus daemon must derive the same path, so for a non-default path set `SESSIONBUS_OPENCLAW_BRIDGE` (absolute) identically in the Gateway's and in the daemon's environment. The worker reads the settings once at its process start, so a `lanes` edit takes effect for **new** lanes after the Gateway restart that a `plugins.entries.sessionbus` change triggers (see the [plugin guide, "Applying config changes"](../docs/GUIDE.md#applying-config-changes)), without restarting the Sessionbus daemon. If the plugin is not reachable, the worker still starts and `describe` works, but its `--agent` description reads `lane settings unavailable: …`, and Open refuses with the cause and the fix: start the Gateway with the sessionbus plugin (the bridge runs whether or not the bus channel is enabled), or set `SESSIONBUS_OPENCLAW_BRIDGE` to the same path on both sides.

The Sessionbus daemon's `service.env` needs the `openclaw-peer` product in `SESSIONBUS_PRODUCTS` and the worker on `PATH`; lane values live in `openclaw.json`. Any legacy `OPENCLAW_LANE_*` variable in the worker environment makes it exit at startup with a migration error.

## Open, run, and resume

The Sessionbus lane name is not shown as the OpenClaw session title. In OpenClaw 8.2, ACP `session/new` `_meta.sessionLabel` selects an existing session by label; it does not set a title, and this worker does not patch the label separately.

The caller may supply `cwd` only as an existing absolute directory whose realpath lies inside the selected agent's resolved OpenClaw workspace. A fresh lane with `cwd` creates its Gateway session there before ACP starts. Without `cwd`, it starts in the agent's workspace. Rejected paths return `unsupported value cwd=<value> (...)`; Gateway refusal retains `refused by OpenClaw` in the reason, while a transport failure fails spawn. Starting in a directory does not prevent tools from accessing other paths.

`model` accepts provider-qualified refs, including nested refs such as `openrouter/z-ai/glm-5.3` and
profile suffixes such as `anthropic/claude-x@work`, an alias, or a bare model id. OpenClaw resolves it
in the agent's scope through `sessions.patch` on this lane's key only; it does not change the agent's
configured default. A refusal names the agent's allowed models and default. `reasoning_effort` is
sent with each prompt. If the model does not support the requested level, that run fails with
`unsupported value reasoning_effort=<level> (...)`.

| `permission_mode` | Response to a permission request |
| --- | --- |
| `default` or omitted | Approve reads, searches, and fetches; deny exec and other requests. |
| `bypassPermissions` | Approve every request. |
| `plan` | Deny every request. |

The agent's own exec rules decide whether a permission request arises. Every request that reaches the worker is answered. The caller's mode determines the answer; the operator sets no separate ceiling.

Text beginning with a slash command is refused: `slash commands are not accepted over the lane`. Recognized built-in inline directives in Text and bus Delivery are neutralized before they reach OpenClaw. Ordinary paths and names that are not directives remain text. Operator-configured OpenClaw model aliases (`/<alias>`) are **not** neutralized. A leading `/<alias>` in Text is refused by the slash rule; an alias appearing mid-text or in bus Delivery reaches OpenClaw as for any user and can switch the lane's model for that turn.

Close releases the lane. `spawn {"resume_session_id":"<earlier lane id>"}` returns that exact id and keeps the original cwd replayed by the daemon. Resume rechecks the current agent workspace and the Gateway's recorded cwd; drift is refused. Only wrapper-minted keys for agents still in the allowed list are admitted. A never-prompted lane cannot be resumed because it has no persisted conversation history.

Open does not probe the tool. A fresh Open and a resume go through one attach: the worker starts the `openclaw acp` child, opens or resumes the ACP session, then registers the session key on the bridge (`lane.register`). The plugin offers the `sessionbus` tool to a lane session while that registration is live, so the tool is there from the first prompt. A registration is refused for a key the worker did not mint, for an agent outside `lanes.agents`, or for a key that is already registered. A tool call from the lane agent runs in the worker (`bus.action` over the bridge) and is performed on the bus as the lane; aborting the call cancels the action.

### A message to a working lane

A bus message to a lane that is running is steered into that run (the worker asks the plugin, which steers through OpenClaw) and acknowledged `written`, so the agent can react before the run ends; the sender's call is never held until the turn ends. The receipts: a lane with no active run answers `not_running` (the daemon starts a run); the bridge lost, a refusal from the bridge, or a request that could not be written answers `rejected` / `not_submitted`; a request that was written but never answered is the error `uncertain_native_admission`. If OpenClaw does not accept the steer, the worker retries three times, 100 ms apart, then answers `not_running` and logs `steer fallback (not BUSY-MID): reason=<registry_absent|not_eligible> — message deferred to the next run`. That is a safety net: the message runs right after the current run, without loss, but it did not join the work in progress. Steering into a `claude-cli` lane is not possible today (see the conformance log in the [plugin guide](../docs/GUIDE.md#conformance-with-sessionbus-0512-busy-mid)). Steered text has built-in inline directives neutralized, like any bus delivery.

### Losing the bridge

Without the bridge the lane can neither be steered nor serve its tool, so it refuses new native work. An idle native session is closed immediately. A running native session finishes its run first and is closed once the daemon has seen the outcome. The worker stays connected so the caller can read and acknowledge retained results with `status`, `wait` and `ack`, until the daemon's configured `auto_close_ms`, owner-lifetime policy, explicit `close`, or daemon disconnect retires it. Bridge loss adds no deadline, and ACK does not retire the worker. New Runs return `unavailable` (`lane bridge lost`), which does not start a new automatic-close grace. Admitting that Run clears any previous deadline; close the lane explicitly after such a refusal even with nonzero `auto_close_ms`. Also close explicitly with `auto_close_ms: 0` or no prior native terminal (nonpersistent lanes also close when their owner leaves). Pending bridge actions are cancelled. A Gateway restart (including the one a `plugins.entries.sessionbus` change causes) closes native sessions this way; close the old lane before resuming afterwards. An actual ACP child exit also closes native resources after its Run ends without retiring the worker or discarding retained records. A later Run is `unavailable` with `lane native session exited` (`lane bridge lost` takes priority when both faults occurred). An idle child exit is detected by the next Run, which ends unavailable; further Runs are refused. Close the old lane explicitly before resuming, just as after bridge loss.

ACP output is bounded as it arrives (900 KiB per run, cut on a character boundary); a truncated result carries the usual truncation note with the number of omitted bytes.

## Credential and isolation

Run the worker's OpenClaw CLI under a **paired device credential** with exactly `operator.read`, `operator.write`, and `operator.approvals`. Do not use a shared Gateway token or password: shared-secret auth can grant admin scope. The worker strips `OPENCLAW_GATEWAY_TOKEN` and `OPENCLAW_GATEWAY_PASSWORD` from both ACP and Gateway-call child environments, along with the Sessionbus launch token. It sets every child's `OPENCLAW_STATE_DIR` to `lanes.credentialDir` and `OPENCLAW_CONFIG_PATH` to `<credentialDir>/openclaw.json`, overriding the daemon's own values. That config must reach the Gateway without a URL override.

At every Open, the worker's admin guard calls the read-only `exec.approvals.get` Gateway method before any session read or write. Its required `operator.admin` scope must be refused with that specific missing-scope result (or the known device-token scope-mismatch result). A successful probe means admin and is a hard refusal. Any other error fails closed as `lane credential check failed`; that inconclusive result is not cached, so a later Open retries safely. A confirmed non-admin result is cached for the worker process. The fixed ACP and Gateway-call commands receive the same credential environment from `lanes.credentialDir`.

### Pairing the device (install step)

`lane/pair-device.mjs` mints that credential. It connects to the Gateway once, through `GatewayClient` (`openclaw/plugin-sdk/gateway-runtime`), requesting exactly `operator.read`, `operator.write`, and `operator.approvals` at connect time — never the constructor's admin default — so silent local pairing (the Gateway's own local-only, no-approval-prompt path; see `openclaw`'s `docs/gateway/pairing.md`) mints a device token with those scopes and nothing wider. It never holds `operator.admin`, not even transiently, and there is no `devices rotate` step: narrowing an already-admin-paired device that way needs a caller connection scoped to `operator.pairing`, which no CLI surface exposes non-interactively.

Run it once as the daemon user on the host that runs the worker's OpenClaw CLI. Log in directly as that user with `ssh <daemon-user>@<host>`; do not use `sudo`. The script checks two requirements before connecting:

- **`OPENCLAW_STATE_DIR` must be a fresh, dedicated directory for the lane's own device identity** — never the daemon user's default OpenClaw state dir (`~/.openclaw`, or `$OPENCLAW_HOME/.openclaw` if set), and never a directory that already has a `state/openclaw.sqlite`. A device already paired there — from an earlier `openclaw acp`/TUI run against that same state dir, or because it's the Gateway's own state dir (the daemon user and the Gateway user are the same on many installs) — gets its existing token handed back unchanged on the next connect, whatever scopes this run requests; the script refuses outright rather than trust a scope check that reused admin could satisfy by coincidence.
- **The Gateway target comes from that state dir's own config**, never a hardcoded port or a scan: `OPENCLAW_CONFIG_PATH` (or the state dir's default config location) needs `gateway.mode: "local"` and, if the Gateway isn't on the default port, `gateway.port` set to match. `gateway.mode: "remote"` is refused — silent local pairing needs a same-host connect anyway.

Pass the shared Gateway token through env only, and keep it out of shell history and argv on both ends of the connection: a literal `OPENCLAW_GATEWAY_TOKEN=<token> node …` on the command line lands in this shell's own history, and passing it as part of an `ssh`/`sudo` command line puts it in that command's argv, where `ps` and (for `sudo`) the system auth log can show it. First open an interactive SSH login as the daemon user; the terminal gives the remote prompt a TTY. At that remote prompt run the `bash -c` block (`sh -c` is not portable because some `/bin/sh` implementations lack `read -s`):

```sh
ssh <daemon-user>@<host>
```

At the remote prompt:

```sh
bash -c '
  IFS= read -rsp "Gateway token: " OPENCLAW_GATEWAY_TOKEN &&
  export OPENCLAW_GATEWAY_TOKEN &&
  OPENCLAW_INSTALL_DIR=/absolute/path/to/openclaw \
  OPENCLAW_STATE_DIR=<fresh, dedicated dir> OPENCLAW_CONFIG_PATH=<that dir>/openclaw.json \
    exec node /absolute/path/to/checkout/lane/pair-device.mjs
'
```

Paste the token at the remote prompt and press Enter (nothing echoes). `OPENCLAW_INSTALL_DIR` is set explicitly rather than relying on `PATH`: a remote shell's `PATH` may not include `~/.npm-global/bin` (where an npm-global install may put its symlink), so leaving it to the `resolveInstallDir` PATH fallback can fail even though `openclaw` works fine in an interactive login shell. `OPENCLAW_GATEWAY_TOKEN` is read from env only — the script never accepts it as an argument, and never writes, logs, or forwards it. On success it prints the granted scopes and exits 0. If the Gateway leaves a pending pairing request instead of pairing silently (for example because `gateway.nodes.pairing.autoApproveLocal` is `false`, or the connection isn't local-equivalent), it exits non-zero — do not approve the request or fall back to a shared token; escalate instead. Then use the rollout checks below to confirm the paired device from the admin side. The docker smoke runs the same script as `run.sh provision_credential` and checks the outcome with the admin CLI.

### Same-user install (daemon and Gateway share an OS user)

Many installs run the Sessionbus daemon and Gateway as the same OS user, whose default `~/.openclaw/openclaw.json` carries an admin shared secret for the operator's own CLI. Keep the lane's paired device in a separate directory and set **`lanes.credentialDir` in the Gateway's `openclaw.json`** to that directory. Put a dedicated, tokenless `<credentialDir>/openclaw.json` there: `gateway.mode: "local"`, `gateway.bind: "loopback"`, and a `gateway.port` matching the real Gateway, with no `gateway.auth` block. The worker gives this state and config path to every child, including the admin guard probe. The daemon's own state and config can remain its normal values.

Verify both after pairing: `openclaw devices list --json`, run as **admin** (the Gateway user's normal env/config, not the lane's dedicated state dir), shows a paired device with exactly the three scopes — identify it as the one newest device by diffing the paired list from before pairing, the same way `run.sh provision_credential` does, since nothing else names it as "the lane device". Running `devices list` *under* the dedicated state dir instead cannot work: that CLI authenticates with the lane's own non-admin token, and `devices list` needs `operator.pairing`, which the lane token doesn't have and must never be given (`unauthorized: device token scope mismatch (re-pair or approve scope upgrade)` — do not act on that wording; see the credential section above). Under the dedicated state dir, the only thing to check is that `openclaw gateway call status --json` succeeds tokenless. The dedicated config file itself must have no `gateway.auth` key.

The wrapper guarantees selected-agent cwd containment, minted-key resume admission, and slash/directive handling. File access between agents is governed by each OpenClaw agent profile. Set `agents.entries.<id>.tools.fs.workspaceOnly: true` and an appropriate exec approval policy or sandbox for every lane agent.

**When the daemon and Gateway share an OS user, an agent allowed to exec can read that user's admin `~/.openclaw/openclaw.json`, including any shared token.** The lane's paired-device scope limits its Gateway RPCs, but cannot limit file access through exec. Set `agents.entries.<id>.tools.fs.workspaceOnly: true` and require an exec approval policy or sandbox for each lane agent. Allowing exec without approval or sandbox access to the admin config removes that protection. Check the effective policy on each installation.

## Exchanging files between lanes

For text, send content over Sessionbus with the lane's `sessionbus` tool; a bus frame is limited to 1 MiB. For code or larger files, use a shared Git repository with a separate clone or worktree inside each agent's workspace. That recipe works only when each agent profile permits the needed exec access to its repository.

## Rollout checks

Run these checks on the target installation after pairing:

- As the daemon user, run `openclaw --version` and confirm `openclaw` is on that user's `PATH`.
- Check the settings the worker receives: start a worker (or open any lane) and read `describe` — allowed IDs, descriptions and each workspace must match the agents OpenClaw resolved (no `lane settings unavailable` text). Verify `tools.fs.workspaceOnly`, exec approvals or sandbox, tool policy allowing `sessionbus`, and `commands.enforceOwnerForCommands` in the agent config. Record model aliases inherited by lane agents; these are live `/<alias>` directives in caller text.
- As the daemon user, run `type -a openclaw-peer` (bash; `command -v` has no `-a`); confirm no other binary has that name on the daemon `PATH`.
- Confirm `lanes.credentialDir` points to the separate paired device with exactly `operator.read`, `operator.write` and `operator.approvals`, and that `<credentialDir>/openclaw.json` has no `gateway.auth` secret. At Open, `exec.approvals.get` must get the specific missing-`operator.admin` refusal; admin is a hard refusal, while an inconclusive result fails closed and is not cached. As admin, inspect `openclaw devices list --json` and identify the newly paired device by comparing the lists from before and after pairing. Check `gateway.roles` for overrides that could break this credential path. Under the dedicated credential directory, `openclaw gateway call status --json` must succeed without a URL override.
- Run `uname -m` and `go version` for the target OS, architecture, and build toolchain.
- Check the bridge: the Gateway and the daemon must derive the same `SESSIONBUS_OPENCLAW_BRIDGE` path (the default, or the same explicit value in both environments); its directory is private to the Gateway's user (mode 0700) and the socket is 0600. The daemon user must be that user. A mismatch shows as `lane settings unavailable` in `describe`; an existing group- or world-accessible directory makes the plugin refuse to start the bridge (see the Gateway log).
- Check the Gateway and daemon OS users and the separate `lanes.credentialDir`. When they share a user, verify an effective exec approval policy or sandbox for each lane agent alongside `tools.fs.workspaceOnly: true`; unrestricted exec can read the admin config.
- If permission decisions must be retained, set `lanes.auditLog` to an absolute path writable by the daemon user and protect the resulting 0600 file.
