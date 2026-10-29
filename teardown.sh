#!/usr/bin/env bash
# Remove umkim-thin and its k3s cluster from one host.
#
#   sudo ./teardown.sh
#   sudo ./teardown.sh --dry-run
#   sudo ./teardown.sh --purge-data --keep-firewall
#
# By default the host state (`/etc/umkim-thin`), the chart mirror
# (`/var/lib/umkim-thin`), Longhorn data (`/var/lib/longhorn`) and the NFS
# exports on `/srv/nfs` are kept. k3s storage, however, lives under
# `/var/lib/rancher/k3s` and is removed by k3s's own uninstall script; back up
# app data before running this on a host you care about.
set -euo pipefail

ASSUME_YES=0
DRY_RUN=0
PURGE_DATA=0
KEEP_TOOLS=0
KEEP_WIREGUARD=0
KEEP_FIREWALL=0
FAILURES=0

STATE_DIR="${UMKIM_THIN_STATE_DIR:-/etc/umkim-thin}"
DATA_DIR="${UMKIM_THIN_DATA_DIR:-/var/lib/umkim-thin}"
REMOTE_DIR="${UMKIM_THIN_REMOTE_DIR:-/opt/umkim-thin}"
KUBECONFIG="${UMKIM_THIN_KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
K3S_DATA_DIR="${K3S_DATA_DIR:-/var/lib/rancher/k3s}"

WEB_NODEPORT="${WEB_NODEPORT:-30080}"
TLS_NODEPORT="${TLS_NODEPORT:-30443}"
GIT_NODEPORT="${GIT_NODEPORT:-30222}"
PUB_GIT="${PUB_GIT:-2222}"
PUB_WEB="${PUB_WEB:-80}"
PUB_TLS="${PUB_TLS:-443}"
WG_PORT="${WG_PORT:-51820}"
TRAEFIK_VPN_PORT="${TRAEFIK_VPN_PORT:-30444}"

# Set by uninstall-ha.sh (or K3S_CONTROL_PLANE_VIP/K3S_VIP for a manual run) so
# a stale kube-vip address left on the interface is removed with the cluster.
CONTROL_PLANE_VIP="${K3S_CONTROL_PLANE_VIP:-${K3S_VIP:-}}"

POD_CIDR="${POD_CIDR:-10.244.0.0/16}"
SERVICE_CIDR="${SERVICE_CIDR:-10.43.0.0/16}"
LAB_CIDR="${LAB_CIDR:-192.168.1.0/24}"
WG_SUBNET="${WG_SUBNET:-192.168.168.0/24}"
WG_SERVER_IP="${WG_SERVER_IP:-192.168.168.1}"

HELM="${HELM:-}"

log()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
  cat <<'USAGE'
umkim-thin teardown — remove umkim-thin and k3s from this host

Usage:
  sudo ./teardown.sh [options]

Options:
  -y, --yes          Do not ask for confirmation.
  --dry-run          Print the plan; make no changes (no root needed).
  --purge-data       Also remove /etc/umkim-thin, /var/lib/umkim-thin,
                     /opt/umkim-thin and Longhorn data. It also unmounts
                     recorded /srv/nfs mounts and removes their fstab entries.
                     It does NOT delete the data on those filesystems.
  --keep-tools       Keep an independently installed /usr/local/bin/helm.
                     (k3s's kubectl/crictl/ctr symlinks are removed.)
  --keep-wireguard   Keep the wg0 interface and its NAT rules.
  --keep-firewall    Keep the host firewall ports and trusted zones.
  --vip ADDRESS      Remove a stale kube-vip control-plane address from the
                     host interface. uninstall-ha.sh passes this automatically.
  -h, --help         Show this help.

The run is safe to repeat. It removes cluster state, the host storage agent,
umkim NFS export definitions, CNI/WireGuard interfaces, k3s, and the console
binary. By default it leaves host state directories and storage contents in
place so they can be inspected or reused.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    -y|--yes)        ASSUME_YES=1; shift ;;
    --dry-run)       DRY_RUN=1; shift ;;
    --purge-data)    PURGE_DATA=1; shift ;;
    --keep-tools)    KEEP_TOOLS=1; shift ;;
    --keep-wireguard) KEEP_WIREGUARD=1; shift ;;
    --keep-firewall) KEEP_FIREWALL=1; shift ;;
    --vip)
      shift
      [ "$#" -gt 0 ] || die "--vip needs an address"
      CONTROL_PLANE_VIP=$1
      shift
      ;;
    -h|--help)       usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '    +'
    printf ' %q' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

