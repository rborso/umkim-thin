#!/usr/bin/env bash
# Ship the Ansible project to a machine that can reach the target (the lab
# jumphost), then run the playbook there.
#
#   deploy/provision.sh                       # full run
#   deploy/provision.sh --tags k3s            # a subset
#   deploy/provision.sh --list-tags
#
# Inputs come from deploy/provision.env (gitignored).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
cd "$here/.."

# These run on the machine invoking this script, not on the target or JUMP.
for tool in go helm podman ssh; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: $tool is required on the machine running deploy/provision.sh" >&2
    echo "       (the target nodes and JUMP do not run this build step)" >&2
    exit 1
  fi
done
[ -f charts/apps/gitea/Chart.yaml ] || {
  echo "charts/apps submodule is missing; run: git submodule update --init --recursive" >&2
  exit 1
}

if [ ! -f "${PROVISION_ENV:-deploy/provision.env}" ]; then
  echo "error: ${PROVISION_ENV:-deploy/provision.env} not found; copy deploy/provision.env.example" >&2
  exit 1
fi
# shellcheck disable=SC1091
source "${PROVISION_ENV:-deploy/provision.env}"

: "${JUMP:?set JUMP in deploy/provision.env}"
: "${VSSH_KEY:?set VSSH_KEY in deploy/provision.env}"
: "${VHOST:?set VHOST in deploy/provision.env}"
JUMP_PORT=${JUMP_PORT:-30200}
VUSER=${VUSER:-linux}
REMOTE_WORKDIR=${REMOTE_WORKDIR:-/home/linux/umkim-thin-deploy}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

# A chart package is a distribution artifact. Include the project license and
# notice in the local mirror packages too, not only in the OCI releases.
package_local_chart() {
  local d="$1" dest="$2"
  cp LICENSE NOTICE "$d/"
  if ! helm package "$d" -d "$dest" >/dev/null; then
    rm -f "$d/LICENSE" "$d/NOTICE"
    return 1
  fi
  rm -f "$d/LICENSE" "$d/NOTICE"
}

cp -r deploy/ansible "$stage/ansible"
mkdir -p "$stage/ansible/group_vars/all" "$stage/ansible/artifacts/charts"

echo ">> building umkim-thin (linux/amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$stage/ansible/artifacts/umkim-thin" ./cmd/umkim-thin
cp -r catalog "$stage/ansible/artifacts/catalog"
cp -r charts/identity "$stage/ansible/artifacts/charts/identity"
cp -r charts/umkim-thin "$stage/ansible/artifacts/charts/umkim-thin"

