#!/usr/bin/env bash
# Shared helpers for the lab scripts. Set TARGET (single|ha) first, then source.
# Provides: node, nodes, node_root, http, ok/bad/skip and the target's env.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

: "${TARGET:?set TARGET=single|ha}"
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck disable=SC1090
source "$PROVISION_ENV"
JUMP_PORT="${JUMP_PORT:-30200}"
KEY="${VSSH_KEY:?set VSSH_KEY}"
# All nodes of the target (single node, or the whole HA control plane).
SERVERS="${servers:-${VHOST}}"
export SERVERS

# The env files use lowercase site vars; expose the uppercase names the lab
# scripts use.
ZONE="${ZONE:-${zone:-}}"
PORTAL_HOST="${PORTAL_HOST:-${portal_host:-}}"
CONSOLE_HOST="${CONSOLE_HOST:-${console_host:-}}"
export ZONE PORTAL_HOST CONSOLE_HOST

fails=0
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }
skip() { printf '  skip  %s\n' "$*"; }

# node '<command>' : run a command on the primary node as the login user
node() {
  ssh -i "$KEY" -p "$JUMP_PORT" -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
    "ssh -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${VHOST} '$*'"
}
# node_root <<'EOS' ... EOS : run a script on the primary node as root
node_root() {
  ssh -i "$KEY" -p "$JUMP_PORT" -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
    "ssh -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${VHOST} 'sudo bash -s'"
}

# nodes_on <command> : run a shell command on every node of the target
nodes_on() {
  local cmd="$1" h
  for h in $SERVERS; do
    ssh -i "$KEY" -p "$JUMP_PORT" -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
      "ssh -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${h} '$cmd'" </dev/null
  done
}

# trust_staging_roots: make the runner's TLS clients trust the Let's Encrypt
# staging chain. Full runs use staging certificates for real (no production
# ACME), so strict verification needs the staging roots. Node/Playwright honours
# NODE_EXTRA_CA_CERTS; Chromium reads the NSS database.
trust_staging_roots() {
  local dir="${XDG_CACHE_HOME:-$HOME/.cache}/umkim-thin"
  local n f
  mkdir -p "$dir"
  : >"$dir/staging-roots.pem"
  for n in x1 x2; do
    f="$dir/staging-root-$n.pem"
    [ -s "$f" ] || curl -fsS "https://letsencrypt.org/certs/staging/letsencrypt-stg-root-$n.pem" -o "$f" || return 1
    cat "$f" >>"$dir/staging-roots.pem"
    if command -v certutil >/dev/null 2>&1; then
      certutil -d "sql:$HOME/.pki/nssdb" -A -n "LE staging root $n" -t "C,," -i "$f" >/dev/null 2>&1 || true
    fi
  done
  export NODE_EXTRA_CA_CERTS="$dir/staging-roots.pem"
  echo "   staging roots trusted ($dir/staging-roots.pem)"
}

# sync_clock : set the nodes' clocks from this host, then let chrony adjust.
# A VM restored from a snapshot boots with a stale system clock and chrony only
# *slews* a multi-hour offset (and may not have a source yet), so `makestep`
# alone is not enough: the first provision step is a dnf install whose GPG
# signatures are clock-sensitive. Setting the time directly makes that
# deterministic.
sync_clock() {
  local epoch
  epoch=$(date -u +%s)
  nodes_on "timeout 20 sudo -n date -u -s @${epoch} >/dev/null 2>&1; timeout 10 sudo -n chronyc makestep >/dev/null 2>&1; true"
}

# http() { ... } : fetch the status code of a URL (000 on failure)
http() { curl -ksS -o /dev/null -m 20 -w '%{http_code}' "$1" 2>/dev/null || echo 000; }

# The directory admin password (used to log into the console and Authelia).
admin_password() {
  node_root <<'EOS'
K=/usr/local/bin/kubectl
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
pw=""
if $K -n umkim-thin get deploy umkim-thin-console >/dev/null 2>&1; then
  # Cluster mode keeps the live secrets on the console PVC.
  pw=$($K -n umkim-thin exec deploy/umkim-thin-console -- cat /data/secrets.yml 2>/dev/null |
    sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p')
  # Older single-node installs kept them in the host state directory; keep the
  # fallback for a migration window.
  [ -n "$pw" ] || pw=$($K -n umkim-thin exec deploy/umkim-thin-console -- cat /etc/umkim-thin/secrets.yml 2>/dev/null |
    sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p')
fi
[ -n "$pw" ] || pw=$(sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p' /etc/umkim-thin/secrets.yml 2>/dev/null)
printf '%s' "$pw"
EOS
}

# mask <value> : kept as a no-op hook for scripts that mark secrets before
# logging. There is no hosted log to redact here.
mask() {
  :
}

# cli <args> : run the umkim-thin CLI inside the console pod (works in host and
# cluster mode; the pod always carries the binary and the catalog).
cli() {
  node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin --catalog /opt/umkim-thin/catalog $*
EOS
}

# pin_hosts <addr> <host>... : resolve the target's names straight to it. The
# lab DNS points the zone at a public DDNS address, which does not hairpin, so
# The lab runner pins the VIP.
pin_hosts() {
  local addr="$1"; shift
  local tmp; tmp=$(mktemp)
  awk -v hosts="$*" 'BEGIN { n=split(hosts, a, " "); for (i=1;i<=n;i++) drop[a[i]]=1 }
    { keep=1; for (i=2;i<=NF;i++) if ($i in drop) keep=0; if (keep) print }' /etc/hosts > "$tmp"
  for h in "$@"; do echo "$addr $h" >> "$tmp"; done
  sudo cp "$tmp" /etc/hosts
  rm -f "$tmp"
}

# run_tags <tags> : re-run part of the playbook (already shipped by
# provision.sh) on the jumphost, e.g. to apply policies once app namespaces
# exist.
run_tags() {
  ssh -n -i "$KEY" -p "$JUMP_PORT" -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
    "cd '${REMOTE_WORKDIR}/ansible' && ansible-playbook -i inventory.ini site.yml --tags $*"
}
