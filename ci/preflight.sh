#!/usr/bin/env bash
# Preflight for a target: do the names we are about to certificate actually
# point at this cluster, and does the cluster serve them?
#
#   ./ci/preflight.sh single|ha              # DNS + in-cluster (run on the runner)
#   ./ci/preflight.sh --external <zone> <host>...   # from OUTSIDE the lab
#
# Why two modes: the lab's NAT does not hairpin, so from inside the public
# address terminates on the router (OpenWrt), not on Traefik. The external mode
# must therefore run from a host outside the lab (an operator machine, a hosted
# runner, ...). ACME itself is validated from the internet, so that is the
# vantage that matters.
#
# DNS is queried through a public resolver (DNS-over-HTTPS), so the answer is
# what Let's Encrypt would see, not what /etc/hosts on the runner says.
set -uo pipefail
cd "$(dirname "$0")/.."

fails=0
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }
skip() { printf '  skip  %s\n' "$*"; }

dns_a() { # dns_a <name> -> space-separated A records from a public resolver
  curl -fsS -H 'accept: application/dns-json' \
    "https://cloudflare-dns.com/dns-query?name=$1&type=A" 2>/dev/null |
    python3 -c 'import json,sys
d=json.load(sys.stdin)
print(" ".join(sorted({a["data"] for a in d.get("Answer",[]) if a.get("type")==1})))' 2>/dev/null
}

dns_cname() {
  curl -fsS -H 'accept: application/dns-json' \
    "https://cloudflare-dns.com/dns-query?name=$1&type=CNAME" 2>/dev/null |
    python3 -c 'import json,sys
d=json.load(sys.stdin)
print(" ".join(sorted({a["data"] for a in d.get("Answer",[]) if a.get("type")==5})))' 2>/dev/null
}

# dns_check <host> [expected-ip]
dns_check() {
  local host="$1" want="${2:-}" addrs cname
  addrs="$(dns_a "$host")"
  cname="$(dns_cname "$host")"
  if [ -z "$addrs" ]; then
    bad "$host: no A record (public DNS)${cname:+ (CNAME $cname)}"
    return
  fi
  if [ -n "$want" ]; then
    if printf '%s\n' $addrs | grep -qx "$want"; then
      ok "$host -> $addrs${cname:+ (CNAME $cname)}"
    else
      bad "$host -> $addrs, expected $want${cname:+ (CNAME $cname)}"
    fi
  else
    ok "$host -> $addrs${cname:+ (CNAME $cname)}"
  fi
}

# tls_issuer <host>[:port]
tls_issuer() {
  echo | timeout 15 openssl s_client -connect "$1:443" -servername "${1%%:*}" 2>/dev/null |
    openssl x509 -noout -issuer 2>/dev/null | sed 's/^issuer=//'
}

external_mode() {
  local zone="$1"; shift
  local h
  echo "== external checks for *.${zone} (from this vantage) =="
  for h in "$@"; do
    local code server
    code="$(timeout 15 curl -s -o /dev/null -w '%{http_code}' "http://$h/.well-known/acme-challenge/preflight-$$" 2>/dev/null)"
    server="$(timeout 15 curl -sI "http://$h/.well-known/acme-challenge/preflight-$$" 2>/dev/null | awk -F': ' 'tolower($1)=="server"{print $2}' | tr -d '\r')"
    case "$code" in
      000|"") bad "$h: port 80 unreachable from here" ;;
      *)
        if printf '%s' "$server" | grep -qiE 'openwrt|uhttpd'; then
          bad "$h: port 80 answered by the router ($server) - this vantage is inside the lab; run from outside"
        else
          ok "$h: port 80 reaches the ingress (HTTP $code${server:+, server $server})"
        fi
        ;;
    esac
    local issuer
    issuer="$(tls_issuer "$h")"
    case "$issuer" in
      *OpenWrt*) bad "$h: TLS answered by the router ($issuer) - inside the lab" ;;
      "") skip "$h: no TLS answer" ;;
      *) ok "$h: TLS issuer ${issuer}" ;;
    esac
  done
  report
}

wrap_up() {
  echo
  if [ "$fails" -eq 0 ]; then echo "PREFLIGHT PASSED"; else echo "PREFLIGHT FAILED: $fails"; exit 1; fi
}
report() { wrap_up; }

if [ "${1:-}" = "--external" ]; then
  zone="${2:?usage: ci/preflight.sh --external <zone> <host>...}"
  shift 2
  [ $# -ge 1 ] || { echo "give at least one host" >&2; exit 2; }
  external_mode "$zone" "$@"
  exit $?
fi

TARGET="${1:?usage: ci/preflight.sh single|ha [--external]}"
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
[ -f "$PROVISION_ENV" ] || { echo "missing $PROVISION_ENV" >&2; exit 2; }
# shellcheck source=ci/lib.sh
source ci/lib.sh

apps="${ci_apps:-gitea}"
hosts="$PORTAL_HOST $CONSOLE_HOST"
extra=""
case " $apps " in *" nextcloud "*) extra="collabora.$ZONE" ;; esac
hosts="$hosts $(ZONE="$ZONE" APPS="$apps" python3 - <<'PY'
import json, os
zone = os.environ["ZONE"]
m = json.load(open("ci/apps.json"))
out = []
for a in os.environ["APPS"].split():
    for e in m.get(a, []):
        out.append(e["host"].replace("{zone}", zone))
print(" ".join(out))
PY
) $extra"

echo "== DNS (public resolver) =="
for h in $hosts; do
  dns_check "$h" "${public_ip:-}"
done

if [ "${CI_PROFILE:-default}" = "full" ] && [ -z "${public_ip:-}" ]; then
  bad "public_ip is not set; a full run cannot confirm the zone points here"
fi

echo "== the cluster serves these names (via the ingress address) =="
addr="${k3s_control_plane_vip:-$VHOST}"
for h in $hosts; do
  code="$(timeout 15 curl -ks -o /dev/null -w '%{http_code}' --resolve "$h:443:$addr" "https://$h/" 2>/dev/null)"
  case "$code" in
    000|"") bad "$h: no answer from $addr" ;;
    *) ok "$h: answered at $addr (HTTP $code)" ;;
  esac
done

echo
echo "note: the ACME challenge is validated from the internet, not from here."
echo "      run 'ci/preflight.sh --external $ZONE <hosts...>' from outside the lab."
wrap_up
