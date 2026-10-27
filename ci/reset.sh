#!/usr/bin/env bash
# Reset a CI target to its clean base by reverting its VMs to a snapshot.
# Preparing that base once (ci/snapshot-base.sh) is faster and more
# deterministic than uninstalling k3s/Calico/Longhorn in place.
set -euo pipefail
cd "$(dirname "$0")/.."

target="${1:?usage: ci/reset.sh single|ha}"
if [ -f ci/lab.env ]; then
  # shellcheck disable=SC1091
  . ci/lab.env
fi
case "$target" in
  single) default_vms="${CI_SINGLE_VMS:-}" ;;
  ha)     default_vms="${CI_HA_VMS:-}" ;;
  *) echo "unknown target '$target'" >&2; exit 2 ;;
esac
vms="${RESET_VMS:-$default_vms}"
if [ -z "$vms" ]; then
  echo "error: no VMs configured for '$target'; set RESET_VMS, CI_SINGLE_VMS/CI_HA_VMS, or ci/lab.env (see ci/lab.env.example)" >&2
  exit 2
fi
snap="${RESET_SNAPSHOT:-base}"
key="${HYPERVISOR_KEY:-$HOME/.ssh/id_ed25519}"

echo ">> reverting $target to snapshot '$snap'"
for spec in $vms; do
  vm="${spec%@*}"; hv="${spec#*@}"
  printf '   %-16s @ %s\n' "$vm" "$hv"
  ssh -i "$key" -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=10 "linux@$hv" \
    "sudo virsh snapshot-revert '$vm' '$snap' --running" \
    || { echo "   snapshot-revert failed for $vm" >&2; exit 1; }
done
echo ">> reset done"
