#!/usr/bin/env bash
set -euo pipefail

runner="$(dirname "$0")/run.sh"
# Load the runner's functions without its CLI dispatch. This keeps these focused clause tests
# local: they never invoke Docker, OpenClaw, or a Sessionbus daemon.
source <(sed '/^case "${1:-all}" in/,$d' "$runner")

test_cwd_permissions() {
  local opens
  opens=$(mktemp)
  trap 'rm -f "$opens"' RETURN
  lane_root=/tmp/oc-lane-test/ws/lane
  b_root=/tmp/oc-lane-test/ws/b
  spawn() {
    printf '%s\n' "$1" >>"$opens"
    if jq -e '.arguments==["--agent","b"]' <<<"$1" >/dev/null; then echo agent:b:sb-test; else echo agent:lane:sb-test; fi
  }
  answer() {
    if [[ "$1" == agent:b:* ]]; then echo "$b_root/sub"; else echo "$lane_root/sub"; fi
  }
  close_lane() { :; }
  rpc() { printf '{"session_id":"agent:lane:sb-test"}'; }
  pass() { :; }
  check_clause cwd-honoured
  check_clause cwd-isolated
  jq -se 'map(select(.arguments==["--agent","lane"])) | length==2 and all(.[]; .permission_mode=="bypassPermissions")' "$opens" >/dev/null || {
    echo 'cwd clauses did not grant exec to both lane opens' >&2
    return 1
  }
}

test_spawn_names_are_unique_per_call() {
  # close_lane never forgets a lane's name, so a fixed literal name
  # collides with name_taken on the very next spawn() call — whether that's the next clause or a
  # second lane inside the same clause (cwd-isolated).
  local payloads
  payloads=$(mktemp)
  trap 'rm -f "$payloads"' RETURN
  rpc() { printf '%s\n' "$2" >>"$payloads"; printf '{"session_id":"agent:lane:sb-test"}'; }
  clause=spawn-open
  spawn "$(agent_open lane)" >/dev/null
  clause=run-answer
  spawn "$(agent_open lane)" >/dev/null
  spawn "$(agent_open lane)" >/dev/null
  local names
  mapfile -t names < <(jq -r '.name' "$payloads")
  [[ "${#names[@]}" -eq 3 ]] || { echo "expected 3 recorded spawn names, got ${#names[@]}: ${names[*]}" >&2; return 1; }
  [[ "${names[0]}" == lane-smoke-spawn-open-* ]] || { echo "name did not carry its clause: ${names[0]}" >&2; return 1; }
  [[ "${names[1]}" == lane-smoke-run-answer-* && "${names[2]}" == lane-smoke-run-answer-* ]] || {
    echo "name did not carry its clause: ${names[1]} / ${names[2]}" >&2
    return 1
  }
  if [[ "${names[0]}" == "${names[1]}" || "${names[1]}" == "${names[2]}" || "${names[0]}" == "${names[2]}" ]]; then
    echo "spawn() reused a name across calls: ${names[*]}" >&2
    return 1
  fi
}

test_provision_credential_reports_helper_failure() {
  # `helper_out=$(… 2>&1)` as a bare statement propagates the helper's exit status under
  # `set -e`, aborting run.sh right there — `fail lane-credential "pair-device helper: …"` never
  # ran, so a pending-pairing (or any other) helper failure produced no evidence at all, only a
  # bare exit 1 while the fixture's failure trap tore everything down. `provision_credential` is
  # called as a plain top-level statement from `up()`, not from inside an `if`/`$(…)` test position
  # — invoking it the same way here (backgrounded, so its own errexit-driven abort can't also take
  # down this test) is what actually exercises the bug; wrapping the call directly in `out=$(…)` or
  # `if …; then` exempts the whole call from errexit (bash's "context where -e is ignored" rule)
  # and made an earlier version of this test pass against the unfixed code.
  local stub outfile rc
  stub=$(mktemp)
  outfile=$(mktemp)
  cat >"$stub" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
shift # exec
shift # -T
argv="$*"
if [[ "$argv" == *pair-device.mjs* ]]; then
  echo "pair-device: gateway connect failed: pairing required" >&2
  exit 1
fi
echo '{"paired":[],"pending":[]}'
STUB
  chmod +x "$stub"
  trap 'rm -f "$stub" "$outfile"' RETURN
  compose=(bash "$stub")
  gateway_token=$(openssl rand -hex 32)
  ( provision_credential ) >"$outfile" 2>&1 &
  rc=0
  wait "$!" || rc=$?
  (( rc != 0 )) || { echo "expected provision_credential to fail when the helper fails" >&2; return 1; }
  grep -qF 'lane-credential: FAIL — pair-device helper:' "$outfile" && grep -qF 'pairing required' "$outfile" || {
    echo "helper failure was not reported as evidence: $(cat "$outfile")" >&2
    return 1
  }
}

test_model_reasoning_compat() {
  node - "$LANE_SMOKE_ROOT/openclaw.json5" <<'NODE'
const fs = require('node:fs');
const vm = require('node:vm');
const config = vm.runInNewContext('(' + fs.readFileSync(process.argv[2], 'utf8') + ')');
const models = config.models.providers.mock.models;
if (models.length !== 2 || !models.every((model) => model.compat?.supportsReasoningEffort === true)) {
  console.error('mock models do not explicitly forward reasoning_effort');
  process.exit(1);
}
NODE
}

test_agents_ownership_not_combined_with_default() {
  # 8.2 rejects agents.ownership:"explicit" combined with a legacy default:true marker
  # (src/config/zod-schema.agents.ts:72-77; the error says "agents.ownership=explicit
  # cannot be combined with a legacy default=true marker"). "lane" needs default:true (it is the
  # agent a callerless spawn falls back to), so ownership must stay unset.
  node - "$LANE_SMOKE_ROOT/openclaw.json5" <<'NODE'
const fs = require('node:fs');
const vm = require('node:vm');
const config = vm.runInNewContext('(' + fs.readFileSync(process.argv[2], 'utf8') + ')');
if (config.agents.ownership !== undefined) {
  console.error(`agents.ownership is set (${JSON.stringify(config.agents.ownership)}); 8.2 rejects it alongside a legacy default:true marker`);
  process.exit(1);
}
if (config.agents.entries.lane?.default !== true) {
  console.error('agents.entries.lane.default is not true; the ownership-vs-default conflict this pins would no longer apply');
  process.exit(1);
}
NODE
}

