#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

LANE_SMOKE_ROOT="$PWD/smoke/docker/lane"
export LANE_SMOKE_ROOT
COMPOSE_PROJECT_NAME="oc_lane_smoke_$$"
export COMPOSE_PROJECT_NAME
state="$LANE_SMOKE_ROOT/.state"
bin="$LANE_SMOKE_ROOT/.bin"
fixture="$state/fixture.env"
admin_tmp=""
admin_daemon_pid=""
admin_bridge_pid=""
compose=(docker compose -f smoke/docker/lane/compose.yaml)
export OPENCLAW_IMAGE="${OPENCLAW_IMAGE:-ghcr.io/openclaw/openclaw:${OPENCLAW_VERSION:-2026.8.2}}"
source smoke/docker/codex-lib.sh
clauses=(describe-fields agent-choice spawn-open run-answer interrupt close resume-history permission-answered slash-refused directive-neutralized model-session-only model-refused-lists thinking-requested cwd-honoured cwd-isolated cwd-outside-refused busy-mid busy-mid-codex lane-voice admin-guard bridge-loss-retain bridge-loss-mid-run)

fail() { printf '%s: FAIL — %s\n' "$1" "$2" >&2; exit 1; }
pass() { printf '%s: PASS — %s\n' "$1" "$2"; }
need() { command -v "$1" >/dev/null || fail setup "missing $1"; }
json() { jq -cn "$@"; }
sb() { SESSIONBUS_SOCKET="$socket" sessionbus-call -timeout 35s -g lanesmoke -socket "$socket" "$1" "$2"; }
rpc() { local out; out=$(sb "$1" "$2" 2>&1) || fail "$clause" "$1: ${out:0:500}"; printf '%s' "$out"; }
agent_open() { json --arg id "$1" '{arguments:["--agent",$id]}'; }
spawn() {
  # close_lane only session.close's a lane, never forget — resume-history needs the row
  # still resumable by session id afterward — so a fixed name stays taken in the daemon's directory
  # once any clause spawns it. Every spawn() call (including a second one inside the same clause,
  # e.g. cwd-isolated's two lanes) needs its own name; $RANDOM is re-evaluated per reference, so two
  # calls in the same clause still diverge.
  local out payload name
  name="lane-smoke-${clause}-$RANDOM$RANDOM"
  payload=$(json --arg name "$name" --argjson open "$1" --argjson auto_close "${2:-null}" '{product:"openclaw-peer",name:$name,persistent:true,notify:false,extra_groups:["lanesmoke"],open:$open} | if $auto_close == null then . else . + {auto_close_ms:$auto_close} end')
  out=$(rpc lane.spawn "$payload")
  jq -er '.session_id' <<<"$out" || fail "$clause" "spawn gave no session_id: ${out:0:300}"
}
run() { rpc turn.run "$(json --arg id "$1" --arg input "$2" '{session_id:$id,input:$input}')"; }
answer() {
  local out
  out=$(run "$1" "$2")
  jq -e '.state=="done" and .result.outcome=="completed"' <<<"$out" >/dev/null || fail "$clause" "turn did not complete: ${out:0:500}"
  jq -r '.result.result' <<<"$out"
}
clock_seconds() { date +%s; }
timed_answer() {
  local started finished reply
  started=$(clock_seconds)
  reply=$(answer "$1" "$2")
  finished=$(clock_seconds)
  (( finished-started <= 30 )) || fail "$clause" "EXEC:id completed in $((finished-started)) s, over the 30 s bound"
  printf '%s' "$reply"
}
close_lane() { rpc session.close "$(json --arg id "$1" '{session_id:$id}')" >/dev/null; }
gateway() { "${compose[@]}" exec -T -e OPENCLAW_STATE_DIR=/lane-cli -e OPENCLAW_CONFIG_PATH=/lane-cli/openclaw.json openclaw-gateway node openclaw.mjs gateway call "$1" --json --params "$2"; }
gateway_list_row() {
  # lane.spawn returns the daemon-qualified id (agent:lane:sb-...@lanesmoke),
  # but the Gateway row is keyed by the bare key the wrapper minted (no @group suffix) — searching
  # with the qualified id straight through never matches, so this always returned empty. The
  # model-session-only clause's `[[ -n "$row" ]]` is the first caller that actually requires a
  # non-empty row; directive-neutralized's `[[ "$row" != *mock-b* ]]` passed vacuously on an empty
  # row the whole time, which hid this from every earlier smoke run.
  local key out
  key="${1%@*}"
  out=$(gateway sessions.list "$(json --arg key "$key" '{search:$key}')" 2>&1) || fail "$clause" "sessions.list: ${out:0:400}"
  jq -c --arg key "$key" '(.sessions // .rows // []) | map(select(.key==$key)) | first // empty' <<<"$out"
}
# Fixture log helpers (codex-fixture prints `codex-fixture: ...` lines on stdout).
fixture_lines() { "${compose[@]}" logs --no-color codex-fixture 2>&1 | grep -c -E -- "$1" || true; }
fixture_tail() { "${compose[@]}" logs --no-color codex-fixture 2>&1 | grep -E 'codex-fixture: (req|tool-call|reply)' | tail -4 | cut -c1-160 | tr '\n' '|'; }
# lane_voice_evidence <lane id> <recipient id>: what the Gateway saw of the lane's tool (factory decisions with the session
# key, bridge registrations) and the lane's session row — printed before a lane-voice failure stops the run.
lane_voice_evidence() {
  local key=${1%@*}
  {
    echo "lane-voice evidence: lane session key $key; recipient $2"
    echo "-- gateway: sessionbus lines for this lane (tool factory decisions, registrations, warnings):"
    "${compose[@]}" logs --no-color openclaw-gateway 2>&1 | grep -F -- "$key" | grep -E 'sessionbus|tool' | tail -15 | cut -c1-300
    echo "-- gateway: last sessionbus lines:"
    "${compose[@]}" logs --no-color openclaw-gateway 2>&1 | grep -E 'sessionbus:' | tail -15 | cut -c1-300
    echo "-- lane session row:"
    gateway_list_row "$1" 2>/dev/null | cut -c1-500
  } >&2 || true
}
mock_log() { cat "$state/mock-log/requests.jsonl" 2>/dev/null || true; }
# The mock log is shared across every agent's model requests, with no per-recipient
# field to filter by; mock_log_mark/mock_log_since scope a check to requests recorded after a
# known point, so a marker left over from an unrelated earlier run (or clause) in the same log
# file cannot satisfy it.
mock_log_mark() { awk 'END{print NR+0}' "$state/mock-log/requests.jsonl" 2>/dev/null || echo 0; }
mock_log_since() { tail -n "+$(($1 + 1))" "$state/mock-log/requests.jsonl" 2>/dev/null || true; }
audit_mark() { if [[ -f "$state/permission-audit.log" ]]; then awk 'END{print NR+0}' "$state/permission-audit.log"; else printf '0\n'; fi; }
audit_since() { tail -n "+$(($1 + 1))" "$state/permission-audit.log" 2>/dev/null || true; }
# A request is the *recipient's own* only when its
# lastUser also carries that agent's workspace root — OpenClaw prefixes every ACP prompt with
# "[Working directory: <cwd>]" (8.2 translator.prompt-stream.ts), so this distinguishes b's wake
# request from one that landed back on the sending lane (which would carry $lane_root instead).
recipient_marker_seen() {
  mock_log_since "$1" | jq -se --arg marker "$2" --arg root "$3" \
    'any(.[]; (.lastUser // "") as $u | ($u | contains($marker)) and ($u | contains($root)))' >/dev/null
}

