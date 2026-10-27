#!/usr/bin/env bash
# Print the deployed credentials for a target. Masked by default; --reveal
# prints the values. It never writes a file: read it from the terminal (or copy
# what you need) and close it.
#
#   ./ci/credentials.sh single|ha [--reveal]
#
# Reads the console's secrets file and the media stack's Secret from the primary
# node, so it works for both host and cluster console modes.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/credentials.sh single|ha [--reveal]}"
export TARGET
REVEAL=0
[ "${2:-}" = "--reveal" ] && REVEAL=1
# shellcheck source=ci/lib.sh
source ci/lib.sh

show() {
  if [ "$REVEAL" = 1 ]; then
    printf '%s' "$1"
  else
    printf '********'
  fi
}

echo "== endpoints =="
echo "  console     https://$CONSOLE_HOST"
echo "  portal      https://$PORTAL_HOST"
echo "  sign in     admin (directory) or localadmin (breakglass)"

echo "== console/directory secrets =="
secrets=$(node_root <<'EOS'
sed -n 's/^\(secret_[a-z_]*\): *"\(.*\)"$/\1=\2/p' /etc/umkim-thin/secrets.yml 2>/dev/null
EOS
)
while IFS='=' read -r key value; do
  [ -n "$key" ] || continue
  printf '  %-32s %s\n' "$key" "$(show "$value")"
done <<<"$secrets"

echo "== media stack (servarr) =="
bootstrap=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n servarr get secret servarr-bootstrap -o json 2>/dev/null
EOS
)
printf '%s' "$bootstrap" | python3 -c 'import base64, json, sys
try:
    data = json.load(sys.stdin).get("data", {})
except Exception:
    sys.exit(0)
for key in sorted(data):
    print(key + "\t" + base64.b64decode(data[key]).decode())' |
while IFS=$'\t' read -r key value; do
  [ -n "$key" ] || continue
  case "$key" in
    *USERNAME*|*_USER) printf '  %-32s %s\n' "$key" "$value" ;;
    *)                 printf '  %-32s %s\n' "$key" "$(show "$value")" ;;
  esac
done

if [ "$REVEAL" != 1 ]; then
  echo
  echo "Re-run with --reveal to print the values."
fi
