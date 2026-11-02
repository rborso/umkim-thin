#!/usr/bin/env bash
# Fault injection for a target: power off the node that holds the VIP, reboot
# nodes gracefully and hard, and stop/start the whole cluster.
#
#   ./ci/faults.sh single
#   ./ci/faults.sh ha --vip
#   ./ci/faults.sh ha --reboot
#   ./ci/faults.sh ha --full-stop
#   ./ci/faults.sh ha all
#
# The VM -> hypervisor map comes from ci/lab.env (CI_SINGLE_VMS / CI_HA_VMS).
# After every fault the script waits for the API, the nodes, the app endpoints
# and the data markers, so a cluster that comes back "Ready" but broken fails.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/faults.sh single|ha [--vip|--reboot|--full-stop|all]}"
shift || true
MODE="${1:-all}"
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh
[ -f ci/lab.env ] && . ci/lab.env

fails=0
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }
say()  { printf '\n== %s ==\n' "$*"; }

vms="${CI_SINGLE_VMS:-}"
[ "$TARGET" = "ha" ] && vms="${CI_HA_VMS:-}"
[ -n "$vms" ] || { echo "no VMs for $TARGET in ci/lab.env" >&2; exit 2; }

# vm_list[i] / hv_list[i] / ip_list[i] line up: the CI_*_VMS order is the
# servers order in the provision env.
vm_list=(); hv_list=(); ip_list=()
set -- $vms
for spec in "$@"; do vm_list+=("${spec%@*}"); hv_list+=("${spec#*@}"); done
set -- $SERVERS
for ip in "$@"; do ip_list+=("$ip"); done

virsh_do() { # virsh_do <hv> <args...>
  local hv="$1"; shift
  ssh -i "$KEY" -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=no "linux@$hv" \
    "sudo virsh $*"
}
dom_state() { virsh_do "$1" "domstate '$2'" 2>/dev/null | tr -d '\r' | head -1; }

wait_ssh() { # wait_ssh <ip> [timeout]
  local ip="$1" t="${2:-300}" i
  for i in $(seq 1 $((t / 5))); do
    if ssh -i "$KEY" -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no "${VUSER}@${ip}" true 2>/dev/null; then return 0; fi
    sleep 5
  done
  return 1
}

# kubectl from whichever node is up (the primary may be the one we powered off).
k() {
  local ip out
  for ip in "${ip_list[@]}"; do
    if out=$(node_at "$ip" "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl $*" 2>/dev/null); then
      printf '%s\n' "$out"; return 0
    fi
  done
  return 1
}

wait_api() { # wait_api [timeout]
  local t="${1:-300}" i
  for i in $(seq 1 $((t / 5))); do
    if k "get --raw=/readyz" >/dev/null 2>&1; then return 0; fi
    sleep 5
  done
  return 1
}

wait_nodes() { # wait_nodes [timeout]
  local t="${1:-420}" i out
  for i in $(seq 1 $((t / 5))); do
    out=$(k "get nodes --no-headers" 2>/dev/null || true)
    if [ -n "$out" ] && [ "$(printf '%s\n' "$out" | awk '{print $2}' | sort -u)" = "Ready" ]; then return 0; fi
    sleep 5
  done
  return 1
}

app_http() { # app_http <host>
  local h="$1" addr="${k3s_control_plane_vip:-$VHOST}"
  curl -sk -m 10 -o /dev/null -w '%{http_code}' --resolve "$h:443:$addr" "https://$h/" 2>/dev/null || echo 000
}

