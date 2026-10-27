#!/usr/bin/env bash
# Create (or reuse) the regular directory user the gating Playwright checks use.
#
#   UMKIM_THIN_PORTAL_USER=ci-portal-<run> \
#   UMKIM_THIN_PORTAL_PASSWORD=<random> ./ci/portal-user.sh single|ha
#
# The console/invite APIs keep using the directory admin; this account is a
# normal tenant (default-tenant) and is expected to pass Authelia and the apps'
# own logins. The e2e run generates a unique name and password per run; there is no
# fixed credential.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/portal-user.sh single|ha}"
export TARGET
# shellcheck source=ci/lib.sh
source ci/lib.sh

name="${UMKIM_THIN_PORTAL_USER:-ci-portal}"
password="${UMKIM_THIN_PORTAL_PASSWORD:?set UMKIM_THIN_PORTAL_PASSWORD to a random value}"
body_file=$(mktemp)
jar=$(mktemp)
trap 'rm -f "$body_file" "$jar"' EXIT

admin_pw="$(admin_password 2>/dev/null || true)"
if [ -z "$admin_pw" ]; then
  echo "   no directory admin password; cannot create the portal user" >&2
  exit 1
fi

# Create the invite through the console API *inside the pod*. This avoids the
# HA portal/nginx proxy entirely and uses the exact handler the working OIDC
# journeys use. The console login is the only session needed here.
admin_b64=$(printf '%s' "$admin_pw" | base64 -w0)
create_invite() {
  node_root <<EOS
set -e
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
printf '%s' "$admin_b64" | base64 -d > /tmp/admin
ADMIN="\$(cat /tmp/admin)"
/usr/local/bin/kubectl -n umkim-thin exec -i deploy/umkim-thin-console -c console -- sh -c '
  ADMIN="\$1"
  curl -s -c /tmp/c -o /dev/null -X POST -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "username=admin" --data-urlencode "password=\$ADMIN" http://127.0.0.1:8080/login
  curl -s -b /tmp/c -H "Content-Type: application/json" \
    -d "{\"displayName\":\"CI-Portal-User\",\"group\":\"default-tenant\",\"email\":\"ci-portal@ci.local\"}" http://127.0.0.1:8080/api/invites
' _ "\$ADMIN" < /dev/null
EOS
}

for attempt in 1 2 3 4 5; do
  resp=$(create_invite)
  token=$(printf '%s' "$resp" | python3 -c 'import json,sys; print((json.load(sys.stdin).get("invite") or {}).get("token",""))' 2>/dev/null || true)
  if [ -z "$token" ]; then
    echo "   invite create attempt $attempt returned no token: $resp" >&2
    sleep 5
    continue
  fi

  payload=$(python3 - "$token" "$name" "$password" <<'PY'
import json, sys
token, user, password = sys.argv[1:4]
print(json.dumps({
    "token": token,
    "username": user,
    "password": password,
    "displayName": "CI-Portal-User",
    "email": f"{user}@ci.local",
}))
PY
)
  # Redeem through the console API inside the pod. This is the same public
  # handler the portal uses, but it avoids the HA invite-signup/Traefik hop,
  # which is the unreliable part. The resulting user has a valid directory
  # password (verified by Authelia firstfactor).
  payload_b64=$(printf '%s' "$payload" | base64 -w0)
  direct=$(node_root <<EOS
set -e
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
printf '%s' "$payload_b64" | base64 -d > /tmp/redeem.json
/usr/local/bin/kubectl -n umkim-thin exec -i deploy/umkim-thin-console -c console -- \
  sh -c 'cat > /tmp/redeem.json; curl -s -o /tmp/redeem.out -w "%{http_code}" -H "Content-Type: application/json" --data-binary @/tmp/redeem.json http://127.0.0.1:8080/api/invites/redeem; echo; cat /tmp/redeem.out' < /tmp/redeem.json
EOS
)
  case "$direct" in
    2[0-9][0-9]*|*"already exists"*|*"never takes over"*)
      echo "   created $name (regular tenant)"
      exit 0
      ;;
  esac
  echo "   invite redeem attempt $attempt failed: $direct" >&2
  sleep 5
done
echo "   invite create/redeem failed after retries" >&2
exit 1