test_permission_deadline() {
  local test_state ticks
  test_state=$(mktemp -d)
  ticks="$test_state/ticks"
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  spawn() { if [[ "$1" == *bypassPermissions* ]]; then echo bypass-lane; else echo default-lane; fi; }
  answer() {
    if [[ "$1" == bypass-lane ]]; then
      echo 'TOOL-DONE: uid=1'
    else
      printf 'permission default exec -> deny\n' >>"$state/permission-audit.log"
      echo 'TOOL-DONE: denied'
    fi
  }
  mock_log() { printf '%s\n' '{"toolResult":true,"messages":[{"role":"tool","content":"Exec denied by policy"}]}'; }
  close_lane() { :; }
  pass() { :; }
  local deadline_case=default
  clock_seconds() {
    local n=0
    [[ -f "$ticks" ]] && n=$(cat "$ticks")
    n=$((n+1))
    printf '%s' "$n" >"$ticks"
    if [[ "$deadline_case" == default ]]; then
      if (( n == 1 )); then echo 0; else echo 31; fi
    else
      case "$n" in 1) echo 0;; 2|3) echo 1;; *) echo 32;; esac
    fi
  }
  local outcome
  if outcome=$(check_clause permission-answered 2>&1); then
    echo 'permission-answered accepted a 31-second default EXEC:id run' >&2
    return 1
  fi
  [[ "$outcome" == *'over the 30 s bound'* ]] || { echo "default run failed for the wrong reason: $outcome" >&2; return 1; }
  rm -f "$ticks"
  deadline_case=bypass
  if outcome=$(check_clause permission-answered 2>&1); then
    echo 'permission-answered accepted a 31-second bypass EXEC:id run' >&2
    return 1
  fi
  [[ "$outcome" == *'over the 30 s bound'* ]] || { echo "bypass run failed for the wrong reason: $outcome" >&2; return 1; }
}

test_permission_denial_source() {
  local test_state mock_fixture write_fresh_audit outcome
  test_state=$(mktemp -d)
  mock_fixture="$test_state/mock.jsonl"
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  printf 'permission default exec -> deny\n' >"$state/permission-audit.log" # stale line from an earlier run
  spawn() { if [[ "$1" == *bypassPermissions* ]]; then echo bypass-lane; else echo default-lane; fi; }
  answer() {
    if [[ "$1" == bypass-lane ]]; then
      echo 'TOOL-DONE: uid=1'
    else
      if [[ "$write_fresh_audit" == true ]]; then
        printf 'permission default exec -> deny\n' >>"$state/permission-audit.log"
      fi
      echo 'TOOL-DONE: denied'
    fi
  }
  close_lane() { :; }
  pass() { :; }
  clock_seconds() { echo 1; }
  mock_log() { cat "$mock_fixture"; }
  write_fresh_audit=true
  printf '%s\n' '{"toolResult":false,"messages":[]}' >"$mock_fixture"
  if (check_clause permission-answered >/dev/null 2>&1); then
    echo 'permission-answered trusted denial words without a denied tool result' >&2
    return 1
  fi
  printf '%s\n' '{"toolResult":true,"messages":[{"role":"tool","content":"Exec denied by policy"}]}' >"$mock_fixture"
  write_fresh_audit=false
  if outcome=$(check_clause permission-answered 2>&1); then
    echo 'permission-answered accepted a stale denial line without a new audit entry' >&2
    return 1
  fi
  [[ "$outcome" == *'audit log has no default denial'* ]] || { echo "stale audit check failed for the wrong reason: $outcome" >&2; return 1; }
  write_fresh_audit=true
  check_clause permission-answered >/dev/null
}

test_lane_voice_sends_through_the_lane_tool() {
  # The lane-voice clause drives the lane's own model into the `sessionbus` tool: the input is
  # `SBSEND:<qualified id of b> <marker>` (the mock turns it into the tool call). The target must be the
  # daemon-qualified id; the clause then needs b's own request carrying the marker. Stubs `run` (records the
  # input, writes the matching b request into the mock log, answers like the mock after the tool ran).
  local captured test_state b_root
  captured=$(mktemp)
  test_state=$(mktemp -d)
  trap 'rm -f "$captured"; rm -rf "$test_state"' RETURN
  state="$test_state"
  b_root="/test/ws/b"
  mkdir -p "$state/mock-log"
  spawn() {
    if jq -e '.arguments==["--agent","b"]' <<<"$1" >/dev/null; then
      echo "agent:b:sb-test@lanesmoke"
    else
      echo "agent:lane:sb-test@lanesmoke"
    fi
  }
  run() {
    printf '%s' "$2" >"$captured"
    local target marker
    target=$(sed -E 's/^SBSEND:([^ ]+) .*$/\1/' "$captured")
    marker=$(sed -E 's/^SBSEND:[^ ]+ (.*)$/\1/' "$captured")
    jq -nc --arg marker "$marker" --arg root "$b_root" '{lastUser:("[Working directory: "+$root+"] "+$marker)}' >>"$state/mock-log/requests.jsonl"
    jq -nc --arg id "$target" '{state:"done",result:{outcome:"completed",result:("TOOL-DONE: "+({deliveries:[{disposition:"written",session_id:$id}]}|tojson))}}'
  }
  close_lane() { :; }
  pass() { :; }
  check_clause lane-voice >/dev/null
  [[ "$(sed -E 's/^SBSEND:([^ ]+) .*$/\1/' "$captured")" == "agent:b:sb-test@lanesmoke" ]] || {
    echo "lane-voice sent to $(cat "$captured"), want the qualified id agent:b:sb-test@lanesmoke" >&2
    return 1
  }
}

test_busy_mid_codex_requires_written_receipt_and_saw_inject() {
  # X1: the codex lane clause needs the native tool to be active (a new fixture tool-call line), a `written` receipt
  # and SAW-INJECT in the run's own result; a not_running receipt (the steer fallback) or a result without
  # SAW-INJECT is a FAIL.
  local test_state disposition=written result_text=SAW-INJECT
  test_state=$(mktemp -d)
  trap 'rm -rf "$test_state"' RETURN
  tmp="$test_state"
  : >"$tmp/daemon.log"
  spawn() { echo "agent:codexlane:sb-test@lanesmoke"; }
  sleep() { :; }
  write_codex_home() { :; }
  egress_blocked() { return 0; }
  # rpc runs in a command substitution, so the fixture's tool-call count lives in a file.
  fixture_lines() { case "$1" in tool-call*) cat "$test_state/tool_calls" 2>/dev/null || printf 0 ;; *) printf '2' ;; esac; }
  fixture_tail() { printf 'fixture-tail'; }
  close_lane() { :; }
  pass() { :; }
  compose=(true)
  rpc() {
    case "$1" in
      turn.start) printf 1 >"$test_state/tool_calls"; printf '{"run_id":"r1"}' ;;
      message.send) jq -nc --arg d "$disposition" '{deliveries:[{session_id:"agent:codexlane:sb-test@lanesmoke",disposition:$d}]}' ;;
      turn.wait) jq -nc --arg t "$result_text" '{state:"done",result:{outcome:"completed",result:$t}}' ;;
      *) printf '{}' ;;
    esac
  }
  check_clause busy-mid-codex >/dev/null
  # Hermeticity: a Gateway that can reach the internet must stop the clause before any codex turn.
  egress_blocked() { return 1; }
  if (check_clause busy-mid-codex >/dev/null 2>&1); then echo "busy-mid-codex ran with a reachable internet" >&2; return 1; fi
  egress_blocked() { return 0; }
  rm -f "$test_state/tool_calls"; disposition=not_running
  if (check_clause busy-mid-codex >/dev/null 2>&1); then echo "busy-mid-codex accepted a not_running receipt" >&2; return 1; fi
  rm -f "$test_state/tool_calls"; disposition=written; result_text='DONE-NO-INJECT'
  if (check_clause busy-mid-codex >/dev/null 2>&1); then echo "busy-mid-codex accepted a run without SAW-INJECT" >&2; return 1; fi
}

