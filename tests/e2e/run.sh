#!/usr/bin/env bash
# Tier 2 (E2E) test runner, for the lab and for a workstation.
#
#   TARGET=ha tests/e2e/run.sh acceptance
#   TARGET=ha tests/e2e/run.sh journeys
#   TARGET=ha tests/e2e/run.sh platform
#   TARGET=single tests/e2e/run.sh migration
#   TARGET=ha tests/e2e/run.sh gate
#   TARGET=ha tests/e2e/run.sh all
#
# Categories:
#   acceptance  the gating Playwright specs: route/auth contract, identity
#               matrix, TLS, and the console browser flow
#   journeys    functional user journeys: deep client workflows, informational
#   platform    platform conformance: nodes, storage, policies, WireGuard, TLS
#   migration   single-node host state to PVC migration check
#   gate        acceptance + platform (+ migration on single); this is the gating path
#   all         gate + journeys (journeys are reported but do not fail the run)
#
# Environment comes from deploy/provision-<target>.env via ci/lib.sh. Set
# UMKIM_THIN_PORTAL_USER / UMKIM_THIN_PORTAL_PASSWORD for the acceptance specs
# that log in as a regular tenant.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

TARGET="${TARGET:-ha}"
export TARGET
# shellcheck disable=SC1090
source ci/lib.sh

if [ -f ci/lab.env ]; then
  # shellcheck disable=SC1091
  source ci/lab.env
fi

: "${UMKIM_THIN_ZONE:=$ZONE}"
: "${UMKIM_THIN_CONSOLE_HOST:=$CONSOLE_HOST}"
: "${UMKIM_THIN_AUTH_HOST:=$PORTAL_HOST}"
: "${UMKIM_THIN_USER:=admin}"
if [ -z "${UMKIM_THIN_PASSWORD:-}" ]; then
  UMKIM_THIN_PASSWORD="$(admin_password)"
fi

apps="${ci_apps:-gitea}"
if [ -z "${UMKIM_THIN_APPS:-}" ]; then
  ZONE="$UMKIM_THIN_ZONE" APPS="$apps" python3 - <<'PY' > tests/playwright/apps.json
import json, os
zone = os.environ["ZONE"]
manifest = json.load(open("ci/apps.json"))
entries = []
for app in os.environ["APPS"].split():
    for e in manifest.get(app, []):
        e = dict(e); e["host"] = e["host"].replace("{zone}", zone); entries.append(e)
print(json.dumps(entries))
PY
  UMKIM_THIN_APPS="$(cat tests/playwright/apps.json)"
fi
: "${UMKIM_THIN_PORTAL_USER:=${portal_user:-}}"
: "${UMKIM_THIN_PORTAL_PASSWORD:=${portal_pw:-}}"
export UMKIM_THIN_ZONE UMKIM_THIN_CONSOLE_HOST UMKIM_THIN_AUTH_HOST \
  UMKIM_THIN_USER UMKIM_THIN_PASSWORD UMKIM_THIN_APPS \
  UMKIM_THIN_PORTAL_USER UMKIM_THIN_PORTAL_PASSWORD \
  UMKIM_THIN_GITEA_SSH_PORT="${UMKIM_THIN_GITEA_SSH_PORT:-2222}"

# App credentials for the client journeys. The acceptance gate uses the
# directory admin and the portal tenant; the journeys also need each app's own
# admin/API credentials.
while IFS=$'\t' read -r name ns secret key user; do
  [ -n "$name" ] || continue
  val="$(node_root <<EOS | base64 -d 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get secret $secret -o jsonpath='{.data.$key}' 2>/dev/null || true
EOS
)"
  pre="UMKIM_THIN_$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
  export "${pre}_ADMIN_USER=$user"
  export "${pre}_ADMIN_PASSWORD=$val"
done < <(APPS="$apps" python3 - <<'PY'
import json, os
manifest = json.load(open("ci/apps.json"))
for app in os.environ["APPS"].split():
    for entry in manifest.get(app, []):
        admin = entry.get("admin")
        if admin:
            print("\t".join([entry["name"], admin["namespace"], admin["secret"],
                             admin.get("passwordKey", "admin-password"), admin["user"]]))
PY
)
while IFS=$'\t' read -r name ns secret key; do
  [ -n "$name" ] || continue
  val="$(node_root <<EOS | base64 -d 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get secret $secret -o jsonpath='{.data.$key}' 2>/dev/null || true
EOS
)"
  pre="UMKIM_THIN_$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
  export "${pre}_API_KEY=$val"
done < <(APPS="$apps" python3 - <<'PY'
import json, os
manifest = json.load(open("ci/apps.json"))
for app in os.environ["APPS"].split():
    for entry in manifest.get(app, []):
        k = entry.get("apiKey")
        if k:
            print("\t".join([entry["name"], k["namespace"], k["secret"], k["key"]]))
PY
)

run_pw() {
  (cd tests/playwright && npx playwright test "$@")
}

acceptance() {
  run_pw apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts
}

platform() {
  ./ci/assert.sh "$TARGET"
}

migration() {
  ./tests/e2e/migration/migrate-single.sh
}

component="${1:-all}"
case "$component" in
  acceptance)
    acceptance
    ;;
  routes|charts|apps)
    run_pw apps.spec.ts
    ;;
  identity|auth)
    run_pw auth.spec.ts
    ;;
  tls|traefik)
    run_pw tls.spec.ts
    ;;
  console)
    run_pw console.spec.ts
    ;;
  journeys|functional)
    run_pw journeys --workers=1
    ;;
  platform|conformance|storage|wireguard|network|cluster)
    platform
    ;;
  migration|upgrade)
    migration
    ;;
  gate)
    acceptance
    platform
    if [ "$TARGET" = "single" ]; then migration; fi
    ;;
  all)
    acceptance
    platform
    if [ "$TARGET" = "single" ]; then migration; fi
    echo "== functional user journeys (informational) =="
    if ! run_pw journeys --workers=1; then
      echo ">> functional user journeys reported failures (informational)"
    fi
    ;;
  *)
    cat >&2 <<EOS
usage: TARGET=single|ha $0 <category>

categories:
  acceptance  gating Playwright specs (routes/auth, identity, TLS, console)
  journeys    functional user journeys (deep client workflows; informational)
  platform    platform conformance (storage, identity, policy, WireGuard)
  migration   single-node host state -> console PVC migration check
  gate        acceptance + platform (+ migration on single)
  all         gate + journeys (journey failures are informational)

single checks:
  apps, auth, tls, console    one acceptance spec
EOS
    exit 2
    ;;
esac
