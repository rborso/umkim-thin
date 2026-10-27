#!/usr/bin/env bash
# Validate public DNS and TLS for the ACME hosts. Read-only: it never asks
# Let's Encrypt for anything, so it cannot consume the failed-authorization or
# certificate rate limits.
#
#   ./ci/acme-check.sh example.com
#   EXPECTED_IP=203.0.113.10 ./ci/acme-check.sh example.com auth console lldap nextcloud
#
# Run it before switching an install to production ACME and after the switch.
# A host that is still on Traefik's self-signed default fails the issuer check.
set -uo pipefail
cd "$(dirname "$0")/.."

ZONE="${1:?usage: acme-check.sh <zone> [host ...]}"
shift || true
HOSTS=("$@")
[ "${#HOSTS[@]}" -gt 0 ] && HOSTS=("${HOSTS[@]}") || HOSTS=(auth console lldap)

fails=0
ok() { printf '  ok    %s\n' "$*"; }
bad() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }

dns_a() {
  curl -fsS -H 'accept: application/dns-json' \
    "https://cloudflare-dns.com/dns-query?name=$1&type=A" 2>/dev/null |
    python3 -c 'import json,sys
d=json.load(sys.stdin)
print(" ".join(sorted({a["data"] for a in d.get("Answer",[]) if a.get("type")==1})))' 2>/dev/null
}

echo "== DNS and TLS for *.$ZONE =="
for h in "${HOSTS[@]}"; do
  case "$h" in
    *.*) fqdn="$h" ;;
    *) fqdn="$h.$ZONE" ;;
  esac

  addrs="$(dns_a "$fqdn")"
  if [ -z "$addrs" ]; then
    bad "$fqdn: no A record"
    continue
  fi
  if [ -n "${EXPECTED_IP:-}" ]; then
    if printf '%s\n' $addrs | grep -qx "$EXPECTED_IP"; then
      ok "$fqdn -> $addrs"
    else
      bad "$fqdn -> $addrs (expected $EXPECTED_IP)"
    fi
  else
    ok "$fqdn -> $addrs"
  fi

  code="$(curl -sS -o /dev/null -m 15 -w '%{http_code}' "https://$fqdn/" 2>/dev/null)" || code=000
  case "$code" in
    200|301|302|303|401|403) ok "$fqdn https -> $code" ;;
    000) bad "$fqdn https: connection/TLS failed" ;;
    *) bad "$fqdn https -> $code" ;;
  esac

  cert="$(printf '' | timeout 20 openssl s_client -connect "$fqdn:443" \
    -servername "$fqdn" -verify_return_error 2>/dev/null || true)"
  if [ -z "$cert" ]; then
    bad "$fqdn: no TLS certificate"
  else
    issuer="$(printf '%s\n' "$cert" | openssl x509 -noout -issuer 2>/dev/null || true)"
    subject="$(printf '%s\n' "$cert" | openssl x509 -noout -subject 2>/dev/null || true)"
    case "$issuer" in
      *"Let's Encrypt"*) ok "$fqdn issuer: $issuer" ;;
      "") bad "$fqdn: could not read the certificate issuer" ;;
      *) bad "$fqdn is not on Let's Encrypt: $issuer ($subject)" ;;
    esac
  fi
done

echo
if [ "$fails" -eq 0 ]; then
  echo "ACME check passed ($ZONE)."
  exit 0
fi
echo "ACME check: $fails failure(s). Do not switch to production ACME until DNS/TLS are clean." >&2
exit 1