# The plugin restart closes old worker connections. Prove loss with the
# clause's refused Run after checking retention; worker stderr is not in daemon.log.
restart_bridge_loss() {
  "${compose[@]}" restart openclaw-gateway || fail "$clause" "Gateway restart failed"
  wait_bridge
}

provision_credential() {
  # The earlier ACP-then-rotate approach is unsafe: `openclaw acp` requests no scopes
  # and self-pairs as admin, while `devices rotate` afterwards is denied. This helper
  # connects once through GatewayClient with exactly
  # operator.read/write/approvals requested at connect time, so silent local pairing mints a token
  # with those scopes and nothing wider, never transiently admin.
  local before after id scopes pending_before pending_after helper_out helper_status
  before=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs devices list --json 2>&1) || fail lane-credential "admin list before pairing: ${before:0:350}"
  jq -e '((.paired // []) | type=="array") and ((.pending // []) | type=="array")' <<<"$before" >/dev/null \
    || fail lane-credential "admin list before pairing has no paired/pending array"
  pending_before=$(jq -r '(.pending // []) | length' <<<"$before") || fail lane-credential "cannot parse pending count before pairing"
  # Only this call supplies the shared secret to /lane-cli. The name-only `-e
  # OPENCLAW_GATEWAY_TOKEN` takes the value from this shell's own env (exported on the line below),
  # so the token never sits in this process's argv, where a concurrent `ps` on the host could
  # read it (the container-side child still gets it as a normal env var either way). It is never
  # written to /lane-cli, a worker command, fixture.env, or a log (lane/pair-device.mjs reads
  # OPENCLAW_GATEWAY_TOKEN from its own env only).
  [[ "$gateway_token" =~ ^[0-9a-f]{64}$ ]] || fail lane-credential "generated shared token missing"
  # Under `set -e`, `var=$(cmd)` as a bare statement propagates `cmd`'s exit status —
  # without `helper_status=0` seeded first and `|| helper_status=$?` on the same line, a nonzero
  # helper exit aborted the script right here, before `fail` could ever print `lane-credential:
  # FAIL — …` (the exact failure this step needs to report, and the case `up`'s own
  # failure trap is supposed to make visible).
  helper_status=0
  helper_out=$(OPENCLAW_GATEWAY_TOKEN="${gateway_token}" "${compose[@]}" exec -T -e OPENCLAW_STATE_DIR=/lane-cli -e OPENCLAW_CONFIG_PATH=/lane-cli/openclaw.json -e OPENCLAW_INSTALL_DIR=/app -e OPENCLAW_GATEWAY_TOKEN openclaw-gateway node /plugin-src/lane/pair-device.mjs 2>&1) || helper_status=$?
  unset gateway_token
  (( helper_status == 0 )) || fail lane-credential "pair-device helper: ${helper_out:0:350}"
  after=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs devices list --json 2>&1) || fail lane-credential "admin list after pairing: ${after:0:350}"
  id=$(jq -nr --argjson before "$before" --argjson after "$after" '$after.paired | map(.deviceId) - ($before.paired | map(.deviceId)) | if length==1 then .[0] else empty end') || fail lane-credential "cannot parse paired device lists"
  [[ -n "$id" ]] || fail lane-credential "expected exactly one newly paired lane device: ${helper_out:0:200}"
  scopes=$(jq -cr --arg id "$id" '[.paired[] | select(.deviceId==$id) | .tokens[]? | select(.role=="operator" and (.revokedAtMs==null)) | .scopes | sort] | first // []' <<<"$after") || fail lane-credential "cannot parse paired scope list"
  [[ "$scopes" == '["operator.approvals","operator.read","operator.write"]' ]] || fail lane-credential "paired scopes were $scopes, expected exactly read/write/approvals (no admin, no rotate)"
  pending_after=$(jq -r '(.pending // []) | length' <<<"$after") || fail lane-credential "cannot parse pending count after pairing"
  [[ "$pending_after" == "$pending_before" ]] || fail lane-credential "pairing left a pending request (before=$pending_before after=$pending_after); no fallback to admin, no rotate"
  "${compose[@]}" exec -T -e OPENCLAW_STATE_DIR=/lane-cli -e OPENCLAW_CONFIG_PATH=/lane-cli/openclaw.json openclaw-gateway node openclaw.mjs gateway call status --json >/dev/null 2>&1 || fail lane-credential "lane CLI cannot connect tokenless after pairing"
  pass lane-credential "paired device $id has exactly operator.read/write/approvals; tokenless lane CLI connects"
}

# One settings.get over the host tunnel: proves host socket -> tunnel -> the plugin's bridge in the Gateway, end to end.
bridge_settings() {
  node - "$host_bridge" <<'NODE'
const net = require('node:net');
const socket = net.createConnection(process.argv[2]);
let buffer = '';
const timer = setTimeout(() => { console.error('no reply within 5 s'); process.exit(1); }, 5000);
socket.setEncoding('utf8');
socket.on('error', (error) => { console.error(error.message); process.exit(1); });
socket.on('connect', () => socket.write(JSON.stringify({ v: 1, id: 'w:1', method: 'settings.get', params: {} }) + '\n'));
socket.on('data', (chunk) => {
  buffer += chunk;
  const end = buffer.indexOf('\n');
  if (end < 0) return;
  clearTimeout(timer);
  const frame = JSON.parse(buffer.slice(0, end));
  if (!frame.result) { console.error(JSON.stringify(frame.error ?? frame)); process.exit(1); }
  process.stdout.write(JSON.stringify(frame.result));
  socket.destroy();
});
NODE
}
wait_bridge() {
  local out deadline=$((SECONDS+60))
  until out=$(bridge_settings 2>&1); do
    (( SECONDS < deadline )) || fail setup "plugin bridge never answered settings.get over the tunnel: ${out:0:300}"
    sleep 2
  done
  jq -e '(.agents | map(.id) | sort) == ["b","codexlane","decoy","lane"] and .credentialDir=="/lane-cli"' <<<"$out" >/dev/null || fail setup "bridge settings.get returned unexpected settings: ${out:0:400}"
  pass bridge "settings.get over the tunnel: agents lane, b, decoy, codexlane; credentialDir /lane-cli"
}

lane_ownership_mode() {
  local mode="${LANE_SMOKE_OWNERSHIP:-}" tag="${OPENCLAW_IMAGE##*:}"
  if [[ -z "$mode" ]]; then
    mode=explicit
    if [[ "$tag" =~ ^v?([0-9]{4})\.([0-9]+)(\.|$|-) ]]; then
      if (( 10#${BASH_REMATCH[1]} < 2026 || (10#${BASH_REMATCH[1]} == 2026 && 10#${BASH_REMATCH[2]} < 10) )); then
        mode=marker
      fi
    fi
  fi
  case "$mode" in
    marker|explicit) printf '%s\n' "$mode" ;;
    *) fail setup "LANE_SMOKE_OWNERSHIP must be marker or explicit" ;;
  esac
}

