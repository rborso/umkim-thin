#!/usr/bin/env bash
# Platform conformance checks for a provisioned target (per-app checks are the
# Playwright suites). Run on the runner, after deploy/provision.sh.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/assert.sh single|ha}"
export TARGET
# shellcheck source=ci/lib.sh
source ci/lib.sh
pw="$(admin_password)"
mask "$pw"

echo "== reachability =="
for h in "$PORTAL_HOST" "$CONSOLE_HOST"; do
  c=$(http "https://$h/")
  # A just-rolled Traefik/console can briefly refuse the first probe.
  for _ in 1 2 3 4 5; do
    case "$c" in 200|302) break ;; esac
    sleep 3
    c=$(http "https://$h/")
  done
  case "$c" in 200|302) ok "$h -> $c" ;; *) bad "$h -> $c" ;; esac
done

echo "== clock (Authelia sessions are issued against the server clock) =="
remote_now=$(node_root <<'EOS'
date -u +%s
EOS
)
local_now=$(date -u +%s)
case "$remote_now" in
  ''|*[!0-9]*) skip "could not read the cluster clock" ;;
  *)
    skew=$((local_now - remote_now)); abs=${skew#-}
    if [ "$abs" -le 60 ]; then
      ok "clock skew ${skew}s vs the cluster"
    else
      bad "clock skew ${skew}s (a client ahead of the server drops sessions)"
    fi
    ;;
esac

echo "== sudo kubectl PATH =="
if node 'sudo -n kubectl get nodes >/dev/null 2>&1'; then
  ok "sudo kubectl works without the full path"
else
  bad "sudo kubectl is not on sudo's secure PATH (/usr/bin/kubectl shim missing)"
fi

echo "== etcd secrets encryption =="
if [ "${secrets_encryption:-true}" = "false" ]; then
  skip "secrets-encryption is disabled for this target (HA key distribution pending)"
else
  enc=$(node_root <<'EOS'
/usr/local/bin/k3s secrets-encrypt status 2>/dev/null | sed -n 's/^Encryption Status: *//p'
EOS
)
  case "$enc" in
    Enabled*) ok "k3s secrets-encryption is enabled" ;;
    *)        bad "k3s secrets-encryption status: ${enc:-unknown}" ;;
  esac
fi

echo "== console acceptance =="
echo "  covered by tests/playwright/console.spec.ts (gating)"

echo "== identity secrets are Secrets =="
cm=$(node_root <<'EOS'
/usr/local/bin/kubectl -n identity get configmap authelia-config -o name 2>/dev/null || true
EOS
)
if [ -z "$cm" ]; then
  ok "Authelia config is not a ConfigMap"
else
  bad "Authelia config is still a ConfigMap: $cm"
fi
sec=$(node_root <<'EOS'
/usr/local/bin/kubectl -n identity get secret authelia-config -o name 2>/dev/null || true
EOS
)
case "$sec" in
  *authelia-config*) ok "Authelia config is a Secret" ;;
  *) bad "Authelia config Secret is missing" ;;
esac

echo "== console pod baseline =="
podsec=$(node_root <<'EOS'
/usr/local/bin/kubectl -n umkim-thin get deploy umkim-thin-console   -o jsonpath='{.spec.template.spec.securityContext.runAsNonRoot} {.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem}'
EOS
)
case "$podsec" in
  "true true") ok "console is non-root with a read-only rootfs" ;;
  *) bad "console securityContext: ${podsec:-missing}" ;;
esac
uid=$(node_root <<'EOS'
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- id -u 2>/dev/null || true
EOS
)
case "$uid" in
  65532) ok "console runs as UID 65532" ;;
  *) bad "console UID: ${uid:-unknown}" ;;
esac

# The new stdin path must not leak the directory password into the log.
logs=$(node_root <<'EOS'
/usr/local/bin/kubectl -n umkim-thin logs deploy/umkim-thin-console --tail=2000 2>/dev/null || true
EOS
)
if printf '%s' "$logs" | grep -F "$pw" >/dev/null; then
  bad "the directory password appears in the console log"
