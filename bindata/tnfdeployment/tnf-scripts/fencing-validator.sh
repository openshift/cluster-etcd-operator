#!/usr/bin/env bash
set -euo pipefail
export LANG=C

# fencing-validator
# - Non-disruptive: verify STONITH present+enabled, both nodes online, etcd has 2 started non-learner members, Daemon status active
# - Disruptive (optional): fence node A then node B (one pass), auto-switching the conductor to avoid self-fencing
# - Transport: auto (prefer SSH to both), or forced ssh/ocdebug

usage() {
  cat <<'EOF'
Usage:
  fencing-validator [--user <ssh-user>] [--ssh-key <path>]
                       [--kubeconfig <path>]
                       [--transport auto|ssh|ocdebug]
                       [--hosts "<hostA,hostB>"] [--host-a <host>] [--host-b <host>]
                       [--disruptive] [--dry-run] [--timeout <sec>]

Examples (hosts):
  --hosts "10.0.0.10,10.0.0.11"
  --hosts "2001:db8::a,2001:db8::b"

Note: For IPv6, pass the raw address (no brackets). The script adds [ ] where needed.

Timeouts:
  --timeout <sec> / TIMEOUT
      Maximum time (in seconds) to wait for a condition to succeed.
      This is the overall loop timeout (e.g., waiting for a node or etcd to recover).
      Default: 1200 seconds (20 min).

  CMD_EXEC_TIMEOUT_SECS
      Maximum time (in seconds) allowed for a single remote command to run
      (e.g., one `podman exec`, one `pcs status` call).
      This is enforced in `host_run` for both SSH and oc debug transports.
      Default: 60 seconds.

  OC_REQ_TIMEOUT
      Per-request API timeout for the `oc` client when contacting the API server.
      Applies to each HTTP request inside `oc` commands, not the overall command runtime.
      Default: 10s.


Env (optional): SSH_USER, SSH_KEY, KUBECONFIG, TRANSPORT, DISRUPTIVE, DRY_RUN, TIMEOUT, IP_A, IP_B, OC_BIN, OC_REQ_TIMEOUT, CMD_EXEC_TIMEOUT_SECS
EOF
}

log(){ printf '\033[36m[INFO]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[WARN]\033[0m %s\n' "$*"; }
err(){ printf '\033[31m[ERROR]\033[0m %s\n' "$*" >&2; }
ok(){  printf '\033[32m[OK]\033[0m %s\n' "$*"; }

# -------- Defaults / args --------
SSH_USER="${SSH_USER:-core}"
SSH_KEY="${SSH_KEY:-}"
KUBECONFIG_PATH="${KUBECONFIG:-}"
TRANSPORT="${TRANSPORT:-auto}"
DISRUPTIVE="${DISRUPTIVE:-false}"
DRY_RUN="${DRY_RUN:-false}"
TIMEOUT="${TIMEOUT:-1200}"
IP_A="${IP_A:-}"
IP_B="${IP_B:-}"
OC_BIN="${OC_BIN:-oc}"
OC_REQ_TIMEOUT="${OC_REQ_TIMEOUT:-10s}"
CMD_EXEC_TIMEOUT_SECS="${CMD_EXEC_TIMEOUT_SECS:-60s}"

# valreq <flag> <value>
# Returns success (0) if <value> exists and is not another option (i.e., doesn't start with '-').
# Used in argument parsing to validate that an option expecting a value actually has one.
valreq(){ [[ -n "${2-}" && "$2" != -* ]]; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user)       valreq "$1" "${2-}" || { err "--user requires a value"; exit 1; }; SSH_USER="$2"; shift 2;;
    --ssh-key)    valreq "$1" "${2-}" || { err "--ssh-key requires a value"; exit 1; }; SSH_KEY="$2"; shift 2;;
    --kubeconfig) valreq "$1" "${2-}" || { err "--kubeconfig requires a value"; exit 1; }; KUBECONFIG_PATH="$2"; shift 2;;
    --transport)  valreq "$1" "${2-}" || { err "--transport requires a value"; exit 1; }; TRANSPORT="$2"; shift 2;;
    --timeout)    valreq "$1" "${2-}" || { err "--timeout requires a value"; exit 1; }; TIMEOUT="$2"; shift 2;;
    --hosts)      valreq "$1" "${2-}" || { err "--hosts requires a value like 'A,B'"; exit 1; }; IFS=',' read -r IP_A IP_B <<<"$2"; shift 2;;
    --host-a)     valreq "$1" "${2-}" || { err "--host-a requires a value"; exit 1; }; IP_A="$2"; shift 2;;
    --host-b)     valreq "$1" "${2-}" || { err "--host-b requires a value"; exit 1; }; IP_B="$2"; shift 2;;
    --disruptive) DISRUPTIVE=true; shift;;
    --dry-run)    DRY_RUN=true; shift;;
    -h|--help)    usage; exit 0;;
    *) err "Unknown arg: $1"; usage; exit 1;;
  esac
