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

# The platform release version baked into the console image. It is exact: a
# commit between releases must not claim the previous tag (the console's update
# check would then offer a misleading upgrade). A git checkout is a release only
# when HEAD is on a v* tag and the tracked tree is clean; a source tarball uses
# the checked-in VERSION file; UMKIM_THIN_VERSION overrides both.
build_version() {
  if [ -n "${UMKIM_THIN_VERSION:-}" ]; then
    printf '%s' "${UMKIM_THIN_VERSION#v}"
    return
  fi
  if git rev-parse --git-dir >/dev/null 2>&1; then
    local tag
    if tag=$(git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null) &&
       [ -z "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
      printf '%s' "${tag#v}"
    else
      printf 'dev+%s' "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    fi
    return
  fi
  if [ -f VERSION ]; then
    tr -d '[:space:]' < VERSION
    return
  fi
  printf 'dev'
}

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
#
# scripts/package-charts.sh packages each chart's current version *and* the
# previous version from charts/apps git history, so an upgrade can be rolled back
# offline (docs/versioning-plan.md). Layering it over the old one-chart-per-release
# loop is what left the catalog advertising a rollback target with no tarball.
mkdir -p "$stage/ansible/artifacts/chart-mirror"
echo ">> packaging charts (current and previous version)"
if ! ./scripts/package-charts.sh "$stage/ansible/artifacts/chart-mirror"; then
  echo "error: packaging the charts failed" >&2
  exit 1
fi
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

# The in-repo platform charts (identity, wireguard-gateway, umkim-thin) are
# packaged by scripts/package-charts.sh above, so a cluster-mode console can
# upgrade them from the mirror too.

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
[ -n "${backup_nfs:-}" ] && printf 'backup_nfs: "%s"\n' "${backup_nfs}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${backup_root_nfs:-}" ] && printf 'backup_root_nfs: "%s"\n' "${backup_root_nfs}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${backup_compression:-}" ] && printf 'backup_compression: "%s"\n' "${backup_compression}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${trusted_access_enabled:-}" ] && printf 'trusted_access_enabled: %s\n' "${trusted_access_enabled}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${trusted_management_enabled:-}" ] && printf 'trusted_management_enabled: %s\n' "${trusted_management_enabled}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${trusted_management_cidrs:-}" ] && printf 'trusted_management_cidrs: "%s"\n' "${trusted_management_cidrs}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${trusted_management_exclude_cidrs:-}" ] && printf 'trusted_management_exclude_cidrs: "%s"\n' "${trusted_management_exclude_cidrs}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${external_proxy:-}" ] && printf 'external_proxy: %s\n' "$(printf '%s' "$external_proxy" | tr '[:upper:]' '[:lower:]')" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_replica_count:-}" ] && printf 'longhorn_replica_count: %s\n' "${longhorn_replica_count}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_extra_disk:-}" ] && printf 'longhorn_extra_disk: "%s"\n' "${longhorn_extra_disk}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_data_fs:-}" ] && printf 'longhorn_data_fs: "%s"\n' "${longhorn_data_fs}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_data_mount_options:-}" ] && printf 'longhorn_data_mount_options: "%s"\n' "${longhorn_data_mount_options}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_storage_over_provisioning_percentage:-}" ] && printf 'longhorn_storage_over_provisioning_percentage: %s\n' "${longhorn_storage_over_provisioning_percentage}" >> "$stage/ansible/group_vars/all/99-local.yml"
[ -n "${longhorn_storage_minimal_available_percentage:-}" ] && printf 'longhorn_storage_minimal_available_percentage: %s\n' "${longhorn_storage_minimal_available_percentage}" >> "$stage/ansible/group_vars/all/99-local.yml"
cat >> "$stage/ansible/group_vars/all/99-local.yml" <<EOF
acme_staging: ${acme_staging:-false}
lab_ca_default: ${lab_ca_default:-true}
secrets_encryption: ${secrets_encryption:-true}
gpu_enabled: ${gpu_enabled:-false}
gpu_driver_flavor: "${gpu_driver_flavor:-proprietary}"
gpu_driver_version: "${gpu_driver_version:-}"
EOF

# An AI target on a GPU host is a property of the target, not of the harness: the
# checks assert the GPU contract there and the CPU contract everywhere else.
[ -n "${ai_gpu:-}" ] && printf 'ai_gpu: %s\n' "${ai_gpu}" >> "$stage/ansible/group_vars/all/99-local.yml"

inventory="$stage/ansible/inventory.ini"
{
  echo "[server]"
  for h in ${servers:-$VHOST}; do echo "$h ansible_user=${VUSER}"; done
  echo
  echo "[primary]"
  echo "$(echo ${servers:-$VHOST} | awk '{print $1}') ansible_user=${VUSER}"
} > "$inventory"

SSH=(ssh -i "$VSSH_KEY" -p "$JUMP_PORT" -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP")

# CI_ON_JUMP=1 means this script already runs on the jumphost (the CI host):
# the Ansible work and the staging directory are local, only the targets are
# remote.
if [ "${CI_ON_JUMP:-0}" = "1" ]; then
  jump_run() { bash -c "$1"; }
  ship_stage() {
    rm -rf "$REMOTE_WORKDIR" && mkdir -p "$REMOTE_WORKDIR" && tar xzf - -C "$REMOTE_WORKDIR"
  }
  import_image() { # import_image <host>
    ssh -i "$VSSH_KEY" -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 \
      -o StrictHostKeyChecking=no -o BatchMode=yes "${VUSER}@$1" \
      'sudo /usr/local/bin/k3s ctr images import -'
  }
else
  jump_run() { "${SSH[@]}" "$1"; }
  ship_stage() {
    "${SSH[@]}" "rm -rf '${REMOTE_WORKDIR}' && mkdir -p '${REMOTE_WORKDIR}' && tar xzf - -C '${REMOTE_WORKDIR}'"
  }
  import_image() { # import_image <host>
    "${SSH[@]}" "ssh -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@$1 'sudo /usr/local/bin/k3s ctr images import -'"
  }
fi

run_playbook() {
  jump_run "cd '${REMOTE_WORKDIR}/ansible' && ansible-playbook -i inventory.ini site.yml $*"
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
  # A plain release version (exact tag) or dev+sha; see build_version above.
  version="$(build_version)"
  podman build -q --build-arg "UMKIM_THIN_GIT_SHA=${sha}" \
    --build-arg "UMKIM_THIN_VERSION=${version}" -t localhost/umkim-thin:dev . >/dev/null
  for h in ${servers:-$VHOST}; do
    echo ">> importing the image into k3s on $h"
    podman save --format docker-archive localhost/umkim-thin:dev | gzip -1 | import_image "$h" >/dev/null
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

The directory admin is admin in the portal and console; localadmin is the
console breakglass account.
EOF
}

echo ">> shipping to ${JUMP}:${REMOTE_WORKDIR}"
tar czf - -C "$stage" ansible | ship_stage

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
