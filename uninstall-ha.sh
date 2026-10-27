#!/usr/bin/env bash
# Uninstall a three-node HA site from the machine that has the checkout and
# SSH access to JUMP. It reads the same environment file used for provisioning
# and runs teardown.sh on every server, non-primary first and the primary last.
#
#   ./uninstall-ha.sh --dry-run
#   ./uninstall-ha.sh
#   ./uninstall-ha.sh --purge-data --yes
set -euo pipefail

cd "$(dirname "$0")"

ENV_FILE="${HA_ENV_FILE:-deploy/provision-ha.env}"
ASSUME_YES=0
DRY_RUN=0
PURGE_DATA=0
KEEP_TOOLS=0
KEEP_WIREGUARD=0
KEEP_FIREWALL=0
FAILURES=0

usage() {
  cat <<'EOF'
Uninstall an umkim-thin three-node HA site

Usage:
  ./uninstall-ha.sh [options]

Options:
  --env FILE         Environment file (default: deploy/provision-ha.env).
  -y, --yes          Do not ask for confirmation.
  --dry-run          Show the plan on every server; make no changes.
  --purge-data       Also remove host state, chart data and Longhorn data.
                     Filesystems under /srv/nfs are not wiped.
  --keep-tools       Keep an independently installed helm.
  --keep-firewall    Keep host firewall ports and trusted zones.
  --keep-wireguard   Keep wg0 and its NAT rules.
  -h, --help         Show this help.

The script reads JUMP, VSSH_KEY, VUSER, VHOST and servers from FILE. It runs
the repository teardown.sh on each server; the primary is always last so the
shared Helm releases are removed while the control plane is still available.
The k3s_control_plane_vip address, when set, is also removed from each host.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --env)
      shift
      [ "$#" -gt 0 ] || { echo "error: --env needs a path" >&2; exit 2; }
      ENV_FILE=$1
      ;;
    -y|--yes)          ASSUME_YES=1 ;;
    --dry-run)         DRY_RUN=1 ;;
    --purge-data)      PURGE_DATA=1 ;;
    --keep-tools)      KEEP_TOOLS=1 ;;
    --keep-wireguard)  KEEP_WIREGUARD=1 ;;
    --keep-firewall)   KEEP_FIREWALL=1 ;;
    -h|--help)         usage; exit 0 ;;
    *) echo "error: unknown option: $1 (try --help)" >&2; exit 2 ;;
  esac
  shift
done

[ -f "$ENV_FILE" ] || { echo "error: environment file not found: $ENV_FILE" >&2; exit 1; }
[ -x ./teardown.sh ] || { echo "error: ./teardown.sh is missing" >&2; exit 1; }

# shellcheck disable=SC1090
source "$ENV_FILE"

: "${JUMP:?set JUMP in $ENV_FILE}"
: "${VSSH_KEY:?set VSSH_KEY in $ENV_FILE}"
: "${VHOST:?set VHOST in $ENV_FILE}"
VUSER=${VUSER:-linux}
JUMP_PORT=${JUMP_PORT:-22}
[ -f "$VSSH_KEY" ] || { echo "error: SSH key not found: $VSSH_KEY" >&2; exit 1; }

read -r -a nodes <<<"${servers:-$VHOST}"
if [ "${#nodes[@]}" -eq 0 ]; then
  echo "error: servers is empty in $ENV_FILE" >&2
  exit 1
fi
if [ "${nodes[0]}" != "$VHOST" ]; then
  echo "error: VHOST must be the first entry in servers" >&2
  exit 1
fi

order=()
for node in "${nodes[@]}"; do
  [ "$node" = "$VHOST" ] || order+=("$node")
done
order+=("$VHOST")

remote_args=(--yes)
[ "$DRY_RUN" -eq 1 ] && remote_args+=(--dry-run)
[ "$PURGE_DATA" -eq 1 ] && remote_args+=(--purge-data)
[ "$KEEP_TOOLS" -eq 1 ] && remote_args+=(--keep-tools)
[ "$KEEP_FIREWALL" -eq 1 ] && remote_args+=(--keep-firewall)
[ "$KEEP_WIREGUARD" -eq 1 ] && remote_args+=(--keep-wireguard)
[ -n "${k3s_control_plane_vip:-}" ] && remote_args+=(--vip "$k3s_control_plane_vip")

echo "Uninstall HA site from $ENV_FILE"
echo "  primary: $VHOST (last)"
echo "  order:   ${order[*]}"
if [ "$DRY_RUN" -eq 1 ]; then
  echo "  mode:    dry run (no changes)"
elif [ "$PURGE_DATA" -eq 1 ]; then
  echo "  mode:    full teardown, including host state and Longhorn data"
else
  echo "  mode:    full teardown; host state and Longhorn data are kept"
fi
echo

if [ "$DRY_RUN" -eq 0 ] && [ "$ASSUME_YES" -eq 0 ]; then
  [ -t 0 ] || { echo "error: stdin is not a terminal; pass --yes" >&2; exit 1; }
  cat <<EOF
This removes k3s and all cluster state on every server, including local-path
application data. By default /etc/umkim-thin, /var/lib/umkim-thin,
/opt/umkim-thin, /var/lib/longhorn and /srv/nfs are kept.
EOF
  if [ "$PURGE_DATA" -eq 1 ]; then
    echo "--purge-data also removes the host state/chart/Longhorn data and"
    echo "unmounts recorded /srv/nfs exports; it does not wipe those filesystems."
  fi
  echo
  read -r -p 'Type "uninstall" to continue: ' answer || exit 1
  [ "$answer" = "uninstall" ] || { echo "cancelled"; exit 1; }
fi

ssh_opts=(
  -i "$VSSH_KEY"
  -p "$JUMP_PORT"
  -o UserKnownHostsFile=/dev/null
  -o ConnectTimeout=15
  -o StrictHostKeyChecking=no
  -o BatchMode=yes
)

run_node() {
  local node=$1
  local runner="bash -s --"
  [ "$DRY_RUN" -eq 0 ] && runner="sudo -n bash -s --"
  local args="${remote_args[*]}"
  local inner="ssh -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${node} '${runner} ${args}'"

  echo
  echo "==> $node"
  if ssh "${ssh_opts[@]}" "$JUMP" "$inner" <./teardown.sh; then
    echo "==> $node done"
  else
    echo "WARN: teardown failed on $node" >&2
    FAILURES=$((FAILURES + 1))
  fi
}

for node in "${order[@]}"; do
  run_node "$node"
done

echo
if [ "$FAILURES" -ne 0 ]; then
  echo "Uninstall finished with $FAILURES failed host(s); inspect the warnings above." >&2
  exit 1
fi
if [ "$DRY_RUN" -eq 1 ]; then
  echo "Dry run complete; no changes made."
else
  echo "HA uninstall complete."
  echo "Remove the VIP/DNS records and close ports 80/443 and 51820/UDP separately."
fi