done

IP_A="${IP_A//[[:space:]]/}"; IP_B="${IP_B//[[:space:]]/}"

[[ "$TRANSPORT" =~ ^(auto|ssh|ocdebug)$ ]] || { err "--transport must be auto|ssh|ocdebug"; exit 1; }
[[ "$TIMEOUT" =~ ^[0-9]+$ ]] || { err "Invalid --timeout '$TIMEOUT'"; exit 1; }

[[ -n "$KUBECONFIG_PATH" ]] && export KUBECONFIG="$KUBECONFIG_PATH"
log "Checking cluster access with '$OC_BIN'..."
$OC_BIN --request-timeout="$OC_REQ_TIMEOUT" whoami >/dev/null

# -------- Basic helpers --------
_is_ipv6(){ [[ "$1" == *:* ]]; }
_fmt_host(){
  local h="$1"
  h="${h#[[]}"
  h="${h%[]]}"
  _is_ipv6 "$h" && echo "[$h]" || echo "$h"
}

ssh_cmd() {
  local host
  host="$(_fmt_host "$1")"
  shift
  local keyopt=(); [[ -n "$SSH_KEY" ]] && keyopt=(-i "$SSH_KEY")
  timeout "$CMD_EXEC_TIMEOUT_SECS" ssh \
    -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    "${keyopt[@]}" "${SSH_USER}@${host}" "$@"
}

oc_run(){ timeout "$CMD_EXEC_TIMEOUT_SECS" "$OC_BIN" --request-timeout="$OC_REQ_TIMEOUT" "$@"; }