render_lane_config() {
  node - "$@" <<'NODE'
const fs = require('node:fs');
const vm = require('node:vm');
const [source, dest, lane, b, decoy, token, audit, ownership] = process.argv.slice(2);
let config = fs.readFileSync(source, 'utf8');
for (const [key, value] of Object.entries({__LANE_ROOT__:lane,__B_ROOT__:b,__DECOY_REAL_ROOT__:decoy,'{{GATEWAY_TOKEN}}':token,__AUDIT_LOG__:audit})) {
  config = config.replaceAll(key, value);
}
const rendered = vm.runInNewContext('(' + config + ')');
if (ownership === 'explicit') {
  for (const entry of Object.values(rendered.agents.entries)) delete entry.default;
  rendered.agents.ownership = 'explicit';
  for (const key of ['heartbeat', 'systemAgent', 'authInheritance']) {
    rendered.agents.defaults[key] = {agentId:'lane'};
  }
}
config = JSON.stringify(rendered, null, 2) + '\n';
fs.writeFileSync(dest, config, {mode:0o600});
NODE
}

up() {
  [[ ! -e "$fixture" ]] || fail setup "fixture already up; run down first"
  local ownership
  ownership=$(lane_ownership_mode) || return 1
  for tool in docker node npm jq go sessionbus sessionbus-call openssl; do need "$tool"; done
  mkdir -p "$state/ws/lane/sub" "$state/ws/b/sub" "$state/ws/decoy-real" "$state/decoy/x" "$state/mock-log" "$state/pkg" "$state/config" "$bin"
  chmod -R a+rwX "$state/ws" "$state/decoy" "$state/mock-log"
  local lane_root b_root decoy_root decoy_real_root
  lane_root="$state/ws/lane"; b_root="$state/ws/b"; decoy_root="$state/decoy"; decoy_real_root="$state/ws/decoy-real"
  # Write state to the fixture as each resource is created, not only once at the
  # end of up() — otherwise a failure partway through (plugin install, healthcheck, credential
  # provisioning, go build, a daemon-socket timeout) leaves no fixture at all, and down() (called
  # by the up-failure trap) has nothing to kill: the host bridge's TCP port, the daemon's socket
  # and $tmp all leak, silently, since the final `rm -rf "$state"` also deletes the bridge's log.
  {
    printf 'COMPOSE_PROJECT_NAME=%q\n' "$COMPOSE_PROJECT_NAME"
    printf 'lane_root=%q\nb_root=%q\ndecoy_root=%q\ndecoy_real_root=%q\n' "$lane_root" "$b_root" "$decoy_root" "$decoy_real_root"
  } >"$fixture"
  chmod 600 "$fixture"
  # The worker's bridge socket on the host: a short /tmp path (Darwin sun_path <= 103 bytes), reached through
  # bridge-tunnel.mjs. `host_bridge` is what the daemon and its workers get as SESSIONBUS_OPENCLAW_BRIDGE.
  bridge_dir=$(mktemp -d /tmp/ocl-br.XXXXXX)
  printf 'bridge_dir=%q\n' "$bridge_dir" >>"$fixture"
  chmod 700 "$bridge_dir"
  host_bridge="$bridge_dir/b/bridge.sock"
  printf 'host_bridge=%q\n' "$host_bridge" >>"$fixture"
  LANE_BRIDGE_PORT=$((40000 + $$ % 20000))
  export LANE_BRIDGE_PORT
  printf 'LANE_BRIDGE_PORT=%q\n' "$LANE_BRIDGE_PORT" >>"$fixture"
  gateway_token=$(openssl rand -hex 32)
  render_lane_config "$LANE_SMOKE_ROOT/openclaw.json5" "$state/config/openclaw.json" "$lane_root" "$b_root" "$decoy_real_root" "$gateway_token" "$state/permission-audit.log" "$ownership"
  # The file carries the live Gateway admin token (openclaw.json5's
  # {{GATEWAY_TOKEN}} substitution above); a+rw made it world-writable for the fixture's
  # lifetime. chown it to the container's own uid (matching the /lane-cli chown below) via a
  # throwaway root run before the container that needs to write it (`plugins install --force`)
  # ever starts, then keep it 0600.
  # Chown the whole mounted config dir, not just the file — plugins install --force
  # replaces the file via a same-dir temp-file-then-rename, and the new temp file needs the
  # container uid on the directory to be created and renamed at all, not just on the final name.
  "${compose[@]}" run --rm --no-deps -T -u root openclaw-setup chown -R node:node /home/node/config || fail setup "cannot chown the Gateway config dir for the container uid"
  chmod 600 "$state/config/openclaw.json"
  local packed filename
  packed=$(npm pack --json --pack-destination "$state/pkg") || fail setup "cannot pack sessionbus plugin"
  filename=$(jq -er '.[0].filename' <<<"$packed") || fail setup "npm pack returned no filename"
  "${compose[@]}" run --rm --no-deps -T openclaw-setup node openclaw.mjs plugins install "/plugin-pkg/$filename" --force --accept-capabilities || fail setup "sessionbus plugin install failed"
  # The host half of the tunnel: the worker's unix socket, forwarded to the sidecar's published loopback port.
  SESSIONBUS_OPENCLAW_BRIDGE="$host_bridge" node "$LANE_SMOKE_ROOT/bridge-tunnel.mjs" host >"$state/bridge-tunnel.log" 2>&1 &
  bridge_tunnel_pid=$!
  printf 'bridge_tunnel_pid=%q\n' "$bridge_tunnel_pid" >>"$fixture"
  sleep .3
  kill -0 "$bridge_tunnel_pid" 2>/dev/null || fail setup "host bridge tunnel exited: $(tail -c 300 "$state/bridge-tunnel.log")"
  "${compose[@]}" up -d
  local deadline=$((SECONDS+120))
  until "${compose[@]}" exec -T openclaw-gateway node dist/docker-healthcheck.js >/dev/null 2>&1; do
    (( SECONDS < deadline )) || fail setup "Gateway healthcheck timed out"
    sleep 2
  done
  local inspected
  inspected=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs plugins inspect sessionbus --runtime --json 2>&1) || fail setup "sessionbus plugin inspect failed: ${inspected:0:350}"
  jq -e '.plugin.id=="sessionbus" and .plugin.status=="loaded"' <<<"$inspected" >/dev/null || fail setup "sessionbus plugin not loaded: ${inspected:0:350}"
  wait_bridge
  "${compose[@]}" exec -T -u root openclaw-gateway chown node:node /lane-cli || fail lane-credential "cannot make /lane-cli writable for the device state"
  provision_credential
  (cd lane && go build -mod=vendor -o "$bin/openclaw-peer" ./cmd/openclaw-peer)
  cat >"$bin/openclaw" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
exec docker compose -f "$LANE_SMOKE_ROOT/compose.yaml" exec -T \
  -e OPENCLAW_STATE_DIR="$OPENCLAW_STATE_DIR" \
  -e OPENCLAW_CONFIG_PATH="$OPENCLAW_CONFIG_PATH" \
  openclaw-gateway node openclaw.mjs "$@"
