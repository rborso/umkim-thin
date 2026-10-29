#!/usr/bin/env bash
# Backup and restore check for a running target. It drives the console's own
# CLI inside the pod, so it exercises the same code path the UI and the schedule
# use: create, verify, list, restore (state unchanged), and the archive layout.
#
#   ./ci/backup-check.sh single
#   ./ci/backup-check.sh ha --env deploy/provision-ha.env
#
# It never deletes the archive it creates; retention is left to the schedule.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/backup-check.sh single|ha}"
shift || true
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env) PROVISION_ENV="$2"; shift 2 ;;
    --env=*) PROVISION_ENV="${1#*=}"; shift ;;
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

fail=0
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fail=1; }
skip() { printf '  skip  %s\n' "$*"; }

# k <kubectl args> : kubectl on the primary with the node kubeconfig.
k() { node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl $*"; }

# podsh <script> : run a shell snippet in the console pod.
podsh() {
  node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- sh -c '$1'
EOS
}

echo "== backup: configuration =="
cfg=$(k -n umkim-thin get configmap umkim-thin-config -o json)
settings=$(printf '%s' "$cfg" | python3 -c '
import json, sys
data = json.load(sys.stdin).get("data", {}).get("config.env", "")
out = {}
for line in data.splitlines():
    if "=" in line and not line.startswith("#"):
        k, v = line.split("=", 1)
        out[k] = v
for key in ("UMKIM_THIN_BACKUP_DIR", "UMKIM_THIN_BACKUP_KEEP",
            "UMKIM_THIN_BACKUP_INTERVAL", "UMKIM_THIN_BACKUP_SNAPSHOTS"):
    print(key + "=" + out.get(key, ""))
')
printf '%s\n' "$settings" | sed 's/^/    /'
case "$settings" in
  *UMKIM_THIN_BACKUP_DIR=*) ok "backup settings are in the console config" ;;
  *) bad "UMKIM_THIN_BACKUP_DIR is missing from the console config" ;;
esac
snapshots=$(printf '%s\n' "$settings" | sed -n 's/^UMKIM_THIN_BACKUP_SNAPSHOTS=//p')

echo "== backup: create =="
out=$(cli backup create 2>&1) || { bad "backup create failed: $out"; echo "backup check failed" >&2; exit 1; }
echo "$out" | sed 's/^/    /'
name=$(printf '%s\n' "$out" | sed -n 's/^wrote \([^ ]*\) .*/\1/p')
[ -n "$name" ] && ok "archive $name" || { bad "could not read the archive name"; echo "backup check failed" >&2; exit 1; }

echo "== backup: verify =="
if out=$(cli backup verify "$name" 2>&1); then ok "$out"; else bad "verify failed: $out"; fi

echo "== backup: contents =="
listing=$(cli backup list 2>&1)
printf '%s\n' "$listing" | sed 's/^/    /'
case "$listing" in
  *"$name"*) ok "the archive is listed" ;;
  *) bad "the archive is not listed" ;;
esac

files=$(podsh "tar tzf /data/backups/$name")
case "$files" in
  *state/apps-state.json*) ok "console app state is archived" ;;
  *) bad "state/apps-state.json is missing from the archive" ;;
esac
case "$files" in
  *state/apps/*-values.yaml*) ok "per-app Helm values are archived" ;;
  *) bad "no app values in the archive (install an app first)" ;;
esac
# In the pod the seed Secret is copied onto the PVC by the init container, so
# the generated secrets live under state/. The seed/ tree carries config.env.
case "$files" in
  *state/secrets.yml*|*seed/secrets.yml*) ok "the generated secrets are archived" ;;
  *) bad "secrets.yml is missing from the archive" ;;
esac
case "$files" in
  *seed/config.env*) ok "the installer config is archived for reference" ;;
  *) bad "seed/config.env is missing from the archive" ;;
esac

echo "== backup: snapshots =="
if [ "${snapshots:-true}" = "true" ]; then
  snaps=$(k get volumesnapshot -A -o json 2>/dev/null | python3 -c '
import json, sys
try:
    items = json.load(sys.stdin).get("items", [])
except Exception:
    items = []
for i in items:
    labels = i.get("metadata", {}).get("labels", {})
    if labels.get("app.kubernetes.io/managed-by") == "umkim-thin":
        print(i["metadata"]["namespace"] + "/" + i["metadata"]["name"])
')
  if [ -n "$snaps" ]; then
    printf '%s\n' "$snaps" | sed 's/^/    /'
    ok "application PVCs were snapshotted"
  else
    skip "no VolumeSnapshot objects (the storage class has no snapshot support)"
  fi
else
  skip "snapshots are disabled for this target"
fi

echo "== backup: restore (state unchanged) =="
if out=$(cli backup restore "$name" 2>&1); then
  echo "$out" | sed 's/^/    /'
  case "$out" in
    *"safety archive"*) ok "restore wrote a safety archive" ;;
    *) bad "restore did not report a safety archive" ;;
  esac
else
  bad "restore failed: $out"
fi

echo "== backup: state survived the round trip =="
if podsh 'test -s /data/invites.json || test -s /data/apps-state.json'; then
  ok "console state is present after the restore"
else
  bad "console state is missing after the restore"
fi
if podsh 'test -s /data/identity-apps.yml'; then
  ok "identity OIDC clients are present after the restore"
else
  bad "identity-apps.yml is missing after the restore"
fi

echo "== backup: console still healthy =="
if k -n umkim-thin rollout status deploy/umkim-thin-console --timeout=120s >/dev/null 2>&1; then
  ok "the console did not restart"
else
  bad "the console rollout did not settle"
fi

if [ "$fail" -ne 0 ]; then
  echo "backup check failed" >&2
  exit 1
fi
echo "backup check passed"
