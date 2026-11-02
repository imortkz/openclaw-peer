# Local OpenClaw lane smoke

This sibling fixture tests the Go `openclaw-peer` worker against a published OpenClaw Gateway
image in Docker and a **private** Sessionbus daemon on the host. It does not use the shared
daemon or other `smoke/docker/*` fixture. The Gateway SQLite state and separate `/lane-cli`
device state live on named Docker volumes. The host worker gets its
settings, registers its lane key and sends `lane.steer` to the plugin over the plugin's **worker bridge** (the plugin also hands lane tool actions to the worker over it): in production a unix
socket at `$SESSIONBUS_OPENCLAW_BRIDGE`; here the plugin runs in a container, so `bridge-tunnel.mjs` carries the
host socket over a loopback TCP port into the Gateway container's own socket (compose sidecar, published on
`127.0.0.1:$LANE_BRIDGE_PORT`). Workspace directories are bind-mounted at the same
absolute host and container paths so both the wrapper and Gateway inspect the same cwd.

Prerequisites: Docker Desktop, Node, jq, Go, OpenSSL, `sessionbus` and `sessionbus-call` on
`PATH`. The runner does not install host packages; Docker may pull the selected image.

`run_test.sh` contains Docker-free unit cases for `run.sh`, including fixture cleanup and
clause assertions. CI runs every case. Run one focused case with
`bash smoke/docker/lane/run_test.sh <case>` (for example, `early-up-bridge-cleanup`).

```sh
smoke/docker/lane/run.sh          # up, every clause, down (also on failure)
smoke/docker/lane/run.sh up       # leave the private fixture running
smoke/docker/lane/run.sh cwd-isolated
smoke/docker/lane/run.sh down
```

The Gateway defaults to `ghcr.io/openclaw/openclaw:2026.8.2`. Set
`OPENCLAW_VERSION=<release>` to test another release, or `OPENCLAW_IMAGE=<ref>` to use a
specific image. Published images include extra plugins such as `codex` and
`diagnostics-otel`; the clauses do not depend on the complete tool list.

The lane agents run in one of two roster modes: `LANE_SMOKE_OWNERSHIP=marker` keeps the legacy `default: true` marker, and `LANE_SMOKE_OWNERSHIP=explicit` removes that marker and sets `agents.ownership: "explicit"` with heartbeat, system-agent, and auth-inheritance owners set to `lane`. By default, versions before 2026.10 use `marker` (explicit ownership breaks pairing on 8.2 and workspace plugin discovery on 9.6); versions 2026.10 and later use `explicit`. A custom `OPENCLAW_IMAGE` defaults to `explicit` unless its tag parses as an older release. The override wins over version selection, and the setup PASS line prints the chosen mode.

`up` generates an ignored config and workspace tree under `.state/`, builds the worker and an
`openclaw` Docker shim into `.bin/`, starts Docker services and the host half of the tunnel, provisions the lane's device
credential, then starts a private daemon at a short `/tmp/ocl.*/p.sock` path. The daemon uses
an isolated `XDG_CONFIG_HOME` and `SESSIONBUS_OPENCLAW_BRIDGE` pointing at the host tunnel socket (`bridge:` setup
line: `settings.get` answered end to end).
`fixture.env` contains only project, socket, PID, and workspace paths; it contains no token.
Individual clauses reuse the fixture. `down` stops the daemon and Compose project, removes
its named volumes, then removes `.state`, `.bin`, and the temporary directory.

The `lane-credential` bootstrap is deliberately fail-closed. One call in the Gateway container runs `lane/pair-device.mjs` (via the pre-existing `/plugin-src` mount) with `/lane-cli`'s state/config and the Gateway's shared token passed only through Docker `exec -e`. The helper connects to the Gateway once through `GatewayClient`, requesting exactly `operator.read`, `operator.write`, and `operator.approvals` at connect time — never the constructor's admin default — so local loopback pairing silently mints a device token with those scopes and nothing wider; it never holds `operator.admin`, not even transiently, and there is no `devices rotate` step (the earlier acp-then-rotate sequence was disproven live: `openclaw acp` requests no scopes and self-pairs as admin, and narrowing an already-admin device afterward needs a caller connection scoped to `operator.pairing`, which no CLI surface exposes non-interactively). The admin CLI then confirms exactly one newly paired device with those live scopes and no pending request left. The lane CLI has its own tokenless config and must subsequently connect without the shared token. A missing pairing, wrong scope set, a leftover pending request, or a failed tokenless connection prints `lane-credential: FAIL — …` and stops `up`. The later `cwd-outside-refused` clause verifies the worker's local decoy workspace refusal and confirms with a device-list diff that no pending request or scope change followed it.

Every clause prints `<name>: PASS|FAIL — <evidence>` and no clause is skipped: `describe-fields`, `agent-choice`, `spawn-open`, `run-answer`, `interrupt`, `close`, `resume-history`, `permission-answered`, `slash-refused`, `directive-neutralized`, `model-session-only`, `thinking-requested`, `cwd-honoured`, `cwd-isolated`, `cwd-outside-refused`, `busy-mid`, `busy-mid-codex`, `lane-voice`, `admin-guard`, `bridge-loss-retain`, and `bridge-loss-mid-run`. `bridge-loss-retain` restarts the Gateway and verifies unchanged completed results after 35 seconds, refusal of new work without a model request, and explicit close. `bridge-loss-mid-run` restarts the Gateway during a SLOW run and verifies its terminal result remains collectable 35 seconds later, accepting either a done result or an unavailable record with a nonempty reason, printing the actual terminal and proving native loss by refusal of new work (`lane bridge lost` or `lane native session exited`) without a model request. `busy-mid-codex` (X1) is `busy-mid` on agent `codexlane` whose runtime is the real codex harness against `../codex-fixture.mjs`: native `sleep` tool active, receipt `written`, `SAW-INJECT` in the same run. `busy-mid` starts a SLOW run, sends a second message to the busy lane, and requires the receipt `written` (the `not_running` steer fallback is a FAIL) and the steered text in the run's own result after its ticks. `lane-voice` makes the lane's own model call the `sessionbus` tool (`SBSEND:<target> <text>` in the mock), which reaches the worker as `bus.action` over the bridge. `admin-guard` serves an admin-state `credentialDir` from a stand-in bridge. `model-session-only` reads the exact row from `sessions.list` (`sessions.get` returns messages, not `spawnedCwd`). The mock records its request bodies in `.state/mock-log/requests.jsonl` for text, model, history, and thinking assertions.

The production worker dials the plugin's bridge socket directly. The Docker fixture tunnels it (`bridge-tunnel.mjs`; unit test `test/bridge-tunnel.test.js`). `run_test.sh` case names: `busy-mid-clause`, `lane-voice-sbsend`, `bridge-handoff`, `early-up-bridge-cleanup`, `load-fixture-bridge-port`, and the rest.
