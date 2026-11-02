#!/usr/bin/env bash
# End-to-end WireGuard check: create a client through the console CLI, bring the
# tunnel up on the CI host, verify the handshake, split DNS and two app URLs,
# then revoke the client.
#
#   ./ci/vpn-check.sh single
#   ./ci/vpn-check.sh ha --keep
#
# The client config dials the public endpoint; the check rewrites it to the
# target's direct endpoint (the CI host is on the lab/provider network), which
# avoids relying on NAT hairpin.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/vpn-check.sh single|ha [--keep]}"
shift || true
KEEP=0
for a in "$@"; do
  case "$a" in
    --keep) KEEP=1 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

run="${UMKIM_THIN_CI_RUN:-$(date +%s)}"
name="ci-vpn-${TARGET}-${run}"
# wg-quick derives the interface name from the file name; it must be a valid
# interface name (at most 15 characters), not the run id.
iface="ciwg-${TARGET}"
conf="/tmp/${iface}.conf"
fails=0
ok()  { printf '  ok    %s\n' "$*"; }
bad() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }
skip() { printf '  skip  %s\n' "$*"; }

cleanup() {
  if [ "$KEEP" = 0 ]; then
    sudo wg-quick down "$conf" >/dev/null 2>&1 || true
    rm -f "$conf"
    cli wg client delete "$name" >/dev/null 2>&1 || true
  else
    echo "   kept $conf and client $name"
  fi
}
trap cleanup EXIT

# Defensive: the console publishes the split-DNS hosts on app install (with
# trusted access enabled). A run that reused an older state can still have an
# empty ConfigMap; fill it from the installed app list so the tunnel test is
# meaningful.
ensure_dns_hosts() {
  local hosts b64
  hosts=$(python3 - <<'PY' 2>/dev/null || true
import json
try:
    apps = json.load(open('tests/playwright/apps.json'))
except Exception:
    raise SystemExit
for a in apps:
    if a.get('host'):
        print(a['host'])
PY
)
  [ -n "$hosts" ] || return 0
  printf '%s\n' "$hosts" | awk -v ip="$server_ip" '{print $1" "ip}' > /tmp/ci-vpndns-hosts
  b64=$(base64 -w0 /tmp/ci-vpndns-hosts)
  node_root >/dev/null 2>&1 <<EOS || true
set -eu
K="sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl"
echo '$b64' | base64 -d > /tmp/ci-vpndns-hosts
\$K -n wireguard create configmap wireguard-vpn-dns --from-file=hosts=/tmp/ci-vpndns-hosts --dry-run=client -o yaml | \$K apply -f - >/dev/null
\$K -n wireguard rollout restart deploy/vpn-dns >/dev/null
\$K -n wireguard rollout status deploy/vpn-dns --timeout=120s >/dev/null || true
EOS
}

out=$(cli wg client create "$name" 2>&1) || { bad "wg client create: $out"; exit 1; }
sed -n '/^\[Interface\]/,$p' <<<"$out" | grep -v '^DNS =' >"$conf"
[ -s "$conf" ] || { bad "the console did not return a client config"; exit 1; }
endpoint="${wg_direct_endpoint:-}"
[ -n "$endpoint" ] || { bad "wg_direct_endpoint is not set in $PROVISION_ENV"; exit 1; }
sed -i "s#^Endpoint = .*#Endpoint = ${endpoint}#" "$conf"
chmod 600 "$conf"
dns_ip=$(sed -n 's/^DNS = //p' <<<"$out" | head -1)
dns_ip="${dns_ip:-${UMKIM_THIN_VPN_DNS_IP:-10.43.200.53}}"
server_ip="${wg_server_ip:-192.168.168.1}"
say() { printf '   %s\n' "$*"; }
say "endpoint $endpoint, dns $dns_ip, server $server_ip"

sudo modprobe wireguard >/dev/null 2>&1 || true
sudo ip link del "$iface" >/dev/null 2>&1 || true
if ! sudo wg-quick up "$conf" >/tmp/ci-wg-up.log 2>&1; then
  bad "wg-quick up failed: $(tail -2 /tmp/ci-wg-up.log | tr '\n' ' ')"
  exit 1
fi
ok "tunnel is up"

# The gateway reads its peers from a mounted Secret and reloads every 10s.
# Kubelet can take a while to propagate a fresh Secret to every node, so wait
# up to 3 minutes; after 60s, restart the gateway to apply the peers at once.
hs=0
for i in $(seq 1 60); do
  if sudo wg show 2>/dev/null | grep -q "latest handshake"; then hs=1; break; fi
  if [ "$i" -eq 20 ]; then
    say "no handshake yet; restarting the gateway to apply the new peer"
    node_root >/dev/null 2>&1 <<'EOS' || true
K="sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl"
$K -n wireguard rollout restart ds/wireguard-gateway >/dev/null 2>&1 || true
EOS
  fi
  sleep 3
done
[ "$hs" = 1 ] && ok "handshake" || bad "no handshake"

if ping -c 2 -W 3 "$server_ip" >/dev/null 2>&1; then ok "ping $server_ip"; else bad "ping $server_ip"; fi

hosts="immich.$ZONE gitea.$ZONE"
for h in $hosts; do
  a=$(dig +time=4 +tries=1 @"$dns_ip" "$h" A +short 2>/dev/null | tail -1)
  [ -n "$a" ] && ok "split DNS $h -> $a" || bad "split DNS $h"
done

for h in $hosts; do
  c=$(curl -sk -m 15 -o /dev/null -w '%{http_code}' --resolve "$h:443:$server_ip" "https://$h/" 2>/dev/null || echo 000)
  case "$c" in
    200|301|302|303|307|308|401|403) ok "app via tunnel $h ($c)" ;;
    *) bad "app via tunnel $h ($c)" ;;
  esac
done

if [ "$fails" -ne 0 ]; then
  echo "vpn check failed" >&2
  exit 1
fi
echo "vpn check passed"
