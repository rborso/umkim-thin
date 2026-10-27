#!/usr/bin/env bash
# Beginner-path VM test: install on a *disposable* Rocky Linux VM with
# install.sh (not Ansible), then assert the platform came up.
#
# This is the path a household admin takes. It is destructive: install.sh
# installs k3s and refuses a host that already has it, so run it only on a VM
# you can rebuild (or reset to a snapshot first).
#
#   TARGET=single ./ci/beginner-vm.sh --yes
#   HOST=192.0.2.10 ZONE=example.com EMAIL=you@example.com ./ci/beginner-vm.sh --yes
#
# The target must be Rocky Linux 9 or 10. The lab single target is Rocky 10.2,
# so it can run this path after a snapshot reset.
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET="${TARGET:-single}"
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
[ -f "$PROVISION_ENV" ] || { echo "missing $PROVISION_ENV" >&2; exit 2; }
# shellcheck source=ci/lib.sh
source ci/lib.sh

info() { printf '  info  %s\n' "$*"; }

HOST="${HOST:-$VHOST}"
ZONE="${ZONE:-$zone}"
EMAIL="${EMAIL:-$acme_email}"
YES=0
for a in "$@"; do
  case "$a" in
    --yes) YES=1 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done
[ "$YES" -eq 1 ] || { echo "refusing to reinstall a host without --yes" >&2; exit 2; }

# Never use production ACME in the lab: a failed lab validation still consumes the
# per-hostname rate limit. PROD_ACME=1 is for a real public host only.
ACME_FLAG="--acme-staging"
[ "${PROD_ACME:-0}" -eq 1 ] && ACME_FLAG=""

run() {
  ssh -i "$KEY" -p "$JUMP_PORT" -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
    "ssh -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${HOST} '$*'"
}

echo ">> checking $HOST is Rocky Linux and disposable"
os=$(run '. /etc/os-release; echo "$ID $VERSION_ID"')
case "$os" in
  rocky*) ok "$os" ;;
  *) echo "  $HOST is $os; install.sh supports Rocky Linux 9/10 only" >&2; exit 2 ;;
esac
if run 'test -d /etc/rancher/k3s'; then
  if [ "${ALLOW_EXISTING:-0}" -eq 1 ]; then
    echo "  $HOST already has k3s; reusing it (ALLOW_EXISTING=1)"
  else
    echo "  $HOST already has k3s; reset it to a clean snapshot first (or set ALLOW_EXISTING=1 to retry an existing install)" >&2
    exit 2
  fi
fi

echo ">> copying the tree"
tar czf - --exclude .git --exclude 'tests/playwright/node_modules' \
  --exclude 'images/servarr-bootstrap/node_modules' \
  --exclude 'images/servarr-bootstrap/dist' --exclude local . |
  ssh -i "$KEY" -p "$JUMP_PORT" -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
    "ssh -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${HOST} 'cat > /tmp/umkim-thin.tgz'"
run 'rm -rf /tmp/umkim-thin && mkdir -p /tmp/umkim-thin && tar --warning=no-timestamp -xzf /tmp/umkim-thin.tgz -C /tmp/umkim-thin'

echo ">> install.sh (this takes 10-20 minutes)"
run "cd /tmp/umkim-thin && sudo ./install.sh --zone '$ZONE' --email '$EMAIL' --skip-dns-check $ACME_FLAG ${INSTALL_FLAGS:-}"

echo ">> assertions"
run 'sudo /usr/local/bin/k3s kubectl get nodes -o wide' | sed 's/^/   /'
ready=$(run 'sudo /usr/local/bin/k3s kubectl get nodes --no-headers | grep -c " Ready "')
[ "$ready" -ge 1 ] && ok "a node is Ready" || bad "no Ready node"

for ns in identity umkim-thin; do
  if run "sudo /usr/local/bin/k3s kubectl -n $ns wait --for=condition=Available deployment --all --timeout=300s" >/dev/null 2>&1; then
    ok "$ns deployments are ready"
  else
    bad "$ns deployments are not ready"
  fi
done

for h in "auth.$ZONE" "console.$ZONE" "lldap.$ZONE"; do
  code=$(run "curl -ks -o /dev/null -w '%{http_code}' https://$h/") || code=000
  if [ "$code" = "000" ]; then
    code=$(run "curl -ks --resolve $h:443:127.0.0.1 -o /dev/null -w '%{http_code}' https://$h/") || code=000
    [ "$code" != "000" ] && info "$h answers on the node (no public hairpin)"
  fi
  case "$code" in 200|302|401|403) ok "$h -> $code" ;; *) bad "$h -> $code" ;; esac
done

# The lab router does not forward 80/443 to the VM, so point the three hosts at
# the node's own ingress for the doctor's HTTPS checks.
run "grep -q auth.$ZONE /etc/hosts || echo 127.0.0.1 auth.$ZONE console.$ZONE lldap.$ZONE | sudo tee -a /etc/hosts >/dev/null"

if run 'cd /tmp/umkim-thin && sudo ./install.sh --backup'; then ok "backup"; else bad "backup"; fi
if run 'cd /tmp/umkim-thin && sudo ./install.sh --doctor --insecure'; then ok "doctor"; else bad "doctor"; fi
if run 'cd /tmp/umkim-thin && sudo ./install.sh --zone "'"$ZONE"'" --email "'"$EMAIL"'" --render-only --out /tmp/recheck' >/dev/null; then
  ok "re-render is possible"
fi

# Leave the VM's resolver as we found it; the pin above was only for the
# doctor's HTTPS checks (the lab has no public hairpin).
run "sudo sed -i '/auth.$ZONE/d' /etc/hosts" || true

echo
if [ "$fails" -eq 0 ]; then
  echo "beginner path: OK ($ZONE)"
else
  echo "beginner path: $fails failure(s)" >&2
  exit 1
fi
