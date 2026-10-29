#!/usr/bin/env bash
# Upgrade check for a running target: re-run the documented platform upgrade
# (the same installer with --reuse-k3s), then prove the platform, the console,
# the identity stack and the installed apps survived it — and that the new
# console settings (backups, update check) landed.
#
#   ./ci/upgrade-check.sh single
#   ./ci/upgrade-check.sh ha --env deploy/provision-ha.env
#   ./ci/upgrade-check.sh single --no-provision   # assertions only
#
# The app-chart N-1 path is exercised when the chart mirror still carries the
# previous catalogued tarball; otherwise it is reported as skipped (an offline
# rollback needs that tarball, which is why the mirror keeps it).
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/upgrade-check.sh single|ha}"
shift || true
PROVISION=1
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env) PROVISION_ENV="$2"; shift 2 ;;
    --env=*) PROVISION_ENV="${1#*=}"; shift ;;
    --no-provision) PROVISION=0; shift ;;
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

k() { node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl $*"; }

revision() {
  k -n umkim-thin get deploy umkim-thin-console -o json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("")
    raise SystemExit
print(d.get("metadata", {}).get("annotations", {}).get("deployment.kubernetes.io/revision", ""))
'
}

echo "== upgrade: before =="
before=$(revision)
ok "console deployment revision ${before:-unknown}"
pods_before=$(k -n umkim-thin get pods -o json | python3 -c '
import json, sys
items = json.load(sys.stdin).get("items", [])
print(" ".join(i["metadata"]["name"] for i in items if i["metadata"]["name"].startswith("umkim-thin-console")))
')
apps_before=$(k get deploy,statefulset -A -l app.kubernetes.io/managed-by=Helm -o json 2>/dev/null | python3 -c '
import json, sys
try:
    items = json.load(sys.stdin).get("items", [])
except Exception:
    items = []
print(len(items))
')

if [ "$PROVISION" -eq 1 ]; then
  echo "== upgrade: run the installer again (platform upgrade) =="
  if PROVISION_ENV="$PROVISION_ENV" ./deploy/provision.sh > /tmp/umkim-upgrade-"$TARGET".log 2>&1; then
    ok "the installer completed (log: /tmp/umkim-upgrade-$TARGET.log)"
  else
    bad "the installer failed; see /tmp/umkim-upgrade-$TARGET.log"
    tail -20 /tmp/umkim-upgrade-"$TARGET".log | sed 's/^/    /'
  fi
else
  skip "provisioning (--no-provision)"
fi

echo "== upgrade: console rolled out =="
if k -n umkim-thin rollout status deploy/umkim-thin-console --timeout=300s >/dev/null 2>&1; then
  ok "the console is Ready"
else
  bad "the console did not become Ready"
fi
after=$(revision)
if [ "$PROVISION" -eq 0 ]; then
  skip "revision comparison (--no-provision)"
elif [ "$before" = "$after" ] && [ -n "$before" ]; then
  bad "the console deployment revision did not change ($before) — the upgrade did not apply"
else
  ok "console deployment revision ${after:-unknown}"
fi
pods_after=$(k -n umkim-thin get pods -o json | python3 -c '
import json, sys
items = json.load(sys.stdin).get("items", [])
print(" ".join(i["metadata"]["name"] for i in items if i["metadata"]["name"].startswith("umkim-thin-console")))
')
if [ "$PROVISION" -eq 0 ]; then
  skip "pod replacement (--no-provision)"
elif [ "$pods_before" = "$pods_after" ] && [ -n "$pods_before" ]; then
  bad "the console pod was not replaced ($pods_before)"
else
  ok "the console pod was replaced"
fi

echo "== upgrade: state survived =="
if node_root <<'EOS' >/dev/null 2>&1
set -e
kubectl -n umkim-thin exec deploy/umkim-thin-console -- test -s /data/secrets.yml
kubectl -n umkim-thin exec deploy/umkim-thin-console -- test -s /data/identity-apps.yml
EOS
then
  ok "secrets and OIDC clients are on the console PVC"
else
  bad "console state is missing after the upgrade"
fi

echo "== upgrade: new settings landed =="
cfg=$(k -n umkim-thin get configmap umkim-thin-config -o json)
for key in UMKIM_THIN_BACKUP_DIR UMKIM_THIN_BACKUP_INTERVAL UMKIM_THIN_UPDATE_CHECK; do
  if printf '%s' "$cfg" | grep -q "$key="; then
    ok "$key is in the console config"
  else
    bad "$key is missing from the console config"
  fi
done

echo "== upgrade: installed apps still healthy =="
apps_after=$(k get deploy,statefulset -A -l app.kubernetes.io/managed-by=Helm -o json 2>/dev/null | python3 -c '
import json, sys
try:
    items = json.load(sys.stdin).get("items", [])
except Exception:
    items = []
print(len(items))
')
if [ "${apps_after:-0}" -ge "${apps_before:-0}" ]; then
  ok "$apps_after Helm workloads (was $apps_before)"
else
  bad "workloads disappeared during the upgrade: $apps_before -> $apps_after"
fi
notready=$(k get deploy -A -o json | python3 -c '
import json, sys
items = json.load(sys.stdin).get("items", [])
bad = []
for i in items:
    ns = i["metadata"]["namespace"]
    if ns in ("kube-system", "longhorn-system"):
        continue
    want = i.get("spec", {}).get("replicas", 0) or 0
    have = i.get("status", {}).get("readyReplicas", 0) or 0
    if want != have:
        bad.append(ns + "/" + i["metadata"]["name"])
print(" ".join(bad))
')
if [ -z "$notready" ]; then
  ok "every application Deployment is ready"
else
  bad "not ready after the upgrade: $notready"
fi

echo "== upgrade: app chart N-1 (offline) =="
app=$(cli app list 2>/dev/null | awk 'NF {print $1; exit}')
if [ -z "$app" ]; then
  skip "no catalog app is installed"
else
  installed=$(cli app versions "$app" 2>/dev/null | awk '$1 == "installed" {print $2}')
  mirror=$(node_root <<EOS
kubectl -n umkim-thin exec deploy/umkim-thin-console -- ls /data/chart-mirror
EOS
)
  # The candidate is the newest catalogued version that is not the installed one
  # and whose tarball is in the mirror: an offline upgrade needs the tarball.
  candidate=$(printf '%s\n' "$mirror" | sed -n "s/^${app}-\(.*\)\.tgz$/\1/p" | sort -V | while read -r v; do
    [ "$v" = "$installed" ] && continue
    cli app versions "$app" 2>/dev/null | grep -q " $v " && echo "$v"
  done | tail -1)
  if [ -z "$installed" ]; then
    skip "could not read the installed $app version"
  elif [ -z "$candidate" ]; then
    skip "the mirror has no second $app tarball (only $installed)"
  else
    if cli app upgrade "$app" --to "$candidate" --yes >/tmp/umkim-n-1-"$TARGET".log 2>&1; then
      ok "$app upgraded to the catalogued chart $candidate"
      if cli app upgrade "$app" --to "$installed" --yes >>/tmp/umkim-n-1-"$TARGET".log 2>&1; then
        ok "$app upgraded back to $installed"
      else
        bad "$app could not return to $installed; see /tmp/umkim-n-1-$TARGET.log"
      fi
    else
      bad "$app N-1 upgrade to $candidate failed; see /tmp/umkim-n-1-$TARGET.log"
    fi
  fi
fi

if [ "$fail" -ne 0 ]; then
  echo "upgrade check failed" >&2
  exit 1
fi
echo "upgrade check passed"
