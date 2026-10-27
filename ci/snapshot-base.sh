#!/usr/bin/env bash
# One-time: take the "base" snapshot of a target's VMs while they are at the
# clean base (fresh VM, before any provisioning). Run this once per target after
# (re)provisioning the VMs; ci/reset.sh reverts to it afterwards.
set -euo pipefail
cd "$(dirname "$0")/.."

target="${1:?usage: ci/snapshot-base.sh single|ha}"
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

for spec in $vms; do
  vm="${spec%@*}"; hv="${spec#*@}"
  echo ">> snapshot '$snap' of $vm @ $hv"
  ssh -i "$key" -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=10 "linux@$hv" \
    "if sudo virsh snapshot-info '$vm' '$snap' >/dev/null 2>&1; then echo '   exists'; \
     else sudo virsh snapshot-create-as '$vm' '$snap' 'clean base'; fi"
done