else
  ok "no directory password in the console log"
fi

echo "== console placement and state =="
expected_config_class="${storage_class_config:-}"
if [ -z "$expected_config_class" ]; then
  case "${storage_backend:-}" in
    longhorn) expected_config_class=longhorn-rwo ;;
    nfs)      expected_config_class=umkim-thin-nfs ;;
    *)        expected_config_class=local-path ;;
  esac
fi
placement=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
hn=$($K -n umkim-thin get deploy umkim-thin-console -o jsonpath='{.spec.template.spec.hostNetwork}')
sa=$($K -n umkim-thin get deploy umkim-thin-console -o jsonpath='{.spec.template.spec.serviceAccountName}')
cls=$($K -n umkim-thin get pvc umkim-thin-state -o jsonpath='{.spec.storageClassName}')
phase=$($K -n umkim-thin get pvc umkim-thin-state -o jsonpath='{.status.phase}')
printf '%s|%s|%s|%s' "${hn:-false}" "$sa" "$cls" "$phase"
EOS
)
IFS='|' read -r hostnet caservice class phase <<< "$placement"
case "$hostnet" in
  ""|false) ok "console uses the pod network" ;;
  *) bad "console hostNetwork: $hostnet" ;;
esac
case "$caservice" in
  umkim-thin-console) ok "console uses its ServiceAccount" ;;
  *) bad "console ServiceAccount: ${caservice:-missing}" ;;
esac
case "$class" in
  "$expected_config_class") ok "console state class: $class" ;;
  *) bad "console state class: ${class:-none}, expected $expected_config_class" ;;
esac
case "$phase" in
  Bound) ok "console state PVC is Bound" ;;
  *) bad "console state PVC phase: ${phase:-unknown}" ;;
esac

cfg=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- cat /etc/umkim-thin/config.env 2>/dev/null
EOS
)
for v in "UMKIM_THIN_STATE_DIR=/data" "UMKIM_THIN_KUBECONFIG=/tmp/kubeconfig" \
         "UMKIM_THIN_CHART_MIRROR=/data/chart-mirror" \
         "UMKIM_THIN_IDENTITY_APPS=/data/identity-apps.yml" \
         "UMKIM_THIN_SECRETS_FILE=/data/secrets.yml"; do
  if printf '%s\n' "$cfg" | grep -F "$v" >/dev/null; then
    ok "console config: $v"
  else
    bad "console config missing: $v"
  fi
done

statefiles=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- sh -c \
  'for f in secrets.yml identity-values.yml console-session-key; do test -f /data/$f || exit 1; done; echo ok' 2>/dev/null
EOS
)
case "$statefiles" in
  ok) ok "console PVC has the seeded state" ;;
  *) bad "console PVC is missing seeded state" ;;
esac

idclasses=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n identity get pvc -o jsonpath='{range .items[*]}{.spec.storageClassName}{"\n"}{end}' 2>/dev/null | sort -u | tr '\n' ' '
EOS
)
if [ -n "$idclasses" ]; then
  case " $idclasses " in
    *" $expected_config_class "*) ok "identity PVC class: $expected_config_class" ;;
    *) bad "identity PVC classes: $idclasses (expected $expected_config_class)" ;;
  esac
else
  skip "identity PVCs not present"
fi

traefikclass=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n kube-system get pvc traefik -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true
EOS
)
if [ -n "$traefikclass" ]; then
  case "$traefikclass" in
    "$expected_config_class") ok "Traefik PVC class: $traefikclass" ;;
    *) bad "Traefik PVC class: $traefikclass (expected $expected_config_class)" ;;
  esac
else
  skip "Traefik PVC not present"
fi

echo "== console restart keeps PVC state =="
if [ "${ASSERT_SKIP_RESTART:-0}" = "1" ]; then
  skip "console restart skipped (existing-installation test)"