host_run() {
  local target="$1"; shift
  local raw="$*"
  local cmd="$raw"
  cmd=${cmd//\'/\'"\'"\'}
  if [[ "$TRANSPORT" == ssh ]]; then
    ssh_cmd "$target" "sudo -n bash -lc '$cmd'"
  else
    oc_run debug -q node/"$target" -- chroot /host bash -lc "$raw"
  fi
}

ocdebug_ok(){ oc_run debug -q node/"$1" -- chroot /host true >/dev/null 2>&1; }
ssh_ok(){ ssh_cmd "$1" true >/dev/null 2>&1; }

_sudo_check() {
  for h in "$IP_A" "$IP_B"; do
    ssh_cmd "$h" "sudo -n true" >/dev/null 2>&1 || {
      err "Passwordless sudo required on $h for SSH mode."; exit 1; }
  done
}

# -------- Discover nodes --------
log "Detecting control-plane nodes…"
mapfile -t A < <(timeout "$CMD_EXEC_TIMEOUT_SECS" "$OC_BIN" get nodes -l node-role.kubernetes.io/master= -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
mapfile -t B < <(timeout "$CMD_EXEC_TIMEOUT_SECS" "$OC_BIN" get nodes -l node-role.kubernetes.io/control-plane= -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

declare -a CP_NODES=(); declare -A seen=()
for n in "${A[@]}" "${B[@]}"; do [[ -z "${seen["$n"]+x}" ]] && { CP_NODES+=("$n"); seen["$n"]=1; }; done
[[ ${#CP_NODES[@]} -eq 2 ]] || { err "Expected exactly 2 control-plane nodes, got ${#CP_NODES[@]}: ${CP_NODES[*]-}"; exit 1; }
NODE_A="${CP_NODES[0]}"; NODE_B="${CP_NODES[1]}"

get_internal_ip() {
  local node="$1" addrs ip4 ip6
  addrs="$("$OC_BIN" get node "$node" -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}' 2>/dev/null || true)"
  ip4="$(grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' <<<"$addrs" | head -n1 || true)"
  ip6="$(grep -E '^[0-9A-Fa-f:]+$' <<<"$addrs" | grep -vi '^fe80:' | head -n1 || true)"
  [[ -n "$ip4" ]] && echo "$ip4" || echo "$ip6"
}

[[ -z "$IP_A" ]] && IP_A="$(get_internal_ip "$NODE_A" || true)"; [[ -z "$IP_A" ]] && IP_A="$NODE_A"
[[ -z "$IP_B" ]] && IP_B="$(get_internal_ip "$NODE_B" || true)"; [[ -z "$IP_B" ]] && IP_B="$NODE_B"

# -------- Transport + conductor --------
CONDUCTOR=""
case "$TRANSPORT" in
  auto)
    if ssh_ok "$IP_A" && ssh_ok "$IP_B"; then
      TRANSPORT=ssh; CONDUCTOR="$IP_B"; _sudo_check; log "Using SSH; conductor=$CONDUCTOR"
    elif ocdebug_ok "$NODE_A" && ocdebug_ok "$NODE_B"; then
      TRANSPORT=ocdebug; CONDUCTOR="$NODE_B"; log "Using oc debug; conductor=$CONDUCTOR"
    else
      err "Auto mode: neither SSH nor oc debug reachable on both nodes."; exit 1
    fi
    ;;
  ssh)
    (ssh_ok "$IP_A" && ssh_ok "$IP_B") || { err "SSH not available to both nodes"; exit 1; }
    CONDUCTOR="$IP_B"; _sudo_check; log "Using SSH; conductor=$CONDUCTOR"
    ;;
  ocdebug)
    (ocdebug_ok "$NODE_A" && ocdebug_ok "$NODE_B") || { err "oc debug not available on both nodes"; exit 1; }
    CONDUCTOR="$NODE_B"; log "Using oc debug; conductor=$CONDUCTOR"
    ;;
esac

# -------- Pacemaker names --------
short() { echo "${1%%.*}"; }
resolve_pcmk_name() {
  local n="$1"
  if host_run "$CONDUCTOR" "command -v crm_node >/dev/null 2>&1"; then
    host_run "$CONDUCTOR" "crm_node -l" | awk -v n="$n" -v s="$(short "$n")" '$2==n||$2==s{print $2; found=1} END{if(!found) print s}'
  else
    short "$n"
  fi
}
PCMK_A="$(resolve_pcmk_name "$NODE_A")"
PCMK_B="$(resolve_pcmk_name "$NODE_B")"
log "Mapping: $NODE_A -> $PCMK_A ; $NODE_B -> $PCMK_B"

# -------- Health/waits --------
node_ready(){
  local n="$1"
  local s; s="$("$OC_BIN" get node "$n" --request-timeout=10s -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)"
  [[ "$s" == *True* ]]
}

pcs_nodes_line(){
  local out
  out="$(host_run "$CONDUCTOR" "pcs status nodes 2>/dev/null || crm_mon -1 2>/dev/null" 2>/dev/null || true)"
  awk '/^[[:space:]]*Online:/{print; exit}' <<<"$out"
}

pcs_nodes_names() {
  local out
  out="$(host_run "$CONDUCTOR" "pcs status nodes 2>/dev/null || crm_mon -1 2>/dev/null" 2>/dev/null || true)"
  awk '/^[[:space:]]*Online:/{ 
    for (i=2;i<=NF;i++) { gsub(/[][]/,"",$i); printf "%s ", $i } 
  } END{ print "" }' <<<"$out"
}

pcmkonline() {
  local want="$1" s="${1%%.*}" names
  names="$(pcs_nodes_names)"
  [[ -n "$names" ]] || return 1
  names=" $names "
  [[ "$names" == *" $want "* || "$names" == *" $s "* ]]
}

wait_not_ready(){
  local n="$1" deadline=$((SECONDS+TIMEOUT))
  log "Waiting for '$n' to become NotReady (API)"
  while (( SECONDS < deadline )); do
    if ! node_ready "$n"; then
      ok "$n NotReady (API)"
      return 0
    fi
    sleep 5
  done
  err "Timeout waiting for $n NotReady"
  return 1
}

wait_ready(){
  local n="$1" deadline=$((SECONDS+TIMEOUT))
  log "Waiting for '$n' Ready (API)…"
  while (( SECONDS < deadline )); do
    node_ready "$n" && { ok "$n Ready (API)"; break; }
    sleep 8
  done
  (( SECONDS < deadline )) || { err "Timeout waiting Ready for $n"; return 1; }
  deadline=$((SECONDS+TIMEOUT))
  log "Waiting for '$n' ONLINE (Pacemaker)…"
  while (( SECONDS < deadline )); do
    pcmkonline "$n" && { ok "$n ONLINE (Pacemaker)"; return 0; }
    sleep 6
  done
  err "Timeout waiting Pacemaker ONLINE for $n"; return 1
}

stonith_show(){
  host_run "$CONDUCTOR" \
    "(pcs stonith config || pcs stonith status || pcs stonith show) 2>&1" \
    || true
}

check_stonith(){
  log "Checking STONITH…"
  local out; out="$(stonith_show)"
  [[ -n "$out" ]] || { err "No STONITH devices detected (pcs returned empty output)"; return 1; }
  host_run "$CONDUCTOR" \
    'pcs property config stonith-enabled 2>&1 || pcs property list stonith-enabled 2>&1 || pcs property show --all stonith-enabled 2>&1' \
  | grep -Eqi 'stonith-enabled[^[:alnum:]]*true' \
    || { err "stonith-enabled=false (or not reported)"; return 1; }

  ok "STONITH present and enabled"
}

check_daemon_status(){
  log "Checking Pacemaker daemon status…"
  local out ds ok_all=1
  out="$(host_run "$CONDUCTOR" "pcs status --full 2>/dev/null || pcs status 2>/dev/null" 2>/dev/null || true)"
  ds="$(awk '/^Daemon Status:/,0{print}' <<<"$out")"
  [[ -n "$ds" ]] || { err "Daemon Status section not found in 'pcs status' output"; return 1; }

  for svc in corosync pacemaker pcsd; do
    if ! grep -qiE "^[[:space:]]*$svc:[[:space:]]*(active|running)" <<<"$ds"; then
      err "Daemon '$svc' not active/enabled"
      ok_all=0
    fi
  done

  if (( ok_all == 1 )); then
    ok "Daemon Status: corosync, pacemaker, pcsd active/enabled"
  else
    return 1
  fi
}

# etcd: need 2 started, non-learner voters
node_exec_target(){
  if [[ "$TRANSPORT" == "ssh" ]]; then
    [[ "$1" == "$NODE_A" ]] && echo "$IP_A" || echo "$IP_B"
  else
    echo "$1"
  fi
}

etcd_two_started() {
  local tgt="$1" out rc
  out="$(host_run "$tgt" "podman exec etcd sh -lc 'ETCDCTL_API=3 etcdctl member list -w table'" 2>&1)"
  rc=$?
  if grep -qE '^\|' <<<"$out"; then
    awk -F'|' '/^\|/{
      for(i=1;i<=NF;i++){gsub(/^[ \t]+|[ \t]+$/,"",$i)}
      if(tolower($3)=="started" && tolower($7)=="false") c++
    } END{ exit !(c>=2) }' <<<"$out"
    return
  fi
  if (( rc != 0 )); then
    if grep -Eqi 'no such container|container state improper|not running|missing required container_id' <<<"$out"; then
      return 1
    fi
    err "etcdctl failed on $tgt: ${out##*$'\n'}"
    return 2
  fi
  return 1
}

etcd_ready(){
  etcd_two_started "$(node_exec_target "$NODE_A")"
  local ra
  ra=$?
  (( ra==0 )) && return 0
  (( ra==2 )) && return 2

  etcd_two_started "$(node_exec_target "$NODE_B")"
  local rb
  rb=$?
  (( rb==0 )) && return 0
  (( rb==2 )) && return 2
  return 1
}

wait_etcd(){
  local deadline=$((SECONDS + TIMEOUT / 2)) rc start=$SECONDS next=$((SECONDS + 30))
  log "Waiting for etcd to report 2 started non-learner members (max wait: $((TIMEOUT/2))s)…"
  while (( SECONDS < deadline )); do
    etcd_ready; rc=$?
    (( rc==0 )) && { ok "etcd has 2 started voters (waited $((SECONDS-start))s)"; return 0; }
    (( rc==2 )) && { err "etcd check failed (fatal) after $((SECONDS-start))s"; return 1; }
    (( SECONDS >= next )) && {
      warn "[$((SECONDS-start))s elapsed, $((deadline-SECONDS))s remaining] Current etcd member status:"
      host_run "$(node_exec_target "$NODE_A")" "podman exec etcd sh -lc 'ETCDCTL_API=3 etcdctl member list -w table' 2>/dev/null | head -n 5" || true
      next=$((SECONDS + 30))
    }
    sleep 6
  done
  err "Timeout waiting for etcd quorum (2 started voters)"
}

# -------- Conductor switch + fencing --------
switch_conductor_for(){
  local target="$1"
  if [[ "$TRANSPORT" == "ssh" ]]; then
   local want
   if [[ "$target" == "$NODE_A" ]]; then
     want="$IP_A"
   else
     want="$IP_B"
   fi
    [[ "$CONDUCTOR" == "$want" ]] && CONDUCTOR="$([[ "$target" == "$NODE_A" ]] && echo "$IP_B" || echo "$IP_A")"
  else
    [[ "$CONDUCTOR" == "$target" ]] && CONDUCTOR="$([[ "$target" == "$NODE_A" ]] && echo "$NODE_B" || echo "$NODE_A")"
  fi
  return 0
}

fence(){
  local t="$1"
  log "[ACTION] Fencing (reboot) '$t' from '$CONDUCTOR'..."
  local to=$(( TIMEOUT/2 )); (( to < 1 )) && to=1

  if [[ "$TRANSPORT" == "ocdebug" ]]; then
    if ! host_run "$CONDUCTOR" "command -v systemd-run >/dev/null 2>&1 && \
        systemd-run --unit fence-$t --collect bash -lc 'pcs stonith fence $t' \
        || nohup bash -lc 'pcs stonith fence $t' >/var/tmp/fence-$t.log 2>&1 & disown"; then
      err "Dispatching fence for '$t' via ocdebug failed to start"
      return 1
    fi
    sleep 2
    return 0
  fi

  if ! host_run "$CONDUCTOR" "timeout $to pcs stonith fence $t"; then
    err "Fencing '$t' failed or timed out (${to}s)"
    return 1
  fi
}

# --- DRY RUN helper ---
dry_run_plan() {
  local c1="$CONDUCTOR" c2
  if [[ "$TRANSPORT" == "ssh" ]]; then
    c2=$([[ "$c1" == "$IP_B" ]] && echo "$IP_A" || echo "$IP_B")
  else
    c2=$([[ "$c1" == "$NODE_B" ]] && echo "$NODE_A" || echo "$NODE_B")
  fi
  log "[DRY-RUN] Would fence $PCMK_A from $c1, then $PCMK_B from $c2"
}


# -------- Run --------
log "Mode: transport=$TRANSPORT disruptive=$DISRUPTIVE dry-run=$DRY_RUN"
log "=== Non-disruptive validation ==="
check_stonith
if ! ( pcmkonline "$PCMK_A" && pcmkonline "$PCMK_B" ); then
  err "Both nodes must be ONLINE (Pacemaker)"
  exit 2
fi
ok "Both nodes ONLINE"
check_daemon_status || exit 2
wait_etcd || exit 2
ok "[PASS] Non-disruptive checks complete"

if ! $DISRUPTIVE; then
  $DRY_RUN && dry_run_plan
  echo "Done (non-disruptive)."
  exit 0
fi

$DRY_RUN && { dry_run_plan; exit 0; }

log "=== Disruptive validation ==="
# Fence A
switch_conductor_for "$NODE_A"
log "Fencing $NODE_A (PCMK: $PCMK_A)"
fence "$PCMK_A"
wait_not_ready "$NODE_A"; wait_ready "$NODE_A"; wait_etcd || exit 2; check_daemon_status || exit 2

# Fence B
switch_conductor_for "$NODE_B"
log "Fencing $NODE_B (PCMK: $PCMK_B)"
wait_etcd || { err "etcd not stable; refusing to fence $NODE_B"; exit 2; }
fence "$PCMK_B"
wait_not_ready "$NODE_B"; wait_ready "$NODE_B"; wait_etcd || exit 2; check_daemon_status || exit 2

ok "Disruptive validation PASSED"