wait_pods() { # wait_pods [timeout] - every workload pod is Running and ready
  # Only steady-state workloads count. A Job or a bare helper pod (the marker
  # pods) may legitimately be Completed, Pending or Unknown after a reboot and
  # is not what "the cluster is back" means.
  local t="${1:-600}" i bad
  for i in $(seq 1 $((t / 10))); do
    bad=$(k "get pods -A -o json" 2>/dev/null | jq '
      [.items[]
       | select((.metadata.ownerReferences // []) as $o
                | ($o | any(.kind == "ReplicaSet" or .kind == "StatefulSet"
                            or .kind == "DaemonSet" or .kind == "ReplicationController")))
       | select(.status.phase != "Running"
                or ([.status.containerStatuses[]? | .ready] | any(. == false))
                or ([.status.containerStatuses[]?] | length) == 0)
      ] | length' 2>/dev/null) || { sleep 10; continue; }
    [ "${bad:-1}" -eq 0 ] && return 0
    sleep 10
  done
  return 1
}

wait_longhorn() { # wait_longhorn [timeout] - no volume is faulted/unknown
  # A node loss leaves volumes degraded (that is fine: the surviving replicas
  # serve); faulted or unknown volumes mean the apps cannot come back.
  local t="${1:-600}" i bad
  for i in $(seq 1 $((t / 10))); do
    bad=$(k "get volumes.longhorn.io -n longhorn-system -o json" 2>/dev/null | jq '
      [.items[] | select(.status.robustness != "healthy" and .status.robustness != "degraded")] | length' 2>/dev/null) || { sleep 10; continue; }
    [ "${bad:-1}" -eq 0 ] && return 0
    sleep 10
  done
  return 1
}

wait_apps() { # wait_apps [timeout]
  local t="${1:-600}" i a b c d
  for i in $(seq 1 $((t / 10))); do
    a=$(app_http "immich.$ZONE"); b=$(app_http "gitea.$ZONE")
    c=$(app_http "paperless.$ZONE"); d=$(app_http "nextcloud.$ZONE")
    case "$a$b$c$d" in
      *000*) ;;
      *)
        case "$a" in 200|301|302|303|307|308|401|403) case "$b" in 200|301|302|303|307|308|401|403) case "$c" in 200|301|302|303|307|308|401|403) case "$d" in 200|301|302|303|307|308|401|403) return 0;; esac;; esac;; esac;; esac ;;
    esac
    sleep 10
  done
  return 1
}

check_data() {
  if ./ci/markers.sh verify "$TARGET" --mark "$MARK" >/tmp/ci-fault-markers.log 2>&1; then
    ok "data markers survived"
  else
    bad "data markers are missing (see /tmp/ci-fault-markers.log)"
    tail -20 /tmp/ci-fault-markers.log | sed 's/^/      /'
  fi
}

wait_healthy() { # wait_healthy <label>
  local label="$1"
  wait_api 300 && ok "$label: API is back" || bad "$label: API did not come back"
  wait_nodes 420 && ok "$label: all nodes Ready" || bad "$label: nodes are not Ready"
  wait_pods 600 && ok "$label: all pods are Running/Completed" || bad "$label: pods are not settled"
  wait_apps 600 && ok "$label: app endpoints answer" || bad "$label: app endpoints do not answer"
  check_data
}

vip_holder() { # echo "<index> <ip>" of the node carrying the VIP
  local i=0 ip
  for ip in "${ip_list[@]}"; do
    if node_at "$ip" "ip -4 addr show 2>/dev/null | grep -qw '$k3s_control_plane_vip' && echo yes" 2>/dev/null | grep -q yes; then
      echo "$i $ip"; return 0
    fi
    i=$((i + 1))
  done
  return 1
}