else
  restart=$(node_root <<'EOS'
set -e
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
old=$($K -n umkim-thin get pod -l app=umkim-thin-console -o jsonpath='{.items[0].metadata.name}')
$K -n umkim-thin delete pod "$old" --wait=true --timeout=120s >/dev/null
$K -n umkim-thin wait --for=condition=Ready pod -l app=umkim-thin-console --timeout=180s >/dev/null
new=$($K -n umkim-thin get pod -l app=umkim-thin-console -o jsonpath='{.items[0].metadata.name}')
$K -n umkim-thin exec deploy/umkim-thin-console -- test -f /data/secrets.yml >/dev/null 2>&1 || exit 1
printf '%s|%s' "$old" "$new"
EOS
)
  IFS='|' read -r oldpod newpod <<< "$restart"
  if [ -n "$newpod" ] && [ "$oldpod" != "$newpod" ]; then
    ok "console restarted with its PVC state (${oldpod} -> ${newpod})"
  else
    bad "console restart/state check failed (${restart:-no output})"
  fi
fi

echo "== console storage socket =="
agent="${storage_agent:-}"
if [ -z "$agent" ]; then
  if [ "$TARGET" = "single" ]; then agent=true; else agent=false; fi
fi
if [ "$agent" = "true" ]; then
  active=$(node_root <<'EOS'
systemctl is-active umkim-thin-storage 2>/dev/null || true
EOS
)
  case "$active" in
    active) ok "host storage agent is active" ;;
    *) bad "host storage agent: ${active:-inactive}" ;;
  esac
  hostsock=$(node_root <<'EOS'
test -S /run/umkim-thin/storage.sock && echo yes || echo no
EOS
)
  case "$hostsock" in
    yes) ok "host storage socket exists" ;;
    *) bad "host storage socket is missing" ;;
  esac
  podsock=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  test -S /run/umkim-thin/storage.sock >/dev/null 2>&1 && echo yes || echo no
EOS
)
  case "$podsock" in
    yes) ok "console mounts the storage socket" ;;
    *) bad "console cannot see the storage socket" ;;
  esac
else
  podsock=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  test -S /run/umkim-thin/storage.sock >/dev/null 2>&1 && echo yes || echo no
EOS
)
  case "$podsock" in
    no) ok "HA has no host storage socket" ;;
    *) bad "HA unexpectedly exposes a storage socket" ;;
  esac
fi

echo "== nextcloud directory auth =="
nc=$(
node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
$K -n nextcloud get deploy nextcloud >/dev/null 2>&1 || { echo skip; exit 0; }
OCC='/usr/local/bin/php /var/www/html/occ'
$K -n nextcloud exec deploy/nextcloud -- sh -c "$OCC ldap:test-config s01" 2>&1 | tail -1
$K -n nextcloud exec deploy/nextcloud -- sh -c "$OCC ldap:search admin" 2>&1 | grep -i 'admin' | head -1
EOS
)
case "$nc" in
  *skip*) skip "nextcloud not installed" ;;
  *"configuration is valid and the connection could be established"*)
    if printf '%s' "$nc" | grep -qi 'admin'; then
      ok "nextcloud directory lookup (ldap test-config + search)"
    else
      bad "nextcloud ldap test-config passes but no directory user is found"
    fi
    ;;
  *) bad "nextcloud ldap: ${nc:-no output}" ;;
esac

echo "== tls (the portal must not serve Traefik's self-signed default) =="
issuer="$(echo | openssl s_client -connect "$PORTAL_HOST:443" -servername "$PORTAL_HOST" 2>/dev/null \
  | openssl x509 -noout -issuer 2>/dev/null)"
case "$issuer" in
  *"TRAEFIK DEFAULT CERT"*) bad "portal serves the Traefik default certificate" ;;
  "") skip "could not read the TLS certificate" ;;
  *)
    if [ "${UMKIM_THIN_STRICT_TLS:-0}" = "1" ]; then
      # full: the certificate must come from ACME (staging), not the fallback
      case "$issuer" in
        *STAGING*) ok "tls issuer (acme staging): ${issuer#issuer=}" ;;
        *"umkim-thin lab CA"*) bad "full profile served the lab CA, not an ACME certificate" ;;
        *) bad "full profile served an unexpected issuer: ${issuer#issuer=}" ;;
      esac
    else
      ok "tls issuer: ${issuer#issuer=}"
    fi
    ;;