test_up_failure_leaves_a_cleanable_fixture() {
  # up() used to write the fixture only once, at its very end — a failure partway
  # through left no fixture at all, so down() (invoked by the up-failure trap) had nothing to kill:
  # the host tunnel process and $bridge_dir leaked silently. Stubs only `docker` (every
  # "${compose[@]}" call) and `npm pack`; real mkdir/openssl/node/the actual bridge-tunnel.mjs run
  # for real in an isolated $state, exactly like the shipped script, up to a simulated
  # `docker compose ... up -d` failure.
  local test_root bridge_was_live
  test_root=$(mktemp -d)
  bridge_was_live="$test_root/bridge-was-live"
  trap 'down >/dev/null 2>&1 || true; rm -rf "$test_root"' RETURN
  LANE_SMOKE_ROOT="$PWD/smoke/docker/lane"
  state="$test_root/state"
  bin="$test_root/bin"
  fixture="$state/fixture.env"
  COMPOSE_PROJECT_NAME="oc_lane_smoke_test_$$"

  docker() {
    if [[ "$1" == "compose" && "$4" == "up" ]]; then
      local pid
      pid=$(sed -n 's/^bridge_tunnel_pid=//p' "$fixture" | tr -d "'")
      kill -0 "$pid" 2>/dev/null && : >"$bridge_was_live"
      echo "simulated docker compose up -d failure" >&2
      return 1
    fi
    return 0
  }
  npm() {
    if [[ "$1" == "pack" ]]; then echo '[{"filename":"fake.tgz"}]'; return 0; fi
    return 0
  }
  # up() checks every host tool (go, sessionbus, …) first; on a host missing one,
  # it fails before the first fixture write and this case fails for the wrong reason.
  # The tools it checks for are not used before the simulated compose failure.
  need() { :; }

  local status=0
  ( up ) >/dev/null 2>&1 &
  wait "$!" || status=$?
  [[ "$status" -ne 0 ]] || { echo "expected the simulated docker compose up -d to fail up()" >&2; return 1; }
  [[ -f "$fixture" ]] || { echo "up() failure left no fixture at all — down() has nothing to clean up" >&2; return 1; }
  grep -q '^bridge_tunnel_pid=' "$fixture" || { echo "fixture is missing bridge_tunnel_pid" >&2; return 1; }
  grep -q '^bridge_dir=' "$fixture" || { echo "fixture is missing bridge_dir" >&2; return 1; }

  [[ -f "$bridge_was_live" ]] || { echo "host bridge tunnel was not running when compose up failed" >&2; return 1; }
  local bridge_tunnel_pid
  bridge_tunnel_pid=$(sed -n 's/^bridge_tunnel_pid=//p' "$fixture" | tr -d "'")

  down >/dev/null 2>&1
  # down() sends SIGTERM and returns; the tunnel needs a moment to exit, and once the up() subshell is
  # gone it is reparented, so it can linger as an unreaped zombie that `kill -0` still reports. Stopped =
  # gone or zombie within 5 s; a tunnel down() never signalled stays running and fails this check.
  local tries
  for tries in {1..50}; do
    kill -0 "$bridge_tunnel_pid" 2>/dev/null || return 0
    [[ "$(ps -o stat= -p "$bridge_tunnel_pid" 2>/dev/null)" == Z* ]] && return 0
    sleep 0.1
  done
  echo "down() did not stop the leaked host bridge tunnel" >&2
  return 1
}

test_early_up_failure_removes_bridge_dir() {
  local test_root before after status leaked_dir
  test_root=$(mktemp -d)
  leaked_dir=""
  trap '[[ -z "$leaked_dir" ]] || rm -rf "$leaked_dir"; rm -rf "$test_root"' RETURN
  LANE_SMOKE_ROOT="$PWD/smoke/docker/lane"
  state="$test_root/state"; bin="$test_root/bin"; fixture="$state/fixture.env"
  need() { :; }
  chmod() {
    if [[ "$1" == 700 && "$2" == /tmp/ocl-br.* ]]; then return 1; fi
    command chmod "$@"
  }
  before=$(find /tmp -maxdepth 1 -type d -name 'ocl-br.*' | wc -l)
  status=0
  ( up ) >/dev/null 2>&1 &
  wait "$!" || status=$?
  (( status != 0 )) || { echo "expected early bridge-dir setup failure" >&2; return 1; }
  grep -q '^bridge_dir=' "$fixture" || { echo "early failure did not record bridge_dir" >&2; return 1; }
  leaked_dir=$(sed -n 's/^bridge_dir=//p' "$fixture" | tr -d "'")
  down >/dev/null 2>&1
  after=$(find /tmp -maxdepth 1 -type d -name 'ocl-br.*' | wc -l)
  [[ ! -e "$leaked_dir" && "$after" -eq "$before" ]] || {
    echo "early up failure leaked $leaked_dir (before=$before after=$after)" >&2
    return 1
  }
}

test_gateway_config_not_world_writable() {
  # The Gateway config carries the live admin token; run.sh used to make it
  # world-writable (chmod a+rw) so the uid-1000 container could write it. A live behavioral check
  # needs Docker (out of this test harness's scope, like every other up()-only concern); this pins
  # the source-level fix — no more a+rw, and the chown-then-0600 replacement is present — the same
  # "required greps" style already used elsewhere in this smoke for Docker-only clauses.
  grep -q 'chmod a+rw "\$state/openclaw.json"' "$runner" && {
    echo "run.sh still makes the Gateway config world-writable (chmod a+rw)" >&2
    return 1
  }
  # The config moved from a single-file bind mount to a directory mount
  # (./.state/config:/home/node/config) — plugins install --force renames a temp file onto the
  # config, which needs the container uid on the whole directory, not just the one file.
  grep -q 'chown -R node:node /home/node/config' "$runner" || {
    echo "run.sh no longer chowns the Gateway config directory to the container uid" >&2
    return 1
  }
  grep -q 'chmod 600 "\$state/config/openclaw.json"' "$runner" || {
    echo "run.sh does not restore 0600 on the Gateway config" >&2
    return 1
  }
  # The writer itself must create the file 0600 from the start — it was world-readable
  # (0644) for the whole `compose build` window before the later chown+chmod steps ran.
  grep -q 'writeFileSync(dest, config, {mode:0o600})' "$runner" || {
    echo "run.sh's config writer does not create openclaw.json 0600 from the start" >&2
    return 1
  }
}

test_gateway_config_is_directory_mount() {
  # `plugins install --force` writes the config via a same-dir temp-file-then-rename;
  # renaming onto a single-file bind mount fails EBUSY ("EBUSY: resource busy or
  # locked, rename '/home/node/.openclaw/openclaw.json.<tmp>' -> '/home/node/.openclaw/
  # openclaw.json'"). The fix bind-mounts a directory instead and points OPENCLAW_CONFIG_PATH at a
  # file inside it — pinned here since a live rename-onto-a-bind-mount failure needs Docker to
  # reproduce.
  local compose_file
  compose_file="$(dirname "$runner")/compose.yaml"
  grep -q '\./\.state/openclaw\.json:/home/node/\.openclaw/openclaw\.json' "$compose_file" && {
    echo "compose.yaml still bind-mounts a single config file (EBUSY on plugins install --force)" >&2
    return 1
  }
  grep -q '\./\.state/config:/home/node/config' "$compose_file" || {
    echo "compose.yaml does not bind-mount the config directory" >&2
    return 1
  }
  grep -q 'OPENCLAW_CONFIG_PATH: /home/node/config/openclaw\.json' "$compose_file" || {
    echo "compose.yaml does not set OPENCLAW_CONFIG_PATH for the gateway service" >&2
    return 1
  }
  grep -q '"\$state/config/openclaw\.json"' "$runner" || {
    echo "run.sh does not write the config under \$state/config" >&2
    return 1
  }
}