fault_vip() {
  say "HA: power off the VIP node"
  local pair i ip vm hv
  pair=$(vip_holder) || { bad "no node carries the VIP"; return; }
  i=${pair%% *}; ip=${pair#* }
  vm=${vm_list[$i]}; hv=${hv_list[$i]}
  say "VIP $k3s_control_plane_vip is on $ip ($vm @ $hv); hard power off"
  virsh_do "$hv" "destroy '$vm'" >/dev/null 2>&1 || { bad "virsh destroy $vm"; return; }
  # The VIP must move to one of the survivors.
  local moved=0
  for _ in $(seq 1 36); do
    sleep 5
    if vip_holder >/dev/null 2>&1; then moved=1; break; fi
  done
  [ "$moved" = 1 ] && ok "the VIP moved to a survivor" || bad "the VIP did not move"
  wait_api 300 && ok "API stayed available" || bad "API did not answer after the power loss"
  wait_nodes 420 && ok "the survivors are Ready" || bad "survivors are not Ready"
  # Longhorn's node-down grace is 5 minutes before it deletes the lost node's
  # pods; the reschedule and volume reattach come after that.
  wait_pods 1200 && ok "pods settled on the survivors" || bad "pods did not settle"
  wait_longhorn 1200 && ok "volumes are usable" || bad "volumes are not usable"
  wait_apps 1200 && ok "apps recovered without the node" || bad "apps did not recover"
  say "HA: start the powered-off node again"
  virsh_do "$hv" "start '$vm'" >/dev/null 2>&1 || { bad "virsh start $vm"; return; }
  wait_ssh "$ip" 900 && ok "$vm is reachable again" || bad "$vm did not come back"
  wait_nodes 600 && ok "the node rejoined" || bad "the node did not rejoin"
  wait_apps 600 && ok "apps are healthy after the rejoin" || bad "apps are not healthy after the rejoin"
  check_data
}

fault_reboot() {
  local n i vm hv ip
  say "reboot each node (graceful, then a hard reboot of the first)"
  for i in "${!vm_list[@]}"; do
    vm=${vm_list[$i]}; hv=${hv_list[$i]}; ip=${ip_list[$i]}
    say "graceful reboot $vm ($ip)"
    virsh_do "$hv" "reboot '$vm'" >/dev/null 2>&1 || { bad "virsh reboot $vm"; continue; }
    sleep 15
    wait_ssh "$ip" 900 && ok "$vm is back" || bad "$vm did not come back"
    wait_api 300 && ok "API after $vm" || bad "API after $vm"
    wait_nodes 420 && ok "nodes after $vm" || bad "nodes after $vm"
    wait_pods 600 && ok "pods after $vm" || bad "pods after $vm"
    wait_longhorn 600 && ok "volumes after $vm" || bad "volumes after $vm"
    wait_apps 900 && ok "apps after $vm" || bad "apps after $vm"
  done
  vm=${vm_list[0]}; hv=${hv_list[0]}; ip=${ip_list[0]}
  say "hard reboot $vm ($ip)"
  virsh_do "$hv" "destroy '$vm'" >/dev/null 2>&1 || { bad "virsh destroy $vm"; return; }
  sleep 5
  virsh_do "$hv" "start '$vm'" >/dev/null 2>&1 || { bad "virsh start $vm"; return; }
  wait_ssh "$ip" 900 && ok "$vm is back" || bad "$vm did not come back"
  wait_api 300 && ok "API after the hard reboot" || bad "API after the hard reboot"
  wait_nodes 420 && ok "nodes after the hard reboot" || bad "nodes after the hard reboot"
  wait_pods 600 && ok "pods after the hard reboot" || bad "pods after the hard reboot"
  wait_longhorn 600 && ok "volumes after the hard reboot" || bad "volumes after the hard reboot"
  wait_apps 900 && ok "apps after the hard reboot" || bad "apps after the hard reboot"
  check_data
}

fault_full_stop() { # fault_full_stop <graceful|hard>
  local how="$1" i vm hv
  say "full cluster stop ($how), then start all nodes"
  for i in "${!vm_list[@]}"; do
    vm=${vm_list[$i]}; hv=${hv_list[$i]}
    if [ "$how" = hard ]; then
      virsh_do "$hv" "destroy '$vm'" >/dev/null 2>&1 || bad "virsh destroy $vm"
    else
      virsh_do "$hv" "shutdown '$vm'" >/dev/null 2>&1 || bad "virsh shutdown $vm"
    fi
  done
  # Wait for every domain to be off. A graceful shutdown drains k3s first and
  # can take several minutes, so allow 15 and force off whatever is left.
  for _ in $(seq 1 180); do
    off=1
    for i in "${!vm_list[@]}"; do
      st=$(dom_state "${hv_list[$i]}" "${vm_list[$i]}")
      [ "$st" = "shut off" ] || off=0
    done
    [ "$off" = 1 ] && break
    sleep 5
  done
  if [ "${off:-0}" != 1 ]; then
    for i in "${!vm_list[@]}"; do
      [ "$(dom_state "${hv_list[$i]}" "${vm_list[$i]}")" = "shut off" ] || \
        virsh_do "${hv_list[$i]}" "destroy '${vm_list[$i]}'" >/dev/null 2>&1 || true
    done
    sleep 5
    off=1
    for i in "${!vm_list[@]}"; do
      [ "$(dom_state "${hv_list[$i]}" "${vm_list[$i]}")" = "shut off" ] || off=0
    done
  fi
  [ "${off:-0}" = 1 ] && ok "all nodes are off" || bad "not all nodes stopped ($how)"

  # Starting can race a domain that is still finishing its shutdown, so retry.
  for i in "${!vm_list[@]}"; do
    st=$(dom_state "${hv_list[$i]}" "${vm_list[$i]}")
    if [ "$st" != "shut off" ]; then bad "${vm_list[$i]} is $st before start"; continue; fi
    started=0
    for _ in 1 2 3 4 5 6; do
      if virsh_do "${hv_list[$i]}" "start '${vm_list[$i]}'" >/dev/null 2>&1; then started=1; break; fi
      sleep 5
    done
    [ "$started" = 1 ] || bad "virsh start ${vm_list[$i]}"
  done
  for i in "${!vm_list[@]}"; do
    wait_ssh "${ip_list[$i]}" 900 >/dev/null 2>&1 || true
  done
  wait_api 900 && ok "API is back after the full stop" || bad "API did not come back after the full stop"
  wait_nodes 900 && ok "all nodes rejoined" || bad "not all nodes rejoined"
  wait_pods 1200 && ok "all pods are back" || bad "not all pods are back"
  wait_longhorn 1200 && ok "volumes are back" || bad "volumes are not back"
  wait_apps 1200 && ok "apps are back" || bad "apps are not back"
  check_data
}

MARK="${UMKIM_THIN_MARK:-}"
[ -n "$MARK" ] || MARK="ci-marker-${UMKIM_THIN_CI_RUN:-$(date +%s)}"

# The data check verifies markers that already exist. The extended run creates
# them earlier; a standalone run has to create its own, or every check reports
# the data as missing.
if [ -z "${UMKIM_THIN_MARK:-}" ]; then
  # A standalone run may start right after a reboot, while the apps are still
  # coming back; wait for them before writing the markers.
  wait_apps 900 >/dev/null 2>&1 || true
  if ! ./ci/markers.sh create "$TARGET" --mark "$MARK" >/tmp/ci-fault-markers-create.log 2>&1; then
    sleep 60
    ./ci/markers.sh create "$TARGET" --mark "$MARK" >/tmp/ci-fault-markers-create.log 2>&1 \
      || { bad "could not create the data markers"; sed -n 1,20p /tmp/ci-fault-markers-create.log >&2; }
  fi
fi

case "$MODE" in
  all)
    if [ "$TARGET" = "ha" ]; then
      fault_vip
      fault_reboot
      fault_full_stop graceful
      fault_full_stop hard
    else
      fault_reboot
      fault_full_stop graceful
      fault_full_stop hard
    fi
    ;;
  --vip) [ "$TARGET" = "ha" ] && fault_vip || fault_reboot ;;
  --reboot) fault_reboot ;;
  --full-stop) fault_full_stop graceful ;;
  --full-stop-hard) fault_full_stop hard ;;
  *) echo "unknown mode $MODE" >&2; exit 2 ;;
esac

if [ "$fails" -ne 0 ]; then
  echo "fault injection found $fails problem(s)" >&2
  exit 1
fi
echo "fault injection passed"