esac

echo "== network policy (cross-namespace must be blocked) =="
out=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
ns=$($K get ns -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep -x gitea | head -1)
if [ -z "$ns" ]; then
  ns=$($K get ns -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' \
       | grep -vE '^(kube-|default|identity|longhorn-system|nfs-provisioner|wireguard|umkim-thin)$' | head -1)
fi
[ -z "$ns" ] && exit 3
gip=$($K -n "$ns" get pods --field-selector=status.phase=Running -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
[ -z "$gip" ] && exit 3
$K -n default delete pod ci-netpol-test --ignore-not-found >/dev/null 2>&1
$K -n default run ci-netpol-test --restart=Never --image=alpine:3.20 --command -- \
  sh -c "wget -q -O /dev/null -T 6 http://$gip:3000 && echo REACHABLE || echo BLOCKED" >/dev/null
$K -n default wait --for=condition=Ready pod/ci-netpol-test --timeout=90s >/dev/null 2>&1 || true
sleep 6
$K -n default logs ci-netpol-test 2>/dev/null | tr -d '\n'
$K -n default delete pod ci-netpol-test --ignore-not-found >/dev/null 2>&1
EOS
)
case "$out" in
  *BLOCKED*) ok "cross-namespace blocked" ;;
  *REACHABLE*) bad "cross-namespace reachable -> $out" ;;
  *) skip "no workload namespace yet" ;;
esac

echo "== shared media (RWX) =="
# The media volume is mounted by several apps at once and must be the same files
# everywhere: write it through one app's pod and read it back through another's.
# On HA the two pods can be on different nodes, which is exactly what the RWX
# class has to support.
rwx=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
if ! $K -n servarr get deploy sonarr >/dev/null 2>&1 || ! $K -n servarr get deploy jellyfin >/dev/null 2>&1; then
  echo "SKIP"
  exit 0
fi
marker="ci-rwx-$(date +%s)"
$K -n servarr exec deploy/sonarr -- sh -c "echo '$marker' > /media/$marker" >/dev/null 2>&1 || { echo "WRITE_FAILED"; exit 0; }
back=$($K -n servarr exec deploy/jellyfin -- cat "/media/$marker" 2>/dev/null)
sn=$($K -n servarr get pod -l app.kubernetes.io/name=sonarr -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
jn=$($K -n servarr get pod -l app.kubernetes.io/name=jellyfin -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
$K -n servarr exec deploy/sonarr -- rm -f "/media/$marker" >/dev/null 2>&1 || true
if [ "$back" = "$marker" ]; then echo "OK nodes=$sn,$jn"; else echo "MISMATCH '$back'"; fi
EOS
)
case "$rwx" in
  OK*) ok "media volume shared across pods (${rwx#OK })" ;;
  SKIP*) skip "servarr not installed" ;;
  *) bad "media volume not shared (${rwx:-no answer})" ;;
esac

echo "== wireguard tunnel =="
if [ "${ASSERT_NO_INSTALL:-0}" = "1" ]; then
  skip "wireguard: installation disabled for existing-installation test"
elif command -v wg-quick >/dev/null 2>&1 && [ -n "${wg_direct_endpoint:-}" ]; then
  # Ensure the gateway exists and is stable before adding a peer: re-installing
  # an already-present app triggers a rollout that would race the peer add.
  if ! node_root <<'EOS' >/dev/null 2>&1
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n wireguard get daemonset wireguard-gateway >/dev/null 2>&1
EOS
  then
    cli app install wireguard >/dev/null 2>&1 || true
  fi
  node_root <<'EOS' >/dev/null 2>&1
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n wireguard rollout status daemonset/wireguard-gateway --timeout=90s
EOS
  client="ci-${TARGET}-$(date +%s)"
  cfg=$(cli wg client create "$client" 2>/dev/null | sed -n '/^\[Interface\]/,$p')
  # Creating a client rewrites the peers Secret and rolls the gateway, so wait
  # for it to come back before the handshake.
  node_root <<'EOS' >/dev/null 2>&1
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n wireguard rollout status daemonset/wireguard-gateway --timeout=90s
EOS
  cip=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n identity get svc lldap -o jsonpath='{.spec.clusterIP}'