test_lane_cli_sites_carry_config_path() {
  # compose.yaml's OPENCLAW_CONFIG_PATH is now set at the service level — every
  # `docker compose exec` into that service inherits it unless overridden per call. Without an
  # explicit -e OPENCLAW_CONFIG_PATH=/lane-cli/openclaw.json alongside every
  # -e OPENCLAW_STATE_DIR=/lane-cli, the "lane CLI" would silently read the Gateway's own config
  # (the one carrying the shared admin token) instead of the tokenless /lane-cli/openclaw.json — a
  # credential leak that would make the tokenless check pass falsely rather than fail loudly.
  local missing sites
  missing=$(grep -n 'OPENCLAW_STATE_DIR=/lane-cli' "$runner" | grep -v 'OPENCLAW_CONFIG_PATH=/lane-cli/openclaw\.json' || true)
  [[ -z "$missing" ]] || {
    echo "run.sh has a lane-cli site missing OPENCLAW_CONFIG_PATH:" >&2
    echo "$missing" >&2
    return 1
  }
  sites=$(grep -c 'OPENCLAW_STATE_DIR=/lane-cli' "$runner")
  [[ "$sites" -ge 3 ]] || {
    echo "expected at least 3 lane-cli bootstrap sites in run.sh, found $sites" >&2
    return 1
  }
}

test_mock_log_scoping() {
  # mock_log_since must exclude entries recorded before mock_log_mark, so a
  # marker left over from an unrelated earlier run in the same log file can't satisfy a later
  # clause's check on its own.
  local test_state
  test_state=$(mktemp -d)
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  mkdir -p "$state/mock-log"
  printf '%s\n' '{"lastUser":"STALE_MARKER"}' >"$state/mock-log/requests.jsonl"
  local mark
  mark=$(mock_log_mark)
  [[ "$mark" == "1" ]] || { echo "mock_log_mark = $mark, want 1 (one pre-existing line)" >&2; return 1; }
  [[ -z "$(mock_log_since "$mark")" ]] || { echo "mock_log_since returned a pre-existing line" >&2; return 1; }
  printf '%s\n' '{"lastUser":"FRESH_MARKER"}' >>"$state/mock-log/requests.jsonl"
  local since
  since=$(mock_log_since "$mark")
  [[ "$since" == '{"lastUser":"FRESH_MARKER"}' ]] || {
    echo "mock_log_since = $since, want only the fresh line" >&2
    return 1
  }
}

test_admin_guard_targets_container_default_state() {
  # The admin-guard clause's live proof depends entirely on its Gateway
  # command actually reaching the container's own admin state — if it ever grew a /lane-cli
  # override (copy-paste from the other lane-CLI sites), the clause would silently test the
  # non-admin device against itself and always pass, proving nothing. This pins that structural
  # property from run.sh's own source; the live refusal behavior itself needs docker (out of
  # scope for this file, per its own header).
  local admin_block
  admin_block=$(sed -n '/^    admin-guard)/,/^      ;;/p' "$runner")
  [[ -n "$admin_block" ]] || { echo "admin-guard clause not found in run.sh" >&2; return 1; }
  if grep -q 'OPENCLAW_STATE_DIR=/lane-cli' <<<"$admin_block"; then
    echo "admin-guard clause's Gateway command still overrides to /lane-cli — it must use the container's own admin default instead" >&2
    return 1
  fi
  grep -q 'credentialDir:.*home/node/config' <<<"$admin_block" || {
    echo "admin-guard clause does not use the colocated admin config directory" >&2
    return 1
  }
  grep -q 'lane credential has operator.admin' <<<"$admin_block" || {
    echo "admin-guard clause does not check for the guard's exact refusal text" >&2
    return 1
  }
  grep -q 'admin-guard' <<<"$(grep '^clauses=' "$runner")" || {
    echo "admin-guard is not listed in the clauses array, so a plain run.sh run would skip it" >&2
    return 1
  }
  grep -Eq '^[[:space:]]+"\$\(json .*2>&1\) \|\| true$' <<<"$admin_block" || {
    echo "admin-guard must tolerate the expected nonzero spawn refusal" >&2
    return 1
  }
  local down_block
  down_block=$(sed -n '/^down() {/,/^}/p' "$runner")
  grep -q 'admin_daemon_pid' <<<"$down_block" || { echo "down does not stop an early-failed admin daemon" >&2; return 1; }
  grep -q 'admin_tmp' <<<"$down_block" || { echo "down does not remove an early-failed admin tmp dir" >&2; return 1; }
}

test_bridge_handoff() {
  # Lane settings reach the worker over the bridge, not through a published file: no lanes.json, no shared state
  # dir mount, no voice directory; the worker's daemon gets the host bridge path and compose carries the tunnel.
  local compose_file config_file
  compose_file="$(dirname "$runner")/compose.yaml"
  config_file="$(dirname "$runner")/openclaw.json5"
  if grep -q 'lanes.json' "$runner"; then echo 'runner still mentions the published lanes file' >&2; return 1; fi
  if grep -Eq 'oc-state/sessionbus|LANE_VOICE_HOST_DIR|LANE_VOICE_BRIDGE|voice_dir|voice-bridge' "$runner" "$compose_file"; then echo 'runner or compose still carries the voice/published-file plumbing' >&2; return 1; fi
  grep -q 'SESSIONBUS_OPENCLAW_BRIDGE="\$host_bridge"' "$runner" || { echo 'worker daemon has no bridge path' >&2; return 1; }
  grep -q 'bridge-tunnel' "$compose_file" || { echo 'compose lacks the bridge tunnel sidecar' >&2; return 1; }
  grep -q 'SESSIONBUS_OPENCLAW_BRIDGE' "$compose_file" || { echo 'compose does not give the Gateway the bridge path' >&2; return 1; }
  grep -q 'credentialDir: "/lane-cli"' "$config_file" || { echo 'operator lanes config lacks credentialDir' >&2; return 1; }
  if grep -Eq 'laneVoice|voiceReadiness|allowConversationAccess' "$config_file"; then echo 'operator config still carries removed keys' >&2; return 1; fi
  local legacy_prefix=OPENCLAW
  legacy_prefix+="_LANE_"
  if grep -Eq "^[[:space:]]*(export )?${legacy_prefix}" "$runner"; then echo 'runner still exports legacy lane variables' >&2; return 1; fi
}

