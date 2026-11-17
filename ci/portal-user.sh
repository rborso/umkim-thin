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

# verify_login : prove the account can actually authenticate. Redeeming an
# invite reports success even when the name already exists (the redemption
# refuses to take over an account rather than resetting it), so a stale
# account with a different password looks "created" and then fails every app
# login for the whole run. One firstfactor call catches that here.
verify_login() {
  local pw_b64 user_b64
  pw_b64=$(printf '%s' "$password" | base64 -w0)
  user_b64=$(printf '%s' "$name" | base64 -w0)
  node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
user=\$(printf '%s' "$user_b64" | base64 -d)
pw=\$(printf '%s' "$pw_b64" | base64 -d)
body=\$(printf '{"username":"%s","password":"%s","targetURL":"https://%s/"}' "\$user" "\$pw" "$CONSOLE_HOST")
code=\$(curl -sk -o /tmp/verify.out -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "\$body" "https://$PORTAL_HOST/api/firstfactor")
echo "\$code"
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
      code=$(verify_login)
      if [ "$code" = "200" ]; then
        echo "   created $name (regular tenant, login verified)"
        exit 0
      fi
      # The invite was accepted but this account cannot log in: an earlier run
      # left the same name with a different password and redemption refused to
      # take it over. That account is a test account; remove it and retry.
      echo "   $name exists but cannot log in (HTTP ${code:-?}); deleting the stale test account" >&2
      stale_b64=$(printf '%s' "$admin_pw" | base64 -w0)
      node_root >/dev/null 2>&1 <<EOS || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
printf '%s' "$stale_b64" | base64 -d > /tmp/stale-admin
ADMIN=\$(cat /tmp/stale-admin)
/usr/local/bin/kubectl -n umkim-thin exec -i deploy/umkim-thin-console -c console -- sh -c '
  ADMIN="\$1"
  curl -s -c /tmp/c -o /dev/null -X POST -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "username=admin" --data-urlencode "password=\$ADMIN" http://127.0.0.1:8080/login
  curl -s -b /tmp/c -o /dev/null -X DELETE "http://127.0.0.1:8080/api/directory/users/$name"
' _ "\$ADMIN" < /dev/null
rm -f /tmp/stale-admin
EOS
      sleep 3
      ;;
  esac
  echo "   invite redeem attempt $attempt failed: $direct" >&2
  sleep 5
done
echo "   invite create/redeem failed after retries" >&2
exit 1