EOS
)
  # The gateway is hostNetwork, so dial the node it runs on (:51820). In HA that
  # is not necessarily the VIP.
  gwip=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n wireguard get pod -l app=wireguard-gateway -o jsonpath='{.items[0].status.podIP}'
EOS
)
  ep="${gwip:-${wg_direct_endpoint%%:*}}:${wg_port:-51820}"
  conf="/tmp/umkimciwg-${TARGET}.conf"
  printf '%s\n' "$cfg" | grep -v '^DNS =' | sed -E "s#^Endpoint = .*#Endpoint = ${ep}#" > "$conf"
  chmod 600 "$conf"
  if sudo wg-quick up "$conf" >/dev/null 2>&1; then
    # A tunnel that never handshakes is not a tunnel.
    hs=""
    for i in $(seq 1 10); do
      hs="$(sudo wg show $(basename "$conf" .conf) latest-handshakes 2>/dev/null | awk '{print $2}' | head -1)"
      [ -n "$hs" ] && [ "$hs" != "0" ] && break
      sleep 3
    done
    [ -n "$hs" ] && [ "$hs" != "0" ] \
      && ok "handshake with the gateway" \
      || bad "wireguard: no handshake with the gateway"

    # A ClusterIP (the client routes the service CIDR over the tunnel).
    code=""
    for i in $(seq 1 10); do
      code="$(curl -s -o /dev/null -m 8 -w '%{http_code}' "http://$cip:17170/health" 2>/dev/null)"
      [ "$code" = 200 ] && break
      sleep 3
    done
    [ "$code" = 200 ] && ok "reached a ClusterIP through the tunnel" \
      || bad "wireguard: ClusterIP through the tunnel -> ${code:-000}"

    # And an app route: the client also routes the pod/service CIDRs, so a
    # request to the ingress service with the app's Host header must reach the
    # router (302 to the portal is the expected answer without a session).
    traefik_cip=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n kube-system get svc traefik -o jsonpath='{.spec.clusterIP}'
EOS
)
    route=""
    for i in $(seq 1 10); do
      route="$(curl -s -o /dev/null -k -m 8 -w '%{http_code}' \
        --resolve "$CONSOLE_HOST:443:${traefik_cip}" "https://$CONSOLE_HOST/" 2>/dev/null)"
      [ "$route" = 302 ] || [ "$route" = 200 ] && break
      sleep 3
    done
    case "$route" in
      200|302) ok "reached an app route through the tunnel" ;;
      *) bad "wireguard: app route through the tunnel -> ${route:-000}" ;;
    esac

    # Revoking the peer must cut it off: that is what delete is for.
    cli wg client delete "$client" >/dev/null 2>&1 || true
    node_root <<'EOS' >/dev/null 2>&1
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n wireguard rollout status daemonset/wireguard-gateway --timeout=90s
EOS
    sleep 5
    # The gateway reloads its peers asynchronously; give the revocation a few
    # seconds to take effect instead of one timing-sensitive probe.
    revoked=200
    for i in $(seq 1 10); do
      revoked="$(curl -s -o /dev/null -m 8 -w '%{http_code}' "http://$cip:17170/health" 2>/dev/null)"
      [ "$revoked" = 200 ] || break
      sleep 3
    done
    [ "$revoked" = 200 ] \
      && bad "wireguard: a revoked peer still reached the ClusterIP" \
      || ok "a revoked peer is cut off"

    sudo wg-quick down "$conf" >/dev/null 2>&1 || true
  else
    skip "wireguard: could not bring the tunnel up (best effort)"
  fi
  rm -f "$conf"
else
  skip "no wg-quick or endpoint"
fi

echo
if [ "$fails" -eq 0 ]; then echo "PLATFORM CONFORMANCE PASSED ($TARGET)"; else echo "PLATFORM CONFORMANCE FAILED: $fails ($TARGET)"; exit 1; fi