test_cwd_outside_refused_checks_pending_and_scopes() {
  # The decoy path is outside the workspace OpenClaw resolves for that agent, so the worker
  # refuses it locally. Keep the devices-list diff around that attempt. This stubs both `sb`
  # calls (the two refused spawns) and the admin `compose`
  # devices-list calls, and checks both a clean pass and each way the diff check can catch a
  # regression (a leftover pending request; changed scopes).
  clause=cwd-outside-refused
  b_root=/tmp/fake-b
  decoy_root=/tmp/fake-decoy
  sb() {
    if [[ "$2" == *'"name":"outside"'* ]]; then
      echo "outside the workspace of agent lane" >&2
    else
      echo "outside the workspace of agent decoy" >&2
    fi
    return 1
  }
  local stub devices_call_file
  stub=$(mktemp)
  devices_call_file=$(mktemp)
  trap 'rm -f "$stub" "$devices_call_file"' RETURN
  cat >"$stub" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
n=$(cat "$DEVICES_CALL_FILE" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$DEVICES_CALL_FILE"
if [[ "$n" -eq 1 ]]; then
  printf '{"pending":[],"paired":[{"tokens":[{"scopes":["operator.approvals","operator.read","operator.write"]}]}]}'
else
  printf '{"pending":%s,"paired":[{"tokens":[{"scopes":%s}]}]}' "$DEVICES_PENDING_AFTER" "$DEVICES_SCOPES_AFTER"
fi
STUB
  chmod +x "$stub"
  compose=(env "DEVICES_CALL_FILE=$devices_call_file" bash "$stub")

  export DEVICES_PENDING_AFTER='[]'
  export DEVICES_SCOPES_AFTER='["operator.approvals","operator.read","operator.write"]'
  local out
  out=$(check_clause cwd-outside-refused 2>&1) || { echo "expected clause to pass with clean state: $out" >&2; return 1; }
  [[ "$out" == *"PASS"* ]] || { echo "expected a PASS line: $out" >&2; return 1; }

  rm -f "$devices_call_file"
  export DEVICES_PENDING_AFTER='[{"deviceId":"leftover"}]'
  out=$(check_clause cwd-outside-refused 2>&1) && { echo "expected the clause to fail when a pending request appears" >&2; return 1; }
  [[ "$out" == *"pending request"* ]] || { echo "did not name the leftover pending request: $out" >&2; return 1; }

  rm -f "$devices_call_file"
  export DEVICES_PENDING_AFTER='[]'
  export DEVICES_SCOPES_AFTER='["operator.admin","operator.approvals","operator.read","operator.write"]'
  out=$(check_clause cwd-outside-refused 2>&1) && { echo "expected the clause to fail when scopes changed" >&2; return 1; }
  [[ "$out" == *"scopes changed"* ]] || { echo "did not name the scope change: $out" >&2; return 1; }
}

test_gateway_list_row_strips_daemon_group_suffix() {
  # lane.spawn returns the daemon-qualified id (agent:lane:sb-...@lanesmoke), but the
  # Gateway row is keyed by the bare key the wrapper minted (no @group suffix). Searching with the
  # qualified id straight through never matched a row — model-session-only's `[[ -n "$row" ]]` is
  # the first caller that actually needs a non-empty result, so this stayed invisible until then.
  # (The fake `gateway` below echoes back whatever key it was actually searched with, inside the
  # row itself — a plain side-channel variable would be set inside gateway_list_row's own `$(…)`
  # subshell and lost the moment that subshell exits.)
  clause=test
  gateway() {
    local key
    key=$(jq -r '.search' <<<"$2")
    json --argjson sessions "$(json --arg key "$key" '[{key:$key,model:"mock-b"}]')" '{sessions:$sessions}'
  }
  local row got_key
  row=$(gateway_list_row 'agent:lane:sb-1234:main@lanesmoke')
  got_key=$(jq -r '.key // empty' <<<"$row")
  [[ "$got_key" == 'agent:lane:sb-1234:main' ]] || {
    echo "gateway_list_row matched key '${got_key:-<empty>}', want the bare key without @group" >&2
    return 1
  }
}

test_lane_voice_recipient_scoped() {
  # mock_log_since alone still accepts the marker in
  # *any* agent's request after the mark — a delivery that looped back to the sending lane would
  # still satisfy an unscoped check. recipient_marker_seen also requires the recipient's own
  # workspace root in the same lastUser text (OpenClaw prefixes every ACP prompt with
  # "[Working directory: <cwd>]").
  local test_state marker lane_root b_root
  test_state=$(mktemp -d)
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  mkdir -p "$state/mock-log"
  marker="LANE_VOICE_TESTMARK"
  lane_root="/test/ws/lane"
  b_root="/test/ws/b"

  # A request carrying the marker but the *sender's* (lane) workspace root would have passed
  # the old, unscoped check as false proof that b received the message.
  jq -nc --arg root "$lane_root" --arg m "$marker" '{lastUser:("[Working directory: "+$root+"] "+$m)}' \
    >"$state/mock-log/requests.jsonl"
  recipient_marker_seen 0 "$marker" "$b_root" && {
    echo "recipient_marker_seen accepted the marker from the sender's own request" >&2
    return 1
  }

  # b's own request, prefixed with its own workspace root — must satisfy it.
  jq -nc --arg root "$b_root" --arg m "$marker" '{lastUser:("[Working directory: "+$root+"] "+$m)}' \
    >>"$state/mock-log/requests.jsonl"
  recipient_marker_seen 0 "$marker" "$b_root" || {
    echo "recipient_marker_seen did not accept the marker from the recipient's own request" >&2
    return 1
  }
}

test_lane_voice_delivery_must_target_recipient() {
  # test_lane_voice_recipient_scoped pins recipient_marker_seen (the marker half); this exercises the tool-result
  # half at clause level: a tool result that names only the sender (not the recipient) must fail the clause.
  local test_state b_root
  test_state=$(mktemp -d)
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  b_root="/test/ws/b"
  mkdir -p "$state/mock-log"
  spawn() {
    if jq -e '.arguments==["--agent","b"]' <<<"$1" >/dev/null; then
      echo "agent:b:sb-test@lanesmoke"
    else
      echo "agent:lane:sb-test@lanesmoke"
    fi
  }
  run() {
    local marker
    marker=$(sed -E 's/^SBSEND:[^ ]+ (.*)$/\1/' <<<"$2")
    jq -nc --arg marker "$marker" --arg root "$b_root" '{lastUser:("[Working directory: "+$root+"] "+$marker)}' >>"$state/mock-log/requests.jsonl"
    jq -nc '{state:"done",result:{outcome:"completed",result:"TOOL-DONE: {\"deliveries\":[{\"disposition\":\"written\",\"session_id\":\"agent:lane:sb-test@lanesmoke\"}]}"}}'
  }
  close_lane() { :; }
  pass() { :; }
  if (check_clause lane-voice >/dev/null 2>&1); then
    echo "lane-voice accepted a tool result that names the sender, not the recipient" >&2
    return 1
  fi
}

test_lane_voice_fails_when_the_lane_turn_does_not_complete() {
  # If the lane's run never completes (no tool call, interrupted, or an error), the clause must fail with the run
  # output as evidence rather than waiting on b.
  local test_state b_root output
  test_state=$(mktemp -d)
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  b_root="/test/ws/b"
  mkdir -p "$state/mock-log"
  spawn() { echo "agent:lane:sb-test@lanesmoke"; }
  run() { printf '{"state":"done","result":{"outcome":"interrupted","result":"plugin bridge gone"}}'; }
  close_lane() { :; }
  pass() { :; }
  output=$(check_clause lane-voice 2>&1) && { echo "expected the clause to fail" >&2; return 1; }
  [[ "$output" == *"the lane turn did not complete"* && "$output" == *"plugin bridge gone"* ]] || {
    echo "the failed run's output did not reach the fail() evidence: $output" >&2
    return 1
  }
}

test_busy_mid_requires_written_receipt_and_the_reaction_in_the_run() {
  # busy-mid: the message to the busy lane must be acknowledged `written` (not_running = the steer fallback), and the
  # run's own result must carry the steered text after its ticks. Stubs the daemon calls and the mock log.
  local test_state seen
  test_state=$(mktemp -d)
  seen="$test_state/marker"
  trap 'rm -rf "$test_state"' RETURN
  state="$test_state"
  tmp="$test_state"
  mkdir -p "$state/mock-log"
  : >"$tmp/daemon.log"
  spawn() { echo "agent:lane:sb-test@lanesmoke"; }
  sleep() { :; }
  mock_log_mark() { echo 0; }
  mock_log_since() { printf '%s\n' '{"lastUser":"SLOW busy-mid"}'; [[ ! -f "$seen" ]] || jq -nc --arg m "$(cat "$seen")" '{lastUser:$m}'; }
  close_lane() { :; }
  pass() { :; }
  compose_logs=""
  local disposition=written result_text='tick0 tick1 ECHO[model=mock-a]: __MARKER__'
  rpc() {
    case "$1" in
      turn.start) printf '{"run_id":"r1"}' ;;
      message.send)
        jq -r '.message' <<<"$2" >"$seen"
        jq -nc --arg d "$disposition" '{deliveries:[{session_id:"agent:lane:sb-test@lanesmoke",disposition:$d}]}'
        ;;
      turn.wait) jq -nc --arg t "${result_text//__MARKER__/$(cat "$seen")}" '{state:"done",result:{outcome:"completed",result:$t}}' ;;
      *) printf '{}' ;;
    esac
  }
  compose=(true) # `"${compose[@]}" logs ...` finds no fallback line
  check_clause busy-mid >/dev/null

  disposition=not_running
  if (check_clause busy-mid >/dev/null 2>&1); then echo "busy-mid accepted a not_running receipt (the steer fallback)" >&2; return 1; fi
  disposition=written
  result_text='tick0 tick1 and nothing else'
  if (check_clause busy-mid >/dev/null 2>&1); then echo "busy-mid accepted a run whose result lacks the steered message" >&2; return 1; fi
  result_text='ECHO[model=mock-a]: __MARKER__ alone'
  if (check_clause busy-mid >/dev/null 2>&1); then echo "busy-mid accepted a reaction without the run's own ticks" >&2; return 1; fi
}