SH
  chmod 700 "$bin/openclaw"
  tmp=$(mktemp -d /tmp/ocl.XXXXXX)
  socket="$tmp/p.sock"
  printf 'tmp=%q\nsocket=%q\n' "$tmp" "$socket" >>"$fixture"
  PATH="$bin:$PATH" OPENCLAW_STATE_DIR="$state/oc-state" SESSIONBUS_OPENCLAW_BRIDGE="$host_bridge" XDG_CONFIG_HOME="$tmp/xdg-config" XDG_STATE_HOME="$tmp/xdg" sessionbus --socket "$socket" --table "$tmp/table" --host lanesmoke >"$tmp/daemon.log" 2>&1 &
  daemon_pid=$!
  printf 'daemon_pid=%q\n' "$daemon_pid" >>"$fixture"
  deadline=$((SECONDS+15))
  until [[ -S "$socket" ]]; do
    kill -0 "$daemon_pid" 2>/dev/null || fail setup "private daemon exited: $(tail -c 300 "$tmp/daemon.log")"
    (( SECONDS < deadline )) || fail setup "private daemon socket timed out"
    sleep .1
  done
  pass setup "private daemon $socket; Docker fixture ready; ownership=$ownership"
}

load_fixture() {
  [[ -f "$fixture" ]] || fail setup "fixture.env missing; run up"
  # The file contains only shell-quoted values written by up, never credentials.
  source "$fixture"
  # A single-clause run never re-derives the tunnel port on its own; without it exported, a compose command that
  # recreates the bridge-tunnel container would interpolate compose.yaml's default port instead of the real one.
  export COMPOSE_PROJECT_NAME LANE_BRIDGE_PORT
  kill -0 "$daemon_pid" 2>/dev/null || fail setup "private daemon is not running"
  kill -0 "$bridge_tunnel_pid" 2>/dev/null || fail setup "host bridge tunnel is not running"
}
down() {
  if [[ -f "$fixture" ]]; then source "$fixture"; export COMPOSE_PROJECT_NAME LANE_BRIDGE_PORT; fi
  if [[ -n "$admin_daemon_pid" ]]; then kill "$admin_daemon_pid" 2>/dev/null || true; fi
  if [[ -n "$admin_bridge_pid" ]]; then kill "$admin_bridge_pid" 2>/dev/null || true; fi
  if [[ -n "$admin_tmp" ]]; then rm -rf "$admin_tmp"; fi
  if [[ -n "${daemon_pid:-}" ]]; then kill "$daemon_pid" 2>/dev/null || true; fi
  if [[ -n "${bridge_tunnel_pid:-}" ]]; then kill "$bridge_tunnel_pid" 2>/dev/null || true; fi
  "${compose[@]}" down -v >/dev/null 2>&1 || true
  if [[ -n "${tmp:-}" ]]; then rm -rf "$tmp"; fi
  if [[ -n "${bridge_dir:-}" ]]; then rm -rf "$bridge_dir"; fi
  rm -rf "$state" "$bin"
}

cleanup_all() {
  local status=$?
  trap - EXIT
  down
  exit "$status"
}

cleanup_admin_guard() {
  if [[ -n "$admin_daemon_pid" ]]; then kill "$admin_daemon_pid" 2>/dev/null || true; fi
  if [[ -n "$admin_bridge_pid" ]]; then kill "$admin_bridge_pid" 2>/dev/null || true; fi
  if [[ -n "$admin_tmp" ]]; then rm -rf "$admin_tmp"; fi
}