try_run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '    +'
    printf ' %q' "$@"
    printf '\n'
    return 0
  fi
  if ! "$@"; then
    warn "command failed: $*"
  fi
  return 0
}

remove_path() {
  local path="$1"
  case "$path" in
    ""|/|/etc|/var|/var/lib|/var/log|/usr|/opt|/home|/root|/srv|/run|/tmp|/etc/rancher|/var/lib/rancher)
      die "refusing to remove unsafe path: '$path'"
      ;;
  esac
  if [ ! -e "$path" ] && [ ! -L "$path" ]; then
    return 0
  fi
  try_run rm -rf -- "$path"
}

need_root() {
  if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    die "run as root (sudo ./teardown.sh; use --dry-run without root)"
  fi
}

resolve_tools() {
  if [ -z "$HELM" ]; then
    if [ -x /usr/local/bin/helm ]; then
      HELM=/usr/local/bin/helm
    else
      HELM=$(command -v helm 2>/dev/null || true)
    fi
  fi
}

confirm() {
  if [ "$DRY_RUN" -eq 1 ]; then
    return 0
  fi
  if [ "$ASSUME_YES" -eq 1 ]; then
    return 0
  fi
  [ -t 0 ] || die "stdin is not a terminal; pass --yes to confirm"

  cat <<EOF

This will remove:
  - every umkim-thin/app Helm release it can find (except k3s system releases)
  - the umkim-thin host storage agent and its NFS exports
  - k3s, cluster state, Calico/CNI networking and the console binary
  - the WireGuard interface and NAT rules created by the gateway
  - the host firewall ports added for this install

k3s local-path data (application PVCs) will be deleted by k3s uninstall.
EOF
  if [ "$PURGE_DATA" -eq 1 ]; then
    cat <<EOF
--purge-data will ALSO remove the host state/chart directories, Longhorn data,
unmount recorded /srv/nfs exports, and remove their fstab entries.
The filesystems themselves are left intact.
EOF
  else
    cat <<EOF
By default /etc/umkim-thin, /var/lib/umkim-thin, /opt/umkim-thin,
/var/lib/longhorn and /srv/nfs are kept.
EOF
  fi
  cat <<EOF

EOF

  read -r -p 'Type "teardown" to continue: ' answer || die "cancelled"
  [ "$answer" = "teardown" ] || die "cancelled"
}

uninstall_releases() {
  log "Helm releases"
  if [ -z "$HELM" ] || ! have "$HELM"; then
    info "helm not found; skipping release uninstall"
    return 0
  fi
  if [ ! -r "$KUBECONFIG" ]; then
    info "kubeconfig $KUBECONFIG not readable; skipping release uninstall"
    return 0
  fi

  local list
  if have timeout; then
    list=$(timeout 20 "$HELM" list -A --all --kubeconfig "$KUBECONFIG" 2>/dev/null) || {
      info "cluster is not reachable; skipping release uninstall"
      return 0
    }
  else
    list=$("$HELM" list -A --all --kubeconfig "$KUBECONFIG" 2>/dev/null) || {
      info "cluster is not reachable; skipping release uninstall"
      return 0
    }
  fi

  local apps=() platform=()
  local name ns
  while IFS=$'\t' read -r name ns; do
    [ -n "$name" ] || continue
    case "$ns" in
      kube-system|kube-public|kube-node-lease)
        continue
        ;;
      longhorn-system)
        warn "leaving Longhorn's Helm release for k3s removal (data kept unless --purge-data)"
        continue
        ;;
    esac
    case "$name" in
      umkim-thin|identity|wireguard)
        platform+=("$name:$ns")
        ;;
      *)
        apps+=("$name:$ns")
        ;;
    esac
  done < <(printf '%s\n' "$list" | awk 'NR > 1 { print $1 "\t" $2 }')

  local entry
  for entry in "${apps[@]}" "${platform[@]}"; do
    [ -n "$entry" ] || continue
    name=${entry%%:*}
    ns=${entry#*:}
    info "uninstall $name ($ns)"
    if have timeout; then
      if ! run timeout 120 "$HELM" uninstall "$name" --namespace "$ns" \
        --kubeconfig "$KUBECONFIG" --timeout 90s; then
        warn "could not uninstall $name; k3s removal will still discard its cluster state"
      fi
    elif ! run "$HELM" uninstall "$name" --namespace "$ns" \
      --kubeconfig "$KUBECONFIG" --timeout 90s; then
      warn "could not uninstall $name; k3s removal will still discard its cluster state"
    fi
  done
  return 0
}