test_up_guard_checked_before_trap() {
  # A live fixture must make `up` refuse and stop — not tear the live
  # fixture down as a side effect of the very EXIT trap meant to protect it. The bug is in the CLI
  # dispatch's trap-then-guard ordering, invisible to any test that calls the up()/down() shell
  # functions directly (as every other case in this file does) — this one runs the real run.sh
  # binary as a subprocess against an isolated fake repo root, so it never touches this repo's own
  # smoke state.
  local abs_runner fake_root dummy
  abs_runner="$(cd "$(dirname "$runner")" && pwd)/$(basename "$runner")"
  fake_root=$(mktemp -d)
  trap 'kill "$dummy" 2>/dev/null; rm -rf "$fake_root"' RETURN
  mkdir -p "$fake_root/smoke/docker/lane/.state"
  cp "$abs_runner" "$fake_root/smoke/docker/lane/run.sh"
  cp "$(dirname "$abs_runner")/../codex-lib.sh" "$fake_root/smoke/docker/codex-lib.sh"
  sleep 100 &
  dummy=$!
  {
    printf 'COMPOSE_PROJECT_NAME=%q\n' "pre-existing-project"
    printf 'bridge_tunnel_pid=%q\ndaemon_pid=%q\n' "$dummy" "$dummy"
  } >"$fake_root/smoke/docker/lane/.state/fixture.env"
  local before
  before=$(cat "$fake_root/smoke/docker/lane/.state/fixture.env")

  local output status
  status=0
  output=$(cd "$fake_root" && bash smoke/docker/lane/run.sh up 2>&1) || status=$?
  [[ "$status" -ne 0 ]] || { echo "expected 'up' to refuse against a live fixture" >&2; return 1; }
  [[ "$output" == *"fixture already up"* ]] || { echo "wrong refusal message: $output" >&2; return 1; }
  kill -0 "$dummy" 2>/dev/null || {
    echo "'up' tore down the live fixture's process while refusing to start against it" >&2
    return 1
  }
  local after
  after=$(cat "$fake_root/smoke/docker/lane/.state/fixture.env")
  [[ "$after" == "$before" ]] || { echo "the live fixture file was modified" >&2; return 1; }
}

test_load_fixture_restores_bridge_port() {
  # load_fixture must re-export LANE_BRIDGE_PORT, the name compose.yaml's bridge-tunnel service interpolates — any
  # compose command that recreates that container in a single-clause run would silently fall back to the default
  # port, and the host tunnel would forward to nothing.
  local test_state dummy1 dummy2
  test_state=$(mktemp -d)
  trap 'rm -rf "$test_state"; kill "$dummy1" "$dummy2" 2>/dev/null' RETURN
  state="$test_state"
  fixture="$state/fixture.env"
  sleep 30 & dummy1=$!
  sleep 30 & dummy2=$!
  {
    printf 'COMPOSE_PROJECT_NAME=%q\n' "test-project"
    printf 'daemon_pid=%q\nbridge_tunnel_pid=%q\n' "$dummy1" "$dummy2"
    printf 'LANE_BRIDGE_PORT=%q\n' "40123"
  } >"$fixture"

  unset LANE_BRIDGE_PORT
  load_fixture
  [[ "${LANE_BRIDGE_PORT:-}" == "40123" ]] || {
    echo "LANE_BRIDGE_PORT=${LANE_BRIDGE_PORT:-<unset>} after load_fixture, want 40123" >&2
    return 1
  }
}

test_agent_choice_lists_every_agent() {
  local agent_error='agent required: lane — Primary lane agent; decoy — Containment probe'
  sb() { printf '%s\n' "$agent_error"; return 1; }
  spawn() { echo agent:b:sb-test; }
  close_lane() { :; }
  pass() { :; }
  if (check_clause agent-choice >/dev/null 2>&1); then
    echo 'agent-choice accepted an error that omitted b' >&2
    return 1
  fi
  agent_error='agent required: lane — Primary lane agent; b — Second lane agent; decoy — Containment probe'
  check_clause agent-choice >/dev/null
}

