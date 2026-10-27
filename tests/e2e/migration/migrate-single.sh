#!/usr/bin/env bash
# Verify the single-node console state migration: the installer-owned state that
# used to live in /etc/umkim-thin on the host is present (and unchanged) on the
# console PVC at /data. Run after provision.sh on a single target; the check is
# a no-op on HA, where the console was always PVC-backed.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

TARGET="${TARGET:-single}"
export TARGET
# shellcheck source=ci/lib.sh
source ci/lib.sh

if [ "$TARGET" != "single" ]; then
  skip "state migration check is single-node only"
  exit 0
fi

# The console may have just been restarted (ci/assert does that); wait for the
# replacement pod before reading the PVC.
node_root <<'EOS' >/dev/null 2>&1 || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin wait --for=condition=Ready pod \
  -l app=umkim-thin-console --timeout=180s
EOS

# The admin/breakglass values are installer-owned. The rest of secrets.yml is
# mutable (the media-stack settings are written at runtime), so compare only
# these keys rather than the whole file.
host_secrets=$(node_root <<'EOS'
cat /etc/umkim-thin/secrets.yml 2>/dev/null || true
EOS
)
pvc_secrets=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -c console -- \
  cat /data/secrets.yml 2>/dev/null || true
EOS
)
extract_secrets() {
  printf '%s\n' "$1" | sed -n \
    -e 's/^secret_lldap_admin_password: "\(.*\)"/\1/p' \
    -e 's/^secret_console_localadmin_password: "\(.*\)"/\1/p'
}
host_keys=$(extract_secrets "$host_secrets")
pvc_keys=$(extract_secrets "$pvc_secrets")
if [ -z "$host_keys" ] && [ -z "$pvc_keys" ]; then
  skip "no single-node host seed or console PVC state to compare"
elif [ "$host_keys" = "$pvc_keys" ] && [ -n "$pvc_keys" ]; then
  ok "admin/breakglass secrets are present on the PVC"
else
  bad "host seed and PVC secrets differ"
fi

for f in console-session-key identity-values.yml; do
  host_hash=$(node_root <<EOS | awk '{print $1}'
sha256sum "/etc/umkim-thin/$f" 2>/dev/null || true
EOS
)
  pvc_hash=""
  for _ in 1 2 3 4 5 6; do
    pvc_hash=$(node_root <<EOS | awk '{print $1}'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -c console -- sha256sum "/data/$f" 2>/dev/null || true
EOS
)
    [ -n "$pvc_hash" ] && break
    sleep 5
  done
  if [ -z "$host_hash" ] && [ -z "$pvc_hash" ]; then
    skip "$f is not present in either state"
  elif [ "$host_hash" = "$pvc_hash" ] && [ -n "$pvc_hash" ]; then
    ok "$f survived the migration unchanged"
  else
    bad "$f differs between the host seed and the PVC (host=${host_hash:-missing} pvc=${pvc_hash:-missing})"
  fi
done

# Runtime state is created by the console; it only needs to exist on the PVC.
for f in identity-apps.yml invites.json wg-clients.json apps-state.json; do
  present=$(node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -c console -- \
  test -f "/data/$f" >/dev/null 2>&1 && echo yes || echo no
EOS
)
  case "$present" in
    yes) ok "/data/$f is on the console PVC" ;;
    *) skip "/data/$f has not been created yet" ;;
  esac
done

if [ "$fails" -eq 0 ]; then
  echo "STATE MIGRATION PASSED (single)"
else
  echo "STATE MIGRATION FAILED: $fails (single)" >&2
  exit 1
fi