check_clause() {
  clause="$1"
  local out id id2 text row start ref receipt first second default_hash bare_key
  local before_devices after_devices before_pending after_pending before_scopes after_scopes
  local admin_socket admin_out
  case "$clause" in
    describe-fields)
      out=$(rpc lane.describe '{"product":"openclaw-peer"}')
      jq -e '.supported_open_fields==["cwd","permission_mode","model","reasoning_effort","arguments"] and .extra_arguments[0].name=="--agent" and (.extra_arguments[0].description | contains("lane — Primary lane agent") and contains("b — Second lane agent") and contains("decoy — Containment probe"))' <<<"$out" >/dev/null || fail "$clause" "describe omitted a field or agent role: ${out:0:400}"
      jq -e '.extra_arguments[0].description | test("lane — Primary lane agent \\[models: [^]]*; default: mock/mock-a\\]")' <<<"$out" >/dev/null || fail "$clause" "lane agent omitted its default model: ${out:0:500}"
      pass "$clause" "five open fields, three --agent roles, and lane default mock/mock-a"
      ;;
    agent-choice)
      out=$(sb lane.spawn "$(json '{product:"openclaw-peer",name:"no-agent",open:{}}')" 2>&1) && fail "$clause" "missing --agent was accepted"
      [[ "$out" == *"agent required"* && "$out" == *"lane — Primary lane agent"* && "$out" == *"b — Second lane agent"* && "$out" == *"decoy — Containment probe"* ]] || fail "$clause" "missing-agent error did not name all agents: ${out:0:350}"
      id=$(spawn "$(agent_open b)")
      [[ "$id" == agent:b:sb-* ]] || fail "$clause" "--agent b minted $id"
      close_lane "$id"
      pass "$clause" "omission refused; --agent b minted $id"
      ;;
    spawn-open|run-answer|close)
      id=$(spawn "$(agent_open lane)")
      [[ "$id" == agent:lane:sb-* ]] || fail "$clause" "bad id $id"
      if [[ "$clause" == run-answer ]]; then
        text=$(answer "$id" 'hello ECHO')
        [[ "$text" == *"ECHO[model="* && "$text" == *"hello ECHO"* ]] || fail "$clause" "wrong answer: ${text:0:300}"
      fi
      close_lane "$id"
      pass "$clause" "$id ${text:-opened and closed}"
      ;;
    interrupt)
      id=$(spawn "$(agent_open lane)")
      start=$(rpc turn.start "$(json --arg id "$id" '{session_id:$id,input:"SLOW"}')")
      ref=$(jq -er '.run_id' <<<"$start")
      sleep 2
      rpc turn.interrupt "$(json --arg id "$id" '{session_id:$id}')" >/dev/null
      out=$(rpc turn.wait "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref,timeout_ms:10000}')")
      jq -e '.state=="done" and .result.outcome=="interrupted"' <<<"$out" >/dev/null || fail "$clause" "not interrupted: ${out:0:350}"
      answer "$id" 'after interrupt' >/dev/null
      close_lane "$id"
      pass "$clause" "interrupted run $ref; next run completed"
      ;;
    resume-history)
      id=$(spawn "$(agent_open lane)")
      answer "$id" 'remember PAPAYA' >/dev/null
      close_lane "$id"
      out=$(rpc lane.spawn "$(json --arg id "$id" '{resume_session_id:$id,persistent:true,notify:false}')")
      [[ "$(jq -r '.session_id' <<<"$out")" == "$id" ]] || fail "$clause" "resume changed id: ${out:0:250}"
      answer "$id" 'which word did I give?' >/dev/null
      mock_log | jq -se '.[-1].messages | any(.[]; .role=="user" and ((.content|tostring)|contains("PAPAYA")))' >/dev/null || fail "$clause" "resumed model request lacks earlier PAPAYA history"
      close_lane "$id"
      pass "$clause" "$id resumed with earlier prompt in model request"
      ;;
    permission-answered)
      audit_start=$(audit_mark)
      id=$(spawn "$(json '{arguments:["--agent","lane"],permission_mode:"default"}')")
      text=$(timed_answer "$id" 'EXEC:id')
      [[ "$text" == TOOL-DONE:* ]] || fail "$clause" "default reply did not render the tool result: ${text:0:350}"
      mock_log | jq -se '.[-1] as $last | ($last.toolResult == true) and ([$last.messages[]? | select(.role=="tool") | (.content|tostring)] | any(.[]; test("denied|rejected|not approved";"i")))' >/dev/null || fail "$clause" "mock request lacks a denied exec tool result"
      audit_since "$audit_start" | grep 'permission default .* -> deny' >/dev/null || fail "$clause" "audit log has no default denial"
      close_lane "$id"
      id=$(spawn "$(json '{arguments:["--agent","lane"],permission_mode:"bypassPermissions"}')")
      text=$(timed_answer "$id" 'EXEC:id')
      [[ "$text" == *"uid="* ]] || fail "$clause" "bypass result has no id output: ${text:0:350}"
      close_lane "$id"
      pass "$clause" "default denied in result and log; bypass returned uid"
      ;;
    slash-refused)
      id=$(spawn "$(agent_open lane)")
      out=$(run "$id" '/model mock/mock-b')
      [[ "$out" == *"slash commands are not accepted over the lane"* ]] || fail "$clause" "slash command reached child: ${out:0:350}"
      close_lane "$id"
      pass "$clause" "leading slash refused"
      ;;
    directive-neutralized)
      id=$(spawn "$(agent_open lane)")
      answer "$id" $'hello\n/model mock/mock-b' >/dev/null
      receipt=$(rpc message.send "$(json --arg id "$id" --arg msg $'hello\n/model mock/mock-b' '{target:$id,message:$msg}')")
      [[ "$receipt" == *"deliveries"* ]] || fail "$clause" "delivery has no receipt"
      local deadline=$((SECONDS+30))
      until mock_log | jq -se 'map(select(.lastUser|contains("\u200b/model mock/mock-b"))) | length>=2' >/dev/null; do
        (( SECONDS < deadline )) || fail "$clause" "Text and Delivery were not both neutralized in ECHO requests"
        sleep 1
      done
      # The mock log only proves the woken Delivery turn's prompt reached the
      # model \u2014 never that OpenClaw's own turn bookkeeping for it has finished. A possible race
      # is exactly that gap: close_lane racing a turn the Gateway still considers running. Wait for
      # session.list's own `running` flag (the SDK's single-active-run state, not a content proxy
      # like the mock log) to clear before closing.
      deadline=$((SECONDS+30))
      until [[ "$(rpc session.list "$(json --arg id "$id" '{session_id:$id}')" | jq -r '.sessions[0].running // false')" == "false" ]]; do
        (( SECONDS < deadline )) || fail "$clause" "woken Delivery turn never finished (session.list still running)"
        sleep 1
      done
      row=$(gateway_list_row "$id")
      [[ "$row" != *'"model":"mock-b"'* ]] || fail "$clause" "directive switched the model"
      close_lane "$id"
      pass "$clause" "Text and Delivery carried U+200B; model unchanged; woken turn finished before close"
      ;;
    model-session-only)
      default_hash=$(shasum -a 256 "$state/config/openclaw.json" | cut -d' ' -f1)
      id=$(spawn "$(json '{arguments:["--agent","lane"],model:"mock/mock-b"}')")
      row=$(gateway_list_row "$id")
      [[ -n "$row" ]] || fail "$clause" "sessions.patch did not create the row"
      jq -e '(.model=="mock-b" or .model=="mock/mock-b") and (.spawnedCwd==null)' <<<"$row" >/dev/null || fail "$clause" "model row or spawnedCwd wrong: ${row:0:350}"
      text=$(answer "$id" 'model probe')
      [[ "$text" == *"ECHO[model=mock-b]"* ]] || fail "$clause" "lane did not use mock-b: ${text:0:250}"
      id2=$(spawn "$(agent_open b)")
      text=$(answer "$id2" 'neighbour model')
      [[ "$text" == *"ECHO[model=mock-a]"* ]] || fail "$clause" "neighbour model changed: ${text:0:250}"
      [[ "$(shasum -a 256 "$state/config/openclaw.json" | cut -d' ' -f1)" == "$default_hash" ]] || fail "$clause" "configured default changed"
      close_lane "$id"; close_lane "$id2"
      pass "$clause" "patch auto-vivified $id without spawnedCwd; mock-b lane, mock-a neighbour, config unchanged"
      ;;
    model-refused-lists)
      out=$(sb lane.spawn "$(json '{product:"openclaw-peer",name:"model-refused",open:{arguments:["--agent","decoy"],model:"mock/mock-b"}}')" 2>&1) && fail "$clause" "decoy accepted mock/mock-b"
      [[ "$out" == *"allowed for decoy: mock/mock-a"* && "$out" == *"default: mock/mock-a"* ]] || fail "$clause" "model refusal omitted allowlist or default: ${out:0:500}"
      out=$(rpc lane.describe '{"product":"openclaw-peer"}')
      jq -e '.extra_arguments[0].description | contains("decoy — Containment probe [models: mock/mock-a;")' <<<"$out" >/dev/null || fail "$clause" "decoy describe omitted its allowlist: ${out:0:500}"
      pass "$clause" "decoy refused mock-b with allowlist and default; describe lists mock/mock-a"
      ;;
    thinking-requested)
      id=$(spawn "$(json '{arguments:["--agent","lane"],reasoning_effort:"low"}')")
      answer "$id" 'thinking probe' >/dev/null
      mock_log | jq -se '.[-1].thinking=="low"' >/dev/null || fail "$clause" "model request log did not show low thinking level"
      close_lane "$id"
      id=$(spawn "$(json '{arguments:["--agent","b"],reasoning_effort:"xhigh"}')")
      out=$(run "$id" 'unsupported thinking probe')
      jq -e '.state=="done" and .result.outcome=="failed" and (.result.result|startswith("unsupported value reasoning_effort=xhigh"))' <<<"$out" >/dev/null || fail "$clause" "unsupported thinking level did not fail: ${out:0:350}"
      close_lane "$id"
      pass "$clause" "low recorded in model request; xhigh refused on mock-b"
      ;;
    cwd-honoured)
      id=$(spawn "$(json --arg cwd "$lane_root/sub" '{arguments:["--agent","lane"],cwd:$cwd,permission_mode:"bypassPermissions"}')")
      text=$(answer "$id" 'EXEC:pwd')
      [[ "$text" == *"$lane_root/sub"* ]] || fail "$clause" "pwd was ${text:0:300}"
      close_lane "$id"
      pass "$clause" "pwd=$lane_root/sub"
      ;;
    cwd-isolated)
      id=$(spawn "$(json --arg cwd "$lane_root/sub" '{arguments:["--agent","lane"],cwd:$cwd,permission_mode:"bypassPermissions"}')")
      id2=$(spawn "$(json --arg cwd "$b_root/sub" '{arguments:["--agent","b"],cwd:$cwd}')")
      first=$(answer "$id" 'EXEC:pwd'); second=$(answer "$id2" 'EXEC:pwd')
      [[ "$first" == *"$lane_root/sub"* && "$second" == *"$b_root/sub"* ]] || fail "$clause" "concurrent cwd: ${first:0:180}; ${second:0:180}"
      close_lane "$id"
      out=$(rpc lane.spawn "$(json --arg id "$id" '{resume_session_id:$id,persistent:true,notify:false}')")
      [[ "$(jq -r '.session_id' <<<"$out")" == "$id" ]] || fail "$clause" "resume id changed"
      text=$(answer "$id" 'EXEC:pwd')
      [[ "$text" == *"$lane_root/sub"* ]] || fail "$clause" "resume lost cwd: ${text:0:300}"
      close_lane "$id"; close_lane "$id2"
      pass "$clause" "two pre-opened lanes had distinct pwd; resume kept $lane_root/sub"
      ;;
    cwd-outside-refused)
      out=$(sb lane.spawn "$(json --arg cwd "$b_root" '{product:"openclaw-peer",name:"outside",open:{arguments:["--agent","lane"],cwd:$cwd}}')" 2>&1) && fail "$clause" "lane accepted b workspace"
      [[ "$out" == *"outside the workspace of agent lane"* ]] || fail "$clause" "chosen-agent refusal wrong: ${out:0:350}"
      # The decoy workspace now comes from OpenClaw's resolved decoy-real path. The worker rejects
      # decoy_root/x locally, before a Gateway call; Gateway-side platform refusal is unreachable
      # through the worker. Keep the device-list diff to prove no pending request or scope change.
      before_devices=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs devices list --json 2>&1) || fail "$clause" "admin devices list before decoy: ${before_devices:0:350}"
      before_pending=$(jq -r '(.pending // []) | length' <<<"$before_devices") || fail "$clause" "cannot parse pending count before decoy"
      before_scopes=$(jq -cr '[.paired[] | .tokens[]? | .scopes // [] | sort]' <<<"$before_devices") || fail "$clause" "cannot parse paired scopes before decoy"
      out=$(sb lane.spawn "$(json --arg cwd "$decoy_root/x" '{product:"openclaw-peer",name:"decoy",open:{arguments:["--agent","decoy"],cwd:$cwd}}')" 2>&1) && fail "$clause" "worker accepted decoy path"
      [[ "$out" == *"outside the workspace of agent decoy"* ]] || fail "$clause" "decoy workspace refusal wrong: ${out:0:450}"
      after_devices=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs devices list --json 2>&1) || fail "$clause" "admin devices list after decoy: ${after_devices:0:350}"
      after_pending=$(jq -r '(.pending // []) | length' <<<"$after_devices") || fail "$clause" "cannot parse pending count after decoy"
      after_scopes=$(jq -cr '[.paired[] | .tokens[]? | .scopes // [] | sort]' <<<"$after_devices") || fail "$clause" "cannot parse paired scopes after decoy"
      [[ "$after_pending" == "$before_pending" ]] || fail "$clause" "the refused cwd left a pending request (before=$before_pending after=$after_pending)"
      [[ "$after_scopes" == "$before_scopes" ]] || fail "$clause" "the lane device's scopes changed (before=$before_scopes after=$after_scopes)"
      pass "$clause" "both cwd checks refused locally; no pending request, scopes unchanged"
      ;;
    busy-mid)
      # BUSY-MID on a lane: a message sent while a run is active is steered into that run (worker -> plugin
      # lane.steer -> OpenClaw), acknowledged `written`, and the run itself reacts before its own terminal. The
      # fallback (`not_running`, "steer fallback (not BUSY-MID)") is a FAIL here: on the embedded runtime it must not fire.
      id=$(spawn "$(agent_open lane)")
      local busy_mark busy_marker busy_ref busy_deadline
      busy_mark=$(mock_log_mark)
      start=$(rpc turn.start "$(json --arg id "$id" '{session_id:$id,input:"SLOW busy-mid"}')")
      busy_ref=$(jq -er '.run_id' <<<"$start")
      busy_deadline=$((SECONDS+60))
      until mock_log_since "$busy_mark" | jq -se 'any(.[]; (.lastUser // "") | contains("SLOW busy-mid"))' >/dev/null; do
        (( SECONDS < busy_deadline )) || fail "$clause" "the SLOW run never reached the model"
        sleep 1
      done
      sleep 3
      busy_marker="INJECT-MARK busy-mid $RANDOM"
      receipt=$(rpc message.send "$(json --arg id "$id" --arg msg "$busy_marker" '{target:$id,message:$msg}')")
      jq -e --arg id "$id" '.deliveries | any(.[]; .session_id==$id and .disposition=="written")' <<<"$receipt" >/dev/null || fail "$clause" "message to the busy lane was not acknowledged written (not_running = the steer fallback fired): ${receipt:0:400}"
      out=$(rpc turn.wait "$(json --arg id "$id" --arg ref "$busy_ref" '{session_id:$id,run_id:$ref,timeout_ms:90000}')")
      jq -e '.state=="done" and .result.outcome=="completed"' <<<"$out" >/dev/null || fail "$clause" "the busy run did not complete: ${out:0:400}"
      text=$(jq -r '.result.result' <<<"$out")
      [[ "$text" == *tick* && "$text" == *"$busy_marker"* ]] || fail "$clause" "the run's own result does not carry the steered message after its ticks: ${text:0:500}"
      mock_log_since "$busy_mark" | jq -se --arg m "$busy_marker" '[.[] | select((.lastUser // "") | contains($m))] | length >= 1' >/dev/null || fail "$clause" "no model request carried the steered message"
      if grep -rqs 'steer fallback (not BUSY-MID)' "$tmp/daemon.log" 2>/dev/null || "${compose[@]}" logs --no-color openclaw-gateway 2>&1 | grep -q 'steer fallback (not BUSY-MID)'; then
        fail "$clause" "the steer fallback line is present in the worker or Gateway log"
      fi
      close_lane "$id"
      pass "$clause" "lane $id run $busy_ref: SLOW active, '$busy_marker' acknowledged written, reaction inside the run's own result before its terminal; no fallback line"
      ;;
    busy-mid-codex)
      # X1: BUSY-MID on a lane whose runtime is the REAL codex harness (the model API is codex-fixture). The fixture
      # answers the SLOW task with a native `sleep` tool call; a message sent during it must be acknowledged `written`
      # and reach the model in the same turn (SAW-INJECT after the tool output). The steer fallback is a FAIL.
      egress_blocked || fail "$clause" "the Gateway container can reach the internet; refusing to run a codex turn (the dummy key must only ever reach the fixture)"
      write_codex_home codexlane || fail "$clause" "cannot write codex-home/config.toml for codexlane"
      id=$(spawn "$(agent_open codexlane)")
      local cx_tools cx_marker cx_ref cx_deadline
      cx_tools=$(fixture_lines 'tool-call n=')
      start=$(rpc turn.start "$(json --arg id "$id" '{session_id:$id,input:"SLOW busy-mid-codex"}')")
      cx_ref=$(jq -er '.run_id' <<<"$start")
      cx_deadline=$((SECONDS+120))
      until (( $(fixture_lines 'tool-call n=') > cx_tools )); do
        (( SECONDS < cx_deadline )) || fail "$clause" "codex never asked the fixture for the native tool (the lane turn did not reach the codex harness): $(fixture_tail)"
        sleep 1
      done
      sleep 3
      cx_marker="INJECT-MARK busy-mid-codex $RANDOM"
      receipt=$(rpc message.send "$(json --arg id "$id" --arg msg "$cx_marker" '{target:$id,message:$msg}')")
      jq -e --arg id "$id" '.deliveries | any(.[]; .session_id==$id and .disposition=="written")' <<<"$receipt" >/dev/null || fail "$clause" "message to the busy codex lane was not acknowledged written (not_running = the steer fallback fired): ${receipt:0:400}"
      out=$(rpc turn.wait "$(json --arg id "$id" --arg ref "$cx_ref" '{session_id:$id,run_id:$ref,timeout_ms:120000}')")
      jq -e '.state=="done" and .result.outcome=="completed"' <<<"$out" >/dev/null || fail "$clause" "the codex run did not complete: ${out:0:400}; $(fixture_tail)"
      text=$(jq -r '.result.result' <<<"$out")
      [[ "$text" == *SAW-INJECT* ]] || fail "$clause" "the run's result lacks SAW-INJECT (the steered message did not reach the model in this turn): ${text:0:400}; $(fixture_tail)"
      (( $(fixture_lines 'req n=[0-9]+ .* codex=yes') >= 2 )) || fail "$clause" "the fixture saw fewer than two requests from the managed codex app-server (codex=yes: user agent openclaw/<codex version> and the fixture bearer): $(fixture_tail)"
      if grep -rqs 'steer fallback (not BUSY-MID)' "$tmp/daemon.log" 2>/dev/null || "${compose[@]}" logs --no-color openclaw-gateway 2>&1 | grep -q 'steer fallback (not BUSY-MID)'; then
        fail "$clause" "the steer fallback line is present"
      fi
      close_lane "$id"
      pass "$clause" "lane $id on the real codex harness: native tool active, '$cx_marker' acknowledged written, SAW-INJECT in the same run; fixture: $(fixture_tail)"
      ;;
    lane-voice)
      # recipient_marker_seen's root check is silently vacuous (contains("") is always
      # true) if b_root is ever empty — e.g. a fixture written by an older `up` that predates this
      # variable. Fail loudly instead of falling back to a marker-only, unscoped check.
      [[ -n "$b_root" ]] || fail "$clause" "b_root unset — fixture predates the recipient-scoped check"
      id=$(spawn "$(agent_open lane)")
      id2=$(spawn "$(agent_open b)")
      local marker
      marker="LANE_VOICE_${RANDOM}_$$"
      local mock_mark
      mock_mark=$(mock_log_mark)
      # The lane's own model calls the `sessionbus` tool (the mock turns `SBSEND:<target> <text>` into a tool call):
      # the plugin hands the action to this lane's worker over its bridge connection (bus.action), and the worker
      # sends from the lane's own identity. The send target is the daemon-qualified id.
      out=$(run "$id" "SBSEND:$id2 $marker")
      jq -e '.state=="done" and .result.outcome=="completed"' <<<"$out" >/dev/null || { lane_voice_evidence "$id" "$id2"; fail "$clause" "the lane turn did not complete: ${out:0:400}"; }
      text=$(jq -r '.result.result' <<<"$out")
      [[ "$text" == TOOL-DONE:* && "$text" == *"$id2"* && "$text" != *'"disposition":"rejected"'* ]] || { lane_voice_evidence "$id" "$id2"; fail "$clause" "the lane's tool result shows no delivery to $id2: ${text:0:500}"; }
      local deadline=$((SECONDS+30))
      # time-scoping (mock_mark) alone still accepts the
      # marker in *any* agent's request. recipient_marker_seen also requires b's own workspace root
      # in the same lastUser text, so a request that reached the wrong lane cannot satisfy this.
      until recipient_marker_seen "$mock_mark" "$marker" "$b_root"; do
        (( SECONDS < deadline )) || fail "$clause" "b's ($id2) model request never contained $marker"
        sleep 1
      done
      close_lane "$id"; close_lane "$id2"
      pass "$clause" "lane $id sent $marker through the bridge (bus.action); $id2 received it"
      ;;
    bridge-loss-retain)
      id=$(spawn "$(agent_open lane)" 0)
      first=$(run "$id" 'hello ECHO bridge-loss-retain')
      jq -e '.state=="done" and .result.outcome=="completed" and (.result.result | contains("ECHO"))' <<<"$first" >/dev/null || fail "$clause" "initial run incomplete: $first"
      ref=$(jq -er '.run_id' <<<"$first")
      restart_bridge_loss
      sleep 35
      for method in turn.status turn.wait; do
        out=$(rpc "$method" "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref}')")
        jq -e --argjson original "$first" '.state==$original.state and .run_id==$original.run_id and .result==$original.result' <<<"$out" >/dev/null || fail "$clause" "$method changed retained result: $out"
      done
      local loss_mark refused_ref loss_marker="BRIDGE_LOSS_REFUSED_${RANDOM}_$$"
      loss_mark=$(mock_log_mark)
      out=$(run "$id" "$loss_marker")
      jq -e '.state=="unavailable" and .reason=="lane bridge lost"' <<<"$out" >/dev/null || fail "$clause" "bridge was not lost: new Run was not refused: $out"
      refused_ref=$(jq -er '.run_id' <<<"$out")
      mock_log_since "$loss_mark" | jq -se --arg marker "$loss_marker" 'all(.[]; (tostring | contains($marker)) | not)' >/dev/null || fail "$clause" "refused input reached mock"
      for ref in "$ref" "$refused_ref"; do
        rpc turn.ack "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref}')" >/dev/null
      done
      close_lane "$id"
      out=$(sb turn.status "$(json --arg id "$id" '{session_id:$id}')" 2>&1) && fail "$clause" "status succeeded after close: $out"
      [[ "$out" == *unknown_session* ]] || fail "$clause" "close did not yield unknown_session: $out"
      pass "$clause" "unchanged status/wait after 35 s; new input refused without model request; close yields unknown_session"
      ;;
    bridge-loss-mid-run)
      id=$(spawn "$(agent_open lane)" 0)
      local slow_mark slow_deadline terminal
      slow_mark=$(mock_log_mark)
      start=$(rpc turn.start "$(json --arg id "$id" '{session_id:$id,input:"SLOW bridge-loss-mid-run"}')")
      ref=$(jq -er '.run_id' <<<"$start")
      slow_deadline=$((SECONDS+60))
      until mock_log_since "$slow_mark" | jq -se 'any(.[]; (.lastUser // "") | contains("SLOW bridge-loss-mid-run"))' >/dev/null; do
        (( SECONDS < slow_deadline )) || fail "$clause" "SLOW run never reached model"
        sleep 1
      done
      out=$(rpc turn.status "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref}')")
      jq -e '.state=="running"' <<<"$out" >/dev/null || fail "$clause" "run was not active before restart: $out"
      restart_bridge_loss
      slow_deadline=$((SECONDS+120))
      while :; do
        terminal=$(rpc turn.wait "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref,timeout_ms:10000}')")
        [[ "$(jq -r '.state' <<<"$terminal")" == running ]] || break
        (( SECONDS < slow_deadline )) || fail "$clause" "run never reached terminal: $terminal"
      done
      jq -e '(.state=="unavailable" and (.reason | type=="string" and length>0)) or (.state=="done" and (.result.outcome=="completed" or .result.outcome=="failed" or .result.outcome=="interrupted"))' <<<"$terminal" >/dev/null || fail "$clause" "no collectable native terminal: $terminal"
      sleep 35
      out=$(rpc turn.status "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref}')")
      jq -e --argjson original "$terminal" '.state==$original.state and .run_id==$original.run_id and .result==$original.result and .reason==$original.reason' <<<"$out" >/dev/null || fail "$clause" "terminal not retained after 35 s: $out"
      local loss_mark refused_ref loss_marker="BRIDGE_LOSS_REFUSED_${RANDOM}_$$"
      loss_mark=$(mock_log_mark)
      second=$(run "$id" "$loss_marker")
      jq -e '.state=="unavailable" and (.reason=="lane bridge lost" or .reason=="lane native session exited")' <<<"$second" >/dev/null || fail "$clause" "bridge was not lost: new Run was not refused: $second"
      refused_ref=$(jq -er '.run_id' <<<"$second")
      mock_log_since "$loss_mark" | jq -se --arg marker "$loss_marker" 'all(.[]; (tostring | contains($marker)) | not)' >/dev/null || fail "$clause" "refused input reached mock"
      for ref in "$ref" "$refused_ref"; do
        rpc turn.ack "$(json --arg id "$id" --arg ref "$ref" '{session_id:$id,run_id:$ref}')" >/dev/null
      done
      close_lane "$id"
      pass "$clause" "collectable after 35 s: state=$(jq -r '.state' <<<"$out") outcome=$(jq -r '.result.outcome // .reason' <<<"$out") run=$(jq -r '.run_id' <<<"$out"); new input refused without model request: $(jq -r '.reason' <<<"$second")"
      ;;
    admin-guard)
      # Live proof of the runtime admin guard. A worker whose Gateway
      # published credentialDir is /home/node/config, whose openclaw.json contains the Gateway's
      # shared admin token. ChildEnv selects this colocated config for every openclaw child;
      # /home/node/.openclaw has no openclaw.json in this fixture. That worker must refuse Open with
      # the guard's text; the normal, non-admin /lane-cli device must be unaffected; and the probe
      # itself (a read-only exec.approvals.get call) must leave no pending pairing request.
      before_devices=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs devices list --json 2>&1) || fail "$clause" "admin devices list before probe: ${before_devices:0:350}"
      before_pending=$(jq -r '(.pending // []) | length' <<<"$before_devices") || fail "$clause" "cannot parse pending count before probe"

      admin_tmp=$(mktemp -d /tmp/ocl-admin.XXXXXX)
      admin_socket="$admin_tmp/p.sock"
      mkdir -p "$admin_tmp/xdg-config" "$admin_tmp/b"
      chmod 700 "$admin_tmp/xdg-config" "$admin_tmp/b"
      # A stand-in plugin bridge that publishes the colocated Gateway config (with the shared admin token) as the
      # lane credentialDir; it only answers settings.get — the worker must refuse before it ever registers.
      admin_bridge="$admin_tmp/b/bridge.sock"
      node - "$admin_bridge" "$lane_root" "$b_root" "$decoy_real_root" <<'NODE' &