test_directive_neutralized_waits_for_turn_completion_before_close() {
  # The mock-log wait only proves the woken Delivery turn's prompt reached
  # the model. It does not prove OpenClaw finished the turn: close_lane could run while the
  # Gateway still considers it active. This stub
  # answers the first session.list check with running:true and only the second with running:false,
  # so a clause that trusts a single read (or never checks session.list at all) fails here.
  local calls
  calls=$(mktemp)
  trap 'rm -f "$calls"' RETURN
  spawn() { echo agent:lane:sb-test; }
  answer() { echo 'ECHO[model=mock-a]: hi'; }
  mock_log() { printf '{"lastUser":"a\\u200b/model mock/mock-b"}\n{"lastUser":"b\\u200b/model mock/mock-b"}\n'; }
  gateway_list_row() { printf '{"model":"mock-a"}'; }
  pass() { :; }
  close_lane() { echo close_lane >>"$calls"; }
  # rpc's own call to session.list runs inside a `$(...)` command substitution in the clause (a
  # subshell), so a plain shell counter reset on every call — the call count has to live in the
  # calls file, which every invocation (subshell or not) shares.
  rpc() {
    case "$1" in
      message.send) printf '{"deliveries":[{"disposition":"injected"}]}' ;;
      session.list)
        local n
        n=$(( $(grep -c '^session.list:' "$calls") + 1 ))
        printf 'session.list:%d\n' "$n" >>"$calls"
        printf 'session.list.params:%s\n' "$2" >>"$calls"
        if (( n < 2 )); then printf '{"sessions":[{"running":true}]}'; else printf '{"sessions":[{"running":false}]}'; fi
        ;;
      *) printf '{}' ;;
    esac
  }
  check_clause directive-neutralized

  grep -q '^session.list:1$' "$calls" || { echo "directive-neutralized never checked session.list before closing" >&2; return 1; }
  grep -q '^session.list.params:{"session_id":"agent:lane:sb-test"}$' "$calls" || { echo "session.list did not filter by the woken lane ID" >&2; return 1; }
  grep -q '^session.list:2$' "$calls" || {
    echo "directive-neutralized closed after a single session.list check — it must poll until running is false, not trust the first read" >&2
    return 1
  }
  [[ "$(tail -1 "$calls")" == close_lane ]] || {
    echo "close_lane did not run strictly after the last session.list check: $(cat "$calls")" >&2
    return 1
  }
}

test_admin_guard_documented() {
  local smoke_readme=smoke/docker/lane/README.md lane_readme=lane/README.md
  grep -q 'lane-voice.*admin-guard' "$smoke_readme" || { echo "smoke clause list omits lane-voice or admin-guard" >&2; return 1; }
  grep -q 'exec.approvals.get' "$lane_readme" || { echo "lane README omits the admin probe" >&2; return 1; }
  grep -Eq 'fails closed|fail-closed' "$lane_readme" || { echo "lane README omits fail-closed behavior" >&2; return 1; }
  grep -q 'not cached' "$lane_readme" || { echo "lane README omits retry behavior" >&2; return 1; }
  grep -q 'admin.*hard refusal' "$lane_readme" || { echo "lane README omits admin refusal" >&2; return 1; }
}

test_lane_ownership_defaults() {
  local OPENCLAW_IMAGE LANE_SMOKE_OWNERSHIP actual image expected
  LANE_SMOKE_OWNERSHIP=""
  while read -r image expected; do
    OPENCLAW_IMAGE="$image"
    actual=$(lane_ownership_mode) || return 1
    [[ "$actual" == "$expected" ]] || { echo "$image: got $actual, expected $expected" >&2; return 1; }
  done <<'CASES'
ghcr.io/openclaw/openclaw:2026.8.2 marker
ghcr.io/openclaw/openclaw:2026.9.6 marker
ghcr.io/openclaw/openclaw:2026.10.9 explicit
ghcr.io/openclaw/openclaw:2027.1.0 explicit
custom/image:latest explicit
custom/image:2026.8.2 marker
custom/image explicit
localhost:5000/custom/image explicit
custom/image@sha256:abcdef explicit
CASES
  OPENCLAW_IMAGE=custom/image:2026.10.9
  LANE_SMOKE_OWNERSHIP=marker
  [[ "$(lane_ownership_mode)" == marker ]] || return 1
  OPENCLAW_IMAGE=custom/image:2026.8.2
  LANE_SMOKE_OWNERSHIP=explicit
  [[ "$(lane_ownership_mode)" == explicit ]] || return 1
  LANE_SMOKE_OWNERSHIP=invalid
  if (lane_ownership_mode >/dev/null 2>&1); then echo 'invalid mode accepted' >&2; return 1; fi
}

test_lane_ownership_render() {
  local rendered_dir mode
  rendered_dir=$(mktemp -d)
  trap 'rm -rf "$rendered_dir"' RETURN
  for mode in marker explicit; do
    render_lane_config "$LANE_SMOKE_ROOT/openclaw.json5" "$rendered_dir/$mode.json" /tmp/lane /tmp/b /tmp/decoy test-token /tmp/audit "$mode"
    node - "$rendered_dir/$mode.json" "$mode" <<'NODE'
const assert = require('node:assert/strict');
const fs = require('node:fs');
const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const explicit = process.argv[3] === 'explicit';
assert.equal(config.agents.ownership, explicit ? 'explicit' : undefined);
assert.equal(config.agents.entries.lane.default, explicit ? undefined : true);
for (const key of ['heartbeat', 'systemAgent', 'authInheritance']) {
  assert.deepEqual(config.agents.defaults[key], explicit ? {agentId:'lane'} : undefined);
}
assert.equal(config.agents.defaults.model.primary, 'mock/mock-a');
assert.equal(config.agents.entries.lane.workspace, '/tmp/lane');
assert.equal(config.agents.entries.b.workspace, '/tmp/b');
assert.equal(config.agents.entries.decoy.workspace, '/tmp/decoy');
assert.equal(config.gateway.auth.token, 'test-token');
assert.equal(config.plugins.entries.sessionbus.config.lanes.auditLog, '/tmp/audit');
assert.equal(fs.statSync(process.argv[2]).mode & 0o777, 0o600);
NODE
  done
}

