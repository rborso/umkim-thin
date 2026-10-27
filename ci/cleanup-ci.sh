#!/usr/bin/env bash
# Delete the CI accounts, keys and repositories a run leaves behind.
#
#   ./ci/cleanup-ci.sh single|ha [run-id]
#
# Idempotent and best-effort: it is safe to call from ci/e2e.sh's EXIT trap,
# and it only touches names starting with `ci-`. It needs the directory admin
# password (read from the node, like the rest of the CI) and the app admin
# credentials from ci/apps.json.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/cleanup-ci.sh single|ha [run-id]}"
RUN="${2:-}"
export TARGET
# shellcheck source=ci/lib.sh
source ci/lib.sh

jar=$(mktemp)
trap 'rm -f "$jar"' EXIT

pw="$(admin_password 2>/dev/null || true)"
if [ -z "$pw" ]; then
  echo "   cleanup: no directory admin password; skipping" >&2
  exit 0
fi

# Authelia + the console's own session. The console API is also protected by
# its login, not only by the forward-auth in front of it.
curl -ksc "$jar" -o /dev/null "https://$PORTAL_HOST/" 2>/dev/null || true
curl -ksb "$jar" -c "$jar" -H 'Content-Type: application/json' \
  --data-binary "{\"username\":\"admin\",\"password\":\"$pw\",\"targetURL\":\"https://$CONSOLE_HOST/\"}" \
  "https://$PORTAL_HOST/api/firstfactor" >/dev/null 2>&1 || true
printf '%s' "$pw" | curl -ksb "$jar" -c "$jar" -o /dev/null \
  --data-urlencode username=admin --data-urlencode "password@-" \
  "https://$CONSOLE_HOST/login" 2>/dev/null || true

# The fixed names from before the random-per-run change, the current run's
# names, and any account a partial run may have created with the run suffix.
users=(ci-portal ci-immich ci-paperless ci-nextcloud)
if [ -n "$RUN" ]; then
  users+=("ci-portal-$RUN" "ci-immich-$RUN" "ci-paperless-$RUN" "ci-nextcloud-$RUN")
fi
for u in "${users[@]}"; do
  code=$(curl -ksb "$jar" -o /dev/null -w '%{http_code}' -X DELETE \
    -H "Origin: https://$CONSOLE_HOST" \
    "https://$CONSOLE_HOST/api/directory/users/$u" 2>/dev/null || echo 000)
  case "$code" in
    2*|400|404) ;;                 # gone, or the endpoint is not deployed yet
    *) echo "   cleanup: user $u -> $code" >&2 ;;
  esac
done

# Enumerate the directory so a run with an earlier/unique run id (or a failed
# trap) cannot leave ci-* accounts behind. This reads LLDAP directly; the
# console endpoint above remains the normal path.
lusers=$(node_root <<'EOS' 2>/dev/null || true
set -eu
pw=$(sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p' /etc/umkim-thin/secrets.yml)
ip=$(KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n identity get svc lldap -o jsonpath='{.spec.clusterIP}')
tok=$(curl -s -X POST "http://$ip:17170/auth/simple/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$pw\"}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
curl -s -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
  -d '{"query":"{users{id}}"}' "http://$ip:17170/api/graphql" \
  | grep -o '"id":"ci-[^"]*"' | cut -d'"' -f4 || true
EOS
)
for u in $lusers; do
  code=$(curl -ksb "$jar" -o /dev/null -w '%{http_code}' -X DELETE \
    -H "Origin: https://$CONSOLE_HOST" \
    "https://$CONSOLE_HOST/api/directory/users/$u" 2>/dev/null || echo 000)
  case "$code" in
    2*|400|404) ;;
    *) echo "   cleanup: directory user $u -> $code" >&2 ;;
  esac
done

# Gitea: the SSH test creates a key and a repository under the app admin, and
# LDAP creates ci-* accounts on first login. The API is behind forward-auth, so
# reuse the portal cookie and add the admin's Basic credentials.
gitea_host="$(ZONE="$ZONE" python3 - <<'PY'
import json, os
z = os.environ["ZONE"]
print(json.load(open("ci/apps.json"))["gitea"][0]["host"].replace("{zone}", z))
PY
)"
read -r guser gsecret gkey <<<"$(python3 - <<'PY'
import json
admin = json.load(open("ci/apps.json"))["gitea"][0]["admin"]
print(admin["user"], admin["secret"], admin.get("passwordKey", "admin-password"))
PY
)"
gpass="$(node_root <<EOS 2>/dev/null | base64 -d 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n gitea get secret "$gsecret" -o jsonpath='{.data.$gkey}' 2>/dev/null || true
EOS
)"

if [ -n "$gitea_host" ] && [ -n "$gpass" ]; then
  auth=(-u "$guser:$gpass")
  # Keys first, then repositories; both lists are the admin's.
  keys=$(curl -ksb "$jar" "${auth[@]}" "https://$gitea_host/api/v1/user/keys" 2>/dev/null || true)
  printf '%s' "$keys" | python3 -c 'import json,sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for k in data:
    if (k.get("title") or "").startswith("ci-ssh-"):
        print(k.get("id", ""))' | while read -r id; do
    [ -n "$id" ] && curl -ksb "$jar" "${auth[@]}" -o /dev/null -X DELETE \
      "https://$gitea_host/api/v1/user/keys/$id" 2>/dev/null || true
  done

  repos=$(curl -ksb "$jar" "${auth[@]}" "https://$gitea_host/api/v1/user/repos?limit=50" 2>/dev/null || true)
  printf '%s' "$repos" | python3 -c 'import json,sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in data:
    if (r.get("name") or "").startswith("ci-ssh-"):
        print(r.get("full_name", ""))' | while read -r full; do
    [ -n "$full" ] && curl -ksb "$jar" "${auth[@]}" -o /dev/null -X DELETE \
      "https://$gitea_host/api/v1/repos/$full" 2>/dev/null || true
  done

  # LDAP-created ci-* Gitea accounts (the directory accounts are removed above).
  gusers=$(curl -ksb "$jar" "${auth[@]}" "https://$gitea_host/api/v1/admin/users?limit=50" 2>/dev/null || true)
  printf '%s' "$gusers" | python3 -c 'import json,sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for u in data:
    if (u.get("login") or "").startswith("ci-"):
        print(u["login"])' | while read -r u; do
    [ -n "$u" ] && curl -ksb "$jar" "${auth[@]}" -o /dev/null -X DELETE \
      "https://$gitea_host/api/v1/admin/users/$u" 2>/dev/null || true
  done
fi

# Test scratch data (keys, clones, journey reports) for this target only: the
# single and HA e2e runs use the same runner in parallel.
rm -rf "/tmp/umkim-git-${TARGET}-"* "/tmp/umkim-journeys-${TARGET}"* 2>/dev/null || true
echo "   cleanup: ci-* users, keys and repositories removed" >&2
