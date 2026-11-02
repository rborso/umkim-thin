#!/usr/bin/env bash
# Exercise the media chain without a private tracker:
#
#   * add the public-domain "Big Buck Bunny" torrent to qBittorrent and watch it
#     download (the download client + shared media volume);
#   * add a public indexer (1337x) to Prowlarr and search for the same title
#     (the indexer -> Prowlarr path). Public trackers are best-effort: a
#     Cloudflare/outage warning does not fail the pipeline, the qBittorrent
#     add/login does.
#
#   ./ci/servarr-download.sh single
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/servarr-download.sh single|ha}"
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

# Big Buck Bunny (Blender Foundation, CC-BY) via WebTorrent's public magnet.
MAGNET='magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c&dn=Big+Buck+Bunny&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce&tr=udp%3A%2F%2Fopen.stealth.si%3A80%2Fannounce&tr=udp%3A%2F%2Ftracker.torrent.eu.org%3A451%2Fannounce&tr=udp%3A%2F%2Fexplodie.org%3A6969%2Fannounce'

remote() {
  cat <<REMOTE
set -u
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
MAGNET='$MAGNET'
fails=0
ok()  { echo "  ok    \$*"; }
bad() { echo "  FAIL  \$*"; fails=\$((fails + 1)); }
warn() { echo "  WARN  \$*"; }
sec() { \$K -n "\$1" get secret "\$2" -o jsonpath="{.data.\$3}" 2>/dev/null | base64 -d; }
ip()  { \$K -n "\$1" get svc "\$2" -o jsonpath='{.spec.clusterIP}' 2>/dev/null; }

QB=\$(ip servarr qbittorrent)
U=\$(sec servarr servarr-bootstrap QBITTORRENT_USERNAME)
P=\$(sec servarr servarr-bootstrap QBITTORRENT_PASSWORD)
[ -n "\$QB" ] && [ -n "\$U" ] && [ -n "\$P" ] || { bad "qBittorrent service/credentials"; exit 1; }

echo "== qBittorrent: login and add Big Buck Bunny =="
# 5.1 returns 200 with "Ok."; 5.3 returns 204 with an empty body.
login=\$(curl -s -o /tmp/ci-qb-login.out -w '%{http_code}' -c /tmp/ci-qb.cookies \
  --data-urlencode "username=\$U" --data-urlencode "password=\$P" "http://\$QB/api/v2/auth/login")
case "\$login" in
  200|204) ok "qBittorrent login (HTTP \$login)" ;;
  *) bad "qBittorrent login: HTTP \$login \$(cat /tmp/ci-qb-login.out 2>/dev/null)"; exit 1 ;;
esac
add=\$(curl -s -b /tmp/ci-qb.cookies \
  --data-urlencode "urls=\$MAGNET" \
  --data-urlencode "savepath=/media/downloads" \
  --data-urlencode "category=ci" \
  "http://\$QB/api/v2/torrents/add")
case "\$add" in
  *Ok*|"") ok "torrent added" ;;
  *Conflict*) ok "torrent already added" ;;
  *) bad "torrent add: \$add" ;;
esac

echo "== qBittorrent: wait for progress =="
progress=0
for i in \$(seq 1 60); do
  info=\$(curl -s -b /tmp/ci-qb.cookies "http://\$QB/api/v2/torrents/info?category=ci")
  progress=\$(printf '%s' "\$info" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print(0); raise SystemExit
print(int((d[0].get("progress",0) if d else 0)*100))' 2>/dev/null || echo 0)
  [ "\${progress:-0}" -ge 100 ] && break
  [ \$((i % 6)) -eq 0 ] && echo "   progress \${progress}%"
  sleep 10
done
if [ "\${progress:-0}" -ge 100 ]; then
  ok "download completed"
  \$K -n servarr exec deploy/qbittorrent -- sh -c 'ls -l /media/downloads | head -5' 2>/dev/null | sed 's/^/   /'
elif [ "\${progress:-0}" -gt 0 ]; then
  warn "download is progressing (\${progress}%) but did not finish in 10 minutes"
else
  warn "the public torrent did not start (tracker/seeders); the client accepted it"
fi

echo "== Prowlarr: public indexer (1337x) =="
PR=\$(ip servarr prowlarr)
PK=\$(sec servarr servarr-api-keys prowlarr-api-key)
if [ -z "\$PR" ] || [ -z "\$PK" ]; then
  warn "prowlarr not available"
else
  schema=\$(curl -s -H "X-Api-Key: \$PK" "http://\$PR/api/v1/indexer/schema")
  body=\$(printf '%s' "\$schema" | python3 -c 'import json,sys
d=json.load(sys.stdin)
s=next((x for x in d if x.get("definitionName")=="1337x"), None)
if not s:
    raise SystemExit
out={k:s.get(k) for k in ("name","definitionName","implementation","configContract","fields","tags","enable") if k in s}
out["name"]="1337x (ci)"; out["enable"]=True
print(json.dumps(out))' 2>/dev/null)
  if [ -z "\$body" ]; then
    warn "no 1337x definition in this Prowlarr build"
  else
    code=\$(curl -s -o /tmp/ci-prowlarr.out -w '%{http_code}' -H "X-Api-Key: \$PK" -H 'Content-Type: application/json' \
      -d "\$body" "http://\$PR/api/v1/indexer")
    case "\$code" in
      200|201) ok "1337x indexer added" ;;
      400|409) warn "1337x indexer already present or rejected (\$code)" ;;
      *) warn "1337x indexer add -> \$code" ;;
    esac
    id=\$(curl -s -H "X-Api-Key: \$PK" "http://\$PR/api/v1/indexer" | python3 -c 'import json,sys
d=json.load(sys.stdin)
print(next((x["id"] for x in d if "1337x" in (x.get("name") or "").lower()), ""))' 2>/dev/null)
    if [ -n "\$id" ]; then
      res=\$(curl -s -H "X-Api-Key: \$PK" "http://\$PR/api/v1/search?query=Big%20Buck%20Bunny&type=search&indexerIds=\$id&limit=10")
      n=\$(printf '%s' "\$res" | python3 -c 'import json,sys
try: print(len(json.load(sys.stdin)))
except Exception: print(0)' 2>/dev/null)
      if [ "\${n:-0}" -gt 0 ]; then ok "Prowlarr search returned \$n result(s)"; else warn "Prowlarr search returned nothing (Cloudflare/outage?)"; fi
    fi
  fi
fi
exit \$fails
REMOTE
}

out=$(remote | node_root 2>&1)
rc=$?
printf '%s\n' "$out"
if [ "$rc" -ne 0 ]; then
  echo "servarr download check failed" >&2
  exit 1
fi
echo "servarr download check passed"