test_bridge_loss_clause() {
  local target="$1" test_dir mode="${2:-good}" output
  test_dir=$(mktemp -d)
  trap 'rm -rf "$test_dir"' RETURN
  tmp="$test_dir"
  local calls="$test_dir/calls"
  compose=(fake_compose)
  fake_compose() {
    echo "compose $*" >>"$calls"
    touch "$test_dir/restarted"
  }
  sleep() { echo "sleep $*" >>"$calls"; }
  wait_bridge() { echo wait_bridge >>"$calls"; }
  mock_log_mark() { echo 0; }
  mock_log_since() {
    echo '{"lastUser":"SLOW bridge-loss-mid-run"}'
    if [[ "$mode" == submitted ]]; then
      jq -nc --arg input "$(cat "$test_dir/input")" '{lastUser:$input}'
    fi
  }
  retained_terminal() {
    if [[ "$mode" == native-exit ]]; then
      echo '{"run_id":"r1","state":"unavailable","reason":"acp child exited code=1"}'
    else
      echo '{"run_id":"r1","state":"done","result":{"outcome":"completed","result":"ECHO retained"}}'
    fi
  }
  sb() {
    echo "$1 $2" >>"$calls"
    case "$1" in
      lane.spawn)
        jq -e '.auto_close_ms==0' <<<"$2" >/dev/null || return 1
        echo '{"session_id":"agent:lane:sb-test"}' ;;
      turn.run)
        if [[ "$2" == *BRIDGE_LOSS_REFUSED* ]]; then
          if [[ "$mode" == not-lost ]]; then
            echo '{"run_id":"r2","state":"done","result":{"outcome":"completed","result":"still connected"}}'; return
          fi
          jq -r '.input' <<<"$2" >"$test_dir/input"
          jq -nc --arg reason "$(case "$mode" in wrong-reason) echo other;; native-exit) echo 'lane native session exited';; *) echo 'lane bridge lost';; esac)" '{run_id:"r2",state:"unavailable",reason:$reason}'
        else
          echo '{"run_id":"r1","state":"done","result":{"outcome":"completed","result":"ECHO retained"}}'
        fi ;;
      turn.start) echo '{"run_id":"r1"}' ;;
      turn.status)
        if [[ "$target" == bridge-loss-mid-run && ! -f "$test_dir/restarted" ]]; then
          echo '{"run_id":"r1","state":"running"}'; return
        fi
        if [[ "$mode" == lost ]] || [[ -f "$test_dir/closed" ]]; then
          echo '{"error":{"code":"unknown_session"}}'; return 1
        fi
        if [[ "$mode" == native-exit ]]; then retained_terminal; return; fi
        jq -nc --arg text "$([[ "$mode" == changed ]] && echo changed || echo 'ECHO retained')" '{run_id:"r1",state:"done",result:{outcome:"completed",result:$text}}' ;;
      turn.wait) retained_terminal ;;
      session.close) touch "$test_dir/closed"; echo '{}' ;;
      turn.ack) echo '{}' ;;
      *) echo "unexpected $1" >&2; return 1 ;;
    esac
  }
  if [[ "$mode" != good && "$mode" != native-exit ]]; then
    if output=$(check_clause "$target" 2>&1); then
      echo "$target accepted $mode" >&2; return 1
    fi
    case "$mode" in
      lost) [[ "$output" == *'turn.status:'*unknown_session* ]] ;;
      changed) [[ "$output" == *'retained'* || "$output" == *'terminal not retained'* ]] ;;
      wrong-reason|not-lost) [[ "$output" == *'bridge was not lost'* ]] ;;
      submitted) [[ "$output" == *'refused input reached mock'* ]] ;;
    esac || { echo "wrong failure: $output" >&2; return 1; }
    return
  fi
  output=$(check_clause "$target" 2>&1) || { echo "$output" >&2; return 1; }
  [[ "$output" == *"$target: PASS"* ]] || return 1
  grep -q 'compose restart openclaw-gateway' "$calls" || return 1
  grep -q '^sleep 35$' "$calls" || return 1
  [[ "$(grep -c '^wait_bridge$' "$calls")" == 1 ]] || return 1
  # Recovery must precede the retention wait; no tunnel-restart fallback.
  awk '/^compose restart openclaw-gateway$/ {restart=1; next}
       /^wait_bridge$/ {if (!restart) exit 1; ready=1; next}
       /^sleep 35$/ {if (!ready) exit 1; waited=1}
       END {if (!waited) exit 1}' "$calls" || return 1
  ! grep -q 'compose restart bridge-tunnel' "$calls" || return 1
  [[ "$(grep -c '^turn.ack ' "$calls")" == 2 ]] || return 1
  grep -q 'turn.ack .*"run_id":"r1"' "$calls" || return 1
  grep -q 'turn.ack .*"run_id":"r2"' "$calls" || return 1
  if [[ "$target" == bridge-loss-mid-run ]]; then
    if [[ "$mode" == native-exit ]]; then
      [[ "$output" == *'state=unavailable'* && "$output" == *'lane native session exited'* ]] || return 1
    else
      [[ "$output" == *'state=done outcome=completed'* ]] || return 1
    fi
  fi
}

if [[ $# == 0 ]]; then
  # Run each focused case in a fresh shell: stubs must not leak between cases.
  while read -r focused; do
    bash "$0" "$focused"
    printf '%s: PASS\n' "$focused"
  done < <(sed -n 's/^  \([a-z][a-z0-9-]*\)).*/\1/p' "$0")
  exit 0
fi

case "${1:-}" in
  bridge-loss-native-exit) test_bridge_loss_clause bridge-loss-mid-run native-exit ;;
  lane-ownership-defaults) test_lane_ownership_defaults ;;
  lane-ownership-render) test_lane_ownership_render ;;
  bridge-loss-retain)
    test_bridge_loss_clause bridge-loss-retain
    for mode in lost changed wrong-reason submitted; do
      (test_bridge_loss_clause bridge-loss-retain "$mode")
    done ;;
  bridge-loss-not-lost)
    (test_bridge_loss_clause bridge-loss-mid-run not-lost)
    (test_bridge_loss_clause bridge-loss-retain not-lost) ;;
  bridge-loss-unknown-session)
    (test_bridge_loss_clause bridge-loss-retain lost)
    (test_bridge_loss_clause bridge-loss-mid-run lost) ;;
  bridge-loss-mid-run)
    test_bridge_loss_clause bridge-loss-mid-run
    for mode in lost changed wrong-reason submitted; do
      (test_bridge_loss_clause bridge-loss-mid-run "$mode")
    done ;;
  bridge-loss-docs)
    for name in bridge-loss-retain bridge-loss-mid-run; do
      grep -q "\`$name\`" smoke/docker/lane/README.md || { echo "README missing $name" >&2; exit 1; }
    done ;;
  cwd-permissions) test_cwd_permissions ;;
  spawn-names-unique) test_spawn_names_are_unique_per_call ;;
  provision-credential-helper-failure) test_provision_credential_reports_helper_failure ;;
  gateway-list-row-bare-key) test_gateway_list_row_strips_daemon_group_suffix ;;
  cwd-outside-refused-pending-scopes) test_cwd_outside_refused_checks_pending_and_scopes ;;
  admin-guard-container-default) test_admin_guard_targets_container_default_state ;;
  model-reasoning-compat) test_model_reasoning_compat ;;
  agents-ownership-default) test_agents_ownership_not_combined_with_default ;;
  permission-deadline) test_permission_deadline ;;
  permission-denial-source) test_permission_denial_source ;;
  agent-choice-all) test_agent_choice_lists_every_agent ;;
  gateway-config-not-world-writable) test_gateway_config_not_world_writable ;;
  gateway-config-directory-mount) test_gateway_config_is_directory_mount ;;
  lane-cli-config-path) test_lane_cli_sites_carry_config_path ;;
  mock-log-scoping) test_mock_log_scoping ;;
  lane-voice-recipient-scoped) test_lane_voice_recipient_scoped ;;
  lane-voice-delivery-recipient) test_lane_voice_delivery_must_target_recipient ;;
  lane-voice-turn-incomplete) test_lane_voice_fails_when_the_lane_turn_does_not_complete ;;
  busy-mid-clause) test_busy_mid_requires_written_receipt_and_the_reaction_in_the_run ;;
  busy-mid-codex-clause) test_busy_mid_codex_requires_written_receipt_and_saw_inject ;;
  load-fixture-bridge-port) test_load_fixture_restores_bridge_port ;;
  up-guard-before-trap) test_up_guard_checked_before_trap ;;
  lane-voice-sbsend) test_lane_voice_sends_through_the_lane_tool ;;
  up-failure-fixture) test_up_failure_leaves_a_cleanable_fixture ;;
  early-up-bridge-cleanup) test_early_up_failure_removes_bridge_dir ;;
  directive-neutralized-waits-turn) test_directive_neutralized_waits_for_turn_completion_before_close ;;
  admin-guard-docs) test_admin_guard_documented ;;
  bridge-handoff) test_bridge_handoff ;;
  *) echo "unknown focused test: ${1:-}" >&2; exit 2 ;;
esac