# Package the vendored app charts into the local mirror the node installs from.
# The catalog's OCI refs stay the canonical location and are rewritten to the
# mirror at install. If a chart is not vendored, `chart_repos` can clone a
# private repo over SSH (the fallback for an out-of-tree chart).
mkdir -p "$stage/ansible/artifacts/chart-mirror"
for d in charts/apps/*; do
  [ -d "$d" ] || continue
  echo ">> packaging local chart $(basename "$d")"
  package_local_chart "$d" "$stage/ansible/artifacts/chart-mirror"
done
for spec in ${chart_repos:-}; do
  if [[ "$spec" == *"://"* || "$spec" == git@* ]]; then
    url="${spec%:*}"; sub="${spec##*:}"; repo="$(basename "$url" .git)"
  else
    repo="${spec%%:*}"; sub="${spec#*:}"
    url="https://github.com/rborso/$repo.git"
  fi
  d="local/charts-src/$repo"
  if [ -d "$d/.git" ]; then
    echo ">> updating chart repo $repo"
    git -C "$d" pull -q --ff-only >/dev/null 2>&1 || true
  else
    echo ">> cloning chart repo $repo"
    mkdir -p local/charts-src
    git clone -q --depth 1 "$url" "$d"
  fi
  echo ">> packaging $repo ($sub)"
  helm package "$d/$sub" -d "$stage/ansible/artifacts/chart-mirror" >/dev/null
done

# In-repo charts (the WireGuard gateway has no private repo).
echo ">> packaging wireguard-gateway (local chart)"
package_local_chart charts/wireguard-gateway "$stage/ansible/artifacts/chart-mirror"
# The identity chart too, so a cluster-mode console can upgrade it from the mirror.
echo ">> packaging identity (local chart)"
package_local_chart charts/identity "$stage/ansible/artifacts/chart-mirror"

echo ">> rendering site variables"
cat > "$stage/ansible/group_vars/all/99-local.yml" <<EOF
zone: ${zone}
portal_host: ${portal_host}
console_host: ${console_host}
base_dn: "${base_dn}"
admin_email: ${admin_email}
acme_email: ${acme_email}
web_nodeport: ${web_nodeport}
tls_nodeport: ${tls_nodeport}
git_nodeport: ${git_nodeport:-30222}
pub_git: ${pub_git:-2222}
wg_port: ${wg_port}
pub_web: ${pub_web}
pub_tls: ${pub_tls}
pub_wg: ${pub_wg}
lab_cidr: ${lab_cidr}
k3s_channel: ${k3s_channel}
k3s_version: "${k3s_version}"
wg_endpoint: "${wg_endpoint}"
wg_subnet: ${wg_subnet}
wg_server_ip: ${wg_server_ip}
remote_dir: ${remote_dir}
state_dir: ${state_dir}
k3s_control_plane_vip: "${k3s_control_plane_vip:-}"
storage_backend: "${storage_backend:-}"
EOF
# Optional storage and console overrides (the HA target uses Longhorn RWO +
# external NFS media; the single target keeps the host storage-agent socket).
[ -n "${storage_class_config:-}" ] && printf 'storage_class_config: "%s"\n' "${storage_class_config}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${storage_class_media:-}" ] && printf 'storage_class_media: "%s"\n' "${storage_class_media}" >> "$stage/ansible/group_vars/all/99-local.yml"
# Keep environments written before the class was renamed working: those set
# storage_class_media=nfs but have no nfs_storage_class. New environments set
# the descriptive name explicitly.
if [ -z "${nfs_storage_class:-}" ] && [ "${storage_class_media:-}" = nfs ]; then
  nfs_storage_class=nfs
fi
[ -n "${nfs_storage_class:-}" ] && printf 'nfs_storage_class: "%s"\n' "${nfs_storage_class}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${nfs_mode:-}" ] && printf 'nfs_mode: "%s"\n' "${nfs_mode}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${nfs_server:-}" ] && printf 'nfs_server: "%s"\n' "${nfs_server}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${nfs_export:-}" ] && printf 'nfs_export: "%s"\n' "${nfs_export}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${traefik_node:-}" ] && printf 'traefik_node: "%s"\n' "${traefik_node}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${storage_agent:-}" ] && printf 'storage_agent: %s\n' "${storage_agent}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${storage_socket:-}" ] && printf 'storage_socket: "%s"\n' "${storage_socket}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${console_selinux_type:-}" ] && printf 'console_selinux_type: "%s"\n' "${console_selinux_type}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${console_persistence_class:-}" ] && printf 'console_persistence_class: "%s"\n' "${console_persistence_class}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${console_state_size:-}" ] && printf 'console_state_size: "%s"\n' "${console_state_size}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_replica_count:-}" ] && printf 'longhorn_replica_count: %s\n' "${longhorn_replica_count}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_storage_over_provisioning_percentage:-}" ] && printf 'longhorn_storage_over_provisioning_percentage: %s\n' "${longhorn_storage_over_provisioning_percentage}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_storage_minimal_available_percentage:-}" ] && printf 'longhorn_storage_minimal_available_percentage: %s\n' "${longhorn_storage_minimal_available_percentage}" >> "$stage/ansible/group_vars/all/99-local.yml"
cat >> "$stage/ansible/group_vars/all/99-local.yml" <<EOF
acme_staging: ${acme_staging:-false}
lab_ca_default: ${lab_ca_default:-true}
secrets_encryption: ${secrets_encryption:-true}
EOF

inventory="$stage/ansible/inventory.ini"
{
  echo "[server]"
  for h in ${servers:-$VHOST}; do echo "$h ansible_user=${VUSER}"; done
  echo
  echo "[primary]"
  echo "$(echo ${servers:-$VHOST} | awk '{print $1}') ansible_user=${VUSER}"
} > "$inventory"

SSH=(ssh -i "$VSSH_KEY" -p "$JUMP_PORT" -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP")

run_playbook() {
  "${SSH[@]}" "cd '${REMOTE_WORKDIR}/ansible' && ansible-playbook -i inventory.ini site.yml $*"
}

# Build the console image and load it into each node's k3s (no registry needed).
# Must run after k3s exists (the CNI/containerd must be up), hence the two-phase
# full run below.
build_and_import_image() {
  if ! command -v podman >/dev/null 2>&1; then
    echo ">> WARN: podman not found; skipping the console image build"
    return
  fi
  echo ">> building the console image"
  sha="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
  podman build -q --build-arg "UMKIM_THIN_GIT_SHA=${sha}" -t localhost/umkim-thin:dev . >/dev/null
  for h in ${servers:-$VHOST}; do
    echo ">> importing the image into k3s on $h"
    podman save --format docker-archive localhost/umkim-thin:dev | gzip -1 | "${SSH[@]}" \
      "ssh -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${h} 'sudo /usr/local/bin/k3s ctr images import -'" >/dev/null
  done
}

print_credentials() {
  cat <<EOF

>> console
https://${console_host:-console.${zone}}

>> credentials
On the primary (${VHOST}):
  sudo sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p' /etc/umkim-thin/secrets.yml
  sudo sed -n 's/^secret_console_localadmin_password: "\(.*\)"/\1/p' /etc/umkim-thin/secrets.yml

The directory admin is `admin` in the portal and console; `localadmin` is the
console breakglass account.
EOF
}

echo ">> shipping to ${JUMP}:${REMOTE_WORKDIR}"
tar czf - -C "$stage" ansible | "${SSH[@]}" \
  "rm -rf '${REMOTE_WORKDIR}' && mkdir -p '${REMOTE_WORKDIR}' && tar xzf - -C '${REMOTE_WORKDIR}'"

if [ "$#" -eq 0 ]; then
  # Full run: install the cluster (and everything but the console), load the
  # image, then deploy the console that needs it.
  echo ">> running ansible-playbook (cluster and platform, console deferred)"
  run_playbook --skip-tags console
  build_and_import_image
  echo ">> running ansible-playbook (console)"
  run_playbook --tags console
  print_credentials
else
  echo ">> running ansible-playbook $*"
  run_playbook "$*"
fi