const net = require('node:net');
const [socketPath, lane, b, decoy] = process.argv.slice(2);
const settings = { agents: [
  { id: 'lane', workspace: lane, description: 'Primary lane agent' },
  { id: 'b', workspace: b, description: 'Second lane agent' },
  { id: 'decoy', workspace: decoy, description: 'Containment probe' },
], credentialDir: '/home/node/config' };
net.createServer((socket) => {
  let buffer = '';
  socket.setEncoding('utf8');
  socket.on('data', (chunk) => {
    buffer += chunk;
    for (let end; (end = buffer.indexOf('\n')) >= 0;) {
      const frame = JSON.parse(buffer.slice(0, end));
      buffer = buffer.slice(end + 1);
      const reply = frame.method === 'settings.get' ? { result: settings } : { error: { code: 'unknown_method', message: frame.method } };
      socket.write(JSON.stringify({ v: 1, id: frame.id, ...reply }) + '\n');
    }
  });
  socket.on('error', () => {});
}).listen(socketPath);
NODE
      admin_bridge_pid=$!
      PATH="$bin:$PATH" OPENCLAW_STATE_DIR="$admin_tmp" SESSIONBUS_OPENCLAW_BRIDGE="$admin_bridge" XDG_CONFIG_HOME="$admin_tmp/xdg-config" XDG_STATE_HOME="$admin_tmp/xdg" \
        sessionbus --socket "$admin_socket" --table "$admin_tmp/table" --host lanesmokeadmin >"$admin_tmp/daemon.log" 2>&1 &
      admin_daemon_pid=$!
      local deadline=$((SECONDS+15))
      until [[ -S "$admin_socket" && -S "$admin_bridge" ]]; do
        kill -0 "$admin_daemon_pid" 2>/dev/null || fail "$clause" "admin-state private daemon exited: $(tail -c 300 "$admin_tmp/daemon.log")"
        (( SECONDS < deadline )) || fail "$clause" "admin-state private daemon or stand-in bridge socket timed out"
        sleep .1
      done

      # Pitfall, again: a bare `admin_out=$(cmd)` propagates cmd's exit status
      # under set -e, and sessionbus-call exits nonzero for this expected spawn_failed — `|| true`
      # keeps the captured output while not letting that exit abort the whole script here.
      admin_out=$(SESSIONBUS_SOCKET="$admin_socket" sessionbus-call -timeout 35s -g lanesmokeadmin -socket "$admin_socket" lane.spawn \
        "$(json '{product:"openclaw-peer",name:"admin-guard",open:{arguments:["--agent","lane"]}}')" 2>&1) || true
      kill "$admin_daemon_pid" "$admin_bridge_pid" 2>/dev/null || true
      rm -rf "$admin_tmp"
      admin_daemon_pid=""
      admin_bridge_pid=""
      admin_tmp=""
      [[ "$admin_out" != *'"session_id"'* ]] || fail "$clause" "admin-state credential was accepted for a spawn: ${admin_out:0:350}"
      [[ "$admin_out" == *"lane credential has operator.admin"* ]] || fail "$clause" "admin-guard refusal wrong: ${admin_out:0:400}"

      id=$(spawn "$(agent_open lane)")
      close_lane "$id"

      after_devices=$("${compose[@]}" exec -T openclaw-gateway node openclaw.mjs devices list --json 2>&1) || fail "$clause" "admin devices list after probe: ${after_devices:0:350}"
      after_pending=$(jq -r '(.pending // []) | length' <<<"$after_devices") || fail "$clause" "cannot parse pending count after probe"
      [[ "$after_pending" == "$before_pending" ]] || fail "$clause" "the admin probe left a pending request (before=$before_pending after=$after_pending)"
      pass "$clause" "admin-state Open refused with the guard text; normal device opened $id; no pending request left"
      ;;
    *) fail setup "unknown clause $clause" ;;
  esac
}

case "${1:-all}" in
  up)
    # Check before arming the trap, not after — otherwise this guard's own failure
    # (fixture already up) runs *inside* the trap it just armed, and `down` tears down the live
    # fixture it was warning the operator about instead of leaving it alone.
    [[ ! -e "$fixture" ]] || fail setup "fixture already up; run down first"
    trap 'status=$?; if (( status != 0 )); then trap - EXIT; down; fi' EXIT
    up
    trap - EXIT
    ;;
  down) down ;;
  all)
    [[ ! -e "$fixture" ]] || fail setup "fixture already up; run down first"
    trap cleanup_all EXIT
    up
    load_fixture
    for clause in "${clauses[@]}"; do check_clause "$clause"; done
    ;;
  *)
    clause="$1"
    [[ " ${clauses[*]} " == *" $clause "* ]] || fail setup "unknown clause $clause"
    load_fixture
    if [[ "$clause" == admin-guard ]]; then trap cleanup_admin_guard EXIT; fi
    check_clause "$clause"
    ;;
esac
