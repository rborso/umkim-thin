#!/usr/bin/env bash
# Configure the media stack through the console API and run its on-demand
# bootstrap. This is the path the console UI takes: the console probes/builds
# the image on the pinned node and enables the chart's Job.
#
#   ./ci/console-servarr.sh single|ha
#
# The stack has no tracker dependency; indexers are added in Prowlarr after the
# bootstrap completes.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/console-servarr.sh single|ha}"
export TARGET
# shellcheck source=ci/lib.sh
source ci/lib.sh

pw="$(admin_password)"
mask "$pw"
jar=$(mktemp)
trap 'rm -f "$jar"' EXIT
curl -ksc "$jar" -o /dev/null "https://$PORTAL_HOST/"
curl -ksb "$jar" -c "$jar" -H 'Content-Type: application/json' \
  --data-binary @- "https://$PORTAL_HOST/api/firstfactor" >/dev/null <<EOF
{"username":"admin","password":"$pw","targetURL":"https://$CONSOLE_HOST/"}
EOF
printf '%s' "$pw" | curl -ksb "$jar" -c "$jar" -o /dev/null \
  --data-urlencode username=admin --data-urlencode "password@-" \
  "https://$CONSOLE_HOST/login"

code=$(curl -ksb "$jar" -o /dev/null -w '%{http_code}' "https://$CONSOLE_HOST/api/status")
if [ "$code" != 200 ]; then
  echo "   console API /api/status -> $code"
  exit 1
fi

job=$(curl -ksb "$jar" -X POST "https://$CONSOLE_HOST/api/apps/servarr/setup")
id=$(echo "$job" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
if [ -z "$id" ]; then
  echo "   setup did not start: $job"
  exit 1
fi
echo "   setup job $id"

st=running
for _ in $(seq 1 200); do
  out=$(curl -ksb "$jar" "https://$CONSOLE_HOST/api/jobs/$id")
  st=$(echo "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null)
  [ "$st" = running ] || break
  tail=$(echo "$out" | python3 -c 'import json,sys; print(" | ".join(json.load(sys.stdin).get("log",[])[-1:]))' 2>/dev/null)
  [ -n "$tail" ] && echo "   $tail"
  sleep 10
done
echo "$out" | python3 -c 'import json,sys
d=json.load(sys.stdin)
print("   setup:", d.get("status"), d.get("result") or d.get("error") or "")'
[ "$st" = done ]