stop_storage_agent() {
  log "host storage agent"
  if have systemctl; then
    local unit
    for unit in umkim-thin-storage umkim-thin; do
      if systemctl cat "$unit.service" >/dev/null 2>&1; then
        info "stop and disable $unit.service"
        try_run systemctl disable --now "$unit.service"
      fi
    done
  fi

  if [ "$DRY_RUN" -eq 0 ] && have pkill; then
    pkill -f '/usr/local/bin/umkim-thin storage-agent' 2>/dev/null || true
  fi

  remove_path /etc/systemd/system/umkim-thin-storage.service
  remove_path /etc/systemd/system/umkim-thin.service
  remove_path /run/umkim-thin
  if have systemctl; then
    try_run systemctl daemon-reload
  fi
}

cleanup_nfs_exports() {
  log "NFS exports"
  local files=(/etc/exports.d/umkim-*.exports)
  if [ -e "${files[0]}" ]; then
    info "remove ${#files[@]} umkim-thin export file(s)"
    try_run rm -f -- "${files[@]}"
  else
    info "no umkim-thin export files"
  fi

  if have exportfs; then
    try_run exportfs -ra
    if [ "$DRY_RUN" -eq 0 ] && have systemctl && systemctl cat nfs-server.service >/dev/null 2>&1; then
      if [ -z "$(exportfs -s 2>/dev/null | tr -d '[:space:]')" ]; then
        info "no NFS exports remain; stop nfs-server"
        try_run systemctl disable --now nfs-server
      else
        info "other NFS exports remain; leaving nfs-server running"
      fi
    fi
  fi
}

purge_storage_mounts() {
  [ "$PURGE_DATA" -eq 1 ] || return 0
  log "recorded NFS mounts"
  local state_file="$STATE_DIR/storage.yml"
  if [ ! -r "$state_file" ]; then
    info "no storage state at $state_file"
    return 0
  fi
  if ! have mountpoint; then
    warn "mountpoint not found; not unmounting /srv/nfs entries"
    return 0
  fi

  local mp tmp
  while IFS= read -r mp; do
    case "$mp" in
      /srv/nfs/*) ;;
      *) continue ;;
    esac
    if mountpoint -q "$mp"; then
      info "unmount $mp (data stays on disk)"
      if ! run umount "$mp"; then
        warn "could not unmount $mp; keeping its fstab entry"
        continue
      fi
    fi
    if [ -r /etc/fstab ] && awk -v mp="$mp" '$2 == mp { found=1 } END { exit !found }' /etc/fstab; then
      tmp=$(mktemp)
      awk -v mp="$mp" '$2 != mp' /etc/fstab >"$tmp"
      info "remove the fstab entry for $mp"
      if ! run cp "$tmp" /etc/fstab; then
        warn "could not update /etc/fstab for $mp"
      fi
      rm -f "$tmp"
    fi
  done < <(awk '$1 == "mountpoint:" { gsub(/"/, "", $2); print $2 }' "$state_file")
  rmdir /srv/nfs 2>/dev/null || true
}

cleanup_vip() {
  log "control-plane VIP"
  if [ -z "$CONTROL_PLANE_VIP" ]; then
    info "no VIP configured; skipping"
    return 0
  fi
  if ! have ip; then
    warn "ip not found; not checking for $CONTROL_PLANE_VIP"
    return 0
  fi

  local ifaces
  ifaces=$(ip -o -4 addr show 2>/dev/null |
    awk -v vip="$CONTROL_PLANE_VIP" '$4 == vip "/32" { sub(/:$/, "", $2); print $2 }')
  if [ -z "$ifaces" ]; then
    info "$CONTROL_PLANE_VIP is not assigned on this host"
    return 0
  fi

  local iface
  for iface in $ifaces; do
    info "remove stale VIP $CONTROL_PLANE_VIP from $iface"
    try_run ip addr del "$CONTROL_PLANE_VIP/32" dev "$iface"
  done
}

cleanup_wireguard() {
  log "WireGuard and CNI interfaces"

  if [ "$KEEP_WIREGUARD" -eq 1 ]; then
    info "keeping the WireGuard interface and NAT rules"
  else
    if have ip && ip link show wg0 >/dev/null 2>&1; then
      local port=""
      if have wg; then
        port=$(wg show wg0 listen-port 2>/dev/null || true)
      fi
      if [ "${port:-}" = "$WG_PORT" ]; then
        info "delete wg0 (listen port $WG_PORT)"
        try_run ip link delete wg0
      elif [ -z "${port:-}" ] && ip -d link show wg0 2>/dev/null | grep -q 'wireguard'; then
        # The host may not have wireguard-tools; the gateway creates wg0 from the pod.
        info "delete wg0 (WireGuard interface)"
        try_run ip link delete wg0
      else
        warn "wg0 exists but its listen port is '${port:-unknown}', not $WG_PORT; leaving it"
      fi
    fi

    if have iptables; then
      local i
      if [ "$DRY_RUN" -eq 1 ]; then
        info "would remove the WireGuard DNAT, RETURN and MASQUERADE rules"
      else
        for i in $(seq 1 8); do
          iptables -t nat -D PREROUTING -s "$WG_SUBNET" -p tcp --dport 443 \
            -j DNAT --to-destination "127.0.0.1:$TRAEFIK_VPN_PORT" 2>/dev/null || break
        done
        for i in $(seq 1 8); do
          iptables -t nat -D PREROUTING -s "$WG_SUBNET" -d "$WG_SERVER_IP" -p tcp --dport 443 \
            -j DNAT --to-destination "127.0.0.1:$TRAEFIK_VPN_PORT" 2>/dev/null || break
        done
        for i in $(seq 1 8); do
          iptables -t nat -D POSTROUTING -s "$WG_SUBNET" -d 127.0.0.1 -p tcp \
            --dport "$TRAEFIK_VPN_PORT" -j RETURN 2>/dev/null || break
        done
        for i in $(seq 1 8); do
          iptables -t nat -D POSTROUTING -s "$WG_SUBNET" ! -o wg0 \
            -j MASQUERADE 2>/dev/null || break
        done
      fi
    fi
  fi

  if have ip; then
    local iface netns
    for iface in cni0 flannel.1 vxlan.calico tunl0; do
      if ip link show "$iface" >/dev/null 2>&1; then
        info "delete interface $iface"
        try_run ip link delete "$iface"
      fi
    done
    for iface in $(ip -o link show 2>/dev/null |
      awk -F': ' '/cali/ { print $2 }' | cut -d@ -f1); do
      [ -n "$iface" ] || continue
      info "delete interface $iface"
      try_run ip link delete "$iface"
    done
    # CNI runtime dirs can outlive the containers.
    for netns in /run/netns/cni-* /var/run/netns/cni-*; do
      [ -e "$netns" ] || continue
      info "remove $netns"
      try_run rm -f "$netns"
    done
  fi
}

cleanup_firewall() {
  if [ "$KEEP_FIREWALL" -eq 1 ]; then
    log "host firewall (kept)"
    return 0
  fi
  log "host firewall"
  if ! have firewall-cmd; then
    info "firewall-cmd not found"
    return 0
  fi
  if have systemctl && ! systemctl is-active --quiet firewalld; then
    info "firewalld is not running; skipping"
    return 0
  fi

  local changed=0
  fw_remove_port() {
    local port="$1"
    if [ "$DRY_RUN" -eq 1 ]; then
      info "would remove $port"
      changed=1
      return 0
    fi
    if firewall-cmd --permanent --query-port="$port" >/dev/null 2>&1; then
      if firewall-cmd --permanent --remove-port="$port" >/dev/null 2>&1; then
        info "remove $port"
        changed=1
      else
        warn "could not remove $port"
      fi
    fi
  }
  fw_remove_source() {
    local zone="$1" src="$2"
    if [ "$DRY_RUN" -eq 1 ]; then
      info "would remove source $src from $zone"
      changed=1
      return 0
    fi
    if firewall-cmd --permanent --zone="$zone" --query-source="$src" >/dev/null 2>&1; then
      if firewall-cmd --permanent --zone="$zone" --remove-source="$src" >/dev/null 2>&1; then
        info "remove source $src from $zone"
        changed=1
      fi
    fi
  }

  fw_remove_port "$PUB_WEB/tcp"
  fw_remove_port "$PUB_TLS/tcp"
  fw_remove_port "$WEB_NODEPORT/tcp"
  fw_remove_port "$TLS_NODEPORT/tcp"
  fw_remove_port "$GIT_NODEPORT/tcp"
  fw_remove_port "$PUB_GIT/tcp"
  fw_remove_port "$TRAEFIK_VPN_PORT/tcp"
  fw_remove_port "$WG_PORT/udp"
  fw_remove_port 8472/udp
  fw_remove_port 10250/tcp

  local rule="rule family=\"ipv4\" source address=\"$LAB_CIDR\" port port=\"6443\" protocol=\"tcp\" accept"
  if [ "$DRY_RUN" -eq 1 ]; then
    info "would remove the 6443 rule for $LAB_CIDR"
    changed=1
  elif firewall-cmd --permanent --query-rich-rule="$rule" >/dev/null 2>&1; then
    if firewall-cmd --permanent --remove-rich-rule="$rule" >/dev/null 2>&1; then
      info "remove the 6443 API rule"
      changed=1
    fi
  fi

  fw_remove_source trusted "$POD_CIDR"
  fw_remove_source trusted "$SERVICE_CIDR"

  if [ "$DRY_RUN" -eq 0 ] && firewall-cmd --permanent --zone=trusted --query-service=nfs >/dev/null 2>&1; then
    if firewall-cmd --permanent --zone=trusted --remove-service=nfs >/dev/null 2>&1; then
      info "remove nfs from the trusted zone"
      changed=1
    fi
  fi

  if [ "$changed" -eq 1 ]; then
    if [ "$DRY_RUN" -eq 1 ]; then
      info "would reload firewalld"
    else
      try_run firewall-cmd --reload
    fi
  fi
}

uninstall_k3s() {
  log "k3s"
  if have systemctl; then
    local unit
    for unit in k3s k3s-agent; do
      if systemctl cat "$unit.service" >/dev/null 2>&1; then
        info "stop and disable $unit.service"
        try_run systemctl disable --now "$unit.service"
      fi
    done
  fi

  local script found=0
  for script in /usr/local/bin/k3s-uninstall.sh /usr/local/bin/k3s-agent-uninstall.sh; do
    if [ -x "$script" ]; then
      found=1
      info "run $script"
      if have timeout; then
        if ! run timeout 300 "$script"; then
          warn "$script failed; continuing with leftover cleanup"
          FAILURES=$((FAILURES + 1))
        fi
      elif ! run "$script"; then
        warn "$script failed; continuing with leftover cleanup"
        FAILURES=$((FAILURES + 1))
      fi
    fi
  done
  if [ "$found" -eq 0 ]; then
    if [ -x /usr/local/bin/k3s-killall.sh ] && [ "$DRY_RUN" -eq 0 ]; then
      info "run k3s-killall.sh"
      try_run timeout 120 /usr/local/bin/k3s-killall.sh
    fi
    info "no k3s uninstall script found; removing what is present"
  fi

  remove_path "$K3S_DATA_DIR"
  remove_path /etc/rancher/k3s
  remove_path /etc/rancher/node
  remove_path /var/lib/kubelet
  remove_path /var/lib/cni
  remove_path /etc/cni
  remove_path /opt/cni
  remove_path /run/k3s
  remove_path /run/flannel
  remove_path /run/calico
  remove_path /var/lib/calico
  remove_path /var/log/pods
  remove_path /var/log/containers
  remove_path /etc/sysctl.d/99-k3s.conf
  remove_path /usr/local/bin/k3s
  remove_path /usr/local/bin/k3s-killall.sh
  remove_path /usr/local/bin/k3s-uninstall.sh
  remove_path /usr/local/bin/k3s-agent-uninstall.sh
  remove_path /etc/systemd/system/k3s.service
  remove_path /etc/systemd/system/k3s-agent.service
  remove_path /etc/systemd/system/k3s.service.env
  remove_path /etc/systemd/system/k3s-agent.service.env
  if have systemctl; then
    try_run systemctl daemon-reload
  fi
  rmdir /etc/rancher 2>/dev/null || true
  rmdir /var/lib/rancher 2>/dev/null || true
}

remove_tools() {
  log "tools and binaries"
  remove_path /usr/local/bin/umkim-thin
  remove_path /usr/local/bin/k3s

  local tool target
  for tool in kubectl crictl ctr; do
    if [ -L "/usr/local/bin/$tool" ]; then
      target=$(readlink "/usr/local/bin/$tool" 2>/dev/null || true)
      case "$target" in
        *k3s*) remove_path "/usr/local/bin/$tool" ;;
        *) warn "leaving /usr/local/bin/$tool (not a k3s symlink)" ;;
      esac
    fi
  done

  # The k3s role also exposes kubectl through /usr/bin for sudo's secure PATH.
  if [ -L /usr/bin/kubectl ]; then
    target=$(readlink /usr/bin/kubectl 2>/dev/null || true)
    case "$target" in
      /usr/local/bin/kubectl|*k3s*) remove_path /usr/bin/kubectl ;;
      *) warn "leaving /usr/bin/kubectl (not the umkim-thin k3s shim)" ;;
    esac
  fi

  if [ "$KEEP_TOOLS" -eq 0 ]; then
    remove_path /usr/local/bin/helm
  else
    info "keeping an independently installed helm"
  fi
}

purge_state() {
  [ "$PURGE_DATA" -eq 1 ] || return 0
  log "host state and Longhorn data"
  if [ -d /var/lib/longhorn ]; then
    local remove_longhorn=1 mounted=0
    if have mountpoint && mountpoint -q /var/lib/longhorn; then
      mounted=1
    elif have findmnt && [ -n "$(findmnt -rn -R /var/lib/longhorn 2>/dev/null)" ]; then
      mounted=1
    fi
    if [ "$mounted" -eq 1 ]; then
      if ! run umount -R /var/lib/longhorn; then
        warn "could not unmount /var/lib/longhorn; not deleting it"
        remove_longhorn=0
      fi
    fi
    if [ "$remove_longhorn" -eq 1 ]; then
      remove_path /var/lib/longhorn
    fi
  fi
  remove_path "$STATE_DIR"
  remove_path "$DATA_DIR"
  remove_path "$REMOTE_DIR"
}

summary() {
  printf '\n'
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry run complete; no changes made."
    return 0
  fi
  if [ "$FAILURES" -ne 0 ]; then
    echo "Teardown finished with $FAILURES failure(s); inspect the warnings above."
  else
    echo "Teardown complete."
  fi
  if [ "$PURGE_DATA" -eq 1 ]; then
    echo "Host state, chart data, Longhorn data and /srv/nfs mount entries were removed."
    echo "The filesystems on /srv/nfs were not wiped."
  else
    echo "Kept: $STATE_DIR, $DATA_DIR, $REMOTE_DIR, /var/lib/longhorn and /srv/nfs."
  fi
  if [ "$KEEP_TOOLS" -eq 1 ]; then
    echo "Kept: an independently installed /usr/local/bin/helm."
  fi
  echo "k3s cluster state and the cluster's local-path app data were removed."
  [ "$FAILURES" -eq 0 ] || return 1
  return 0
}

need_root
resolve_tools
confirm

uninstall_releases
stop_storage_agent
uninstall_k3s
cleanup_vip
cleanup_nfs_exports
cleanup_wireguard
purge_storage_mounts
cleanup_firewall
remove_tools
purge_state
summary
