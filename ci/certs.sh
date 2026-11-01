#!/usr/bin/env bash
# Save or restore the staging environments' Traefik ACME state.
#
#   ./ci/certs.sh save single
#   ./ci/certs.sh restore ha
#   ./ci/certs.sh restore single --regen   # skip the cache, let ACME issue
#
# The ACME account and the issued certificates live in /data/acme.json on the
# kube-system/traefik PVC. A normal run restores the cached file before the
# apps are installed, so a reset does not re-issue certificates. The weekly
# full run uses --regen: it lets Traefik issue fresh certificates and then
# saves them.
#
# The cache lives outside the cluster (CI_CERT_CACHE, default
# $HOME/.cache/umkim-thin/certs) so it survives a snapshot reset.
set -uo pipefail
cd "$(dirname "$0")/.."

MODE="${1:?usage: ci/certs.sh save|restore <single|ha> [--regen]}"
TARGET="${2:?usage: ci/certs.sh save|restore <single|ha> [--regen]}"
shift 2
REGEN=0
for a in "$@"; do
  case "$a" in
    --regen) REGEN=1 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

CACHE_DIR="${CI_CERT_CACHE:-$HOME/.cache/umkim-thin/certs}"
CACHE="$CACHE_DIR/$ZONE/acme.json"
HELPER=ci-certs
NS=kube-system

say() { printf '   %s\n' "$*"; }
warn() { printf '   WARN: %s\n' "$*" >&2; }

# The helper is a small public image, so it does not depend on the console
# image having been imported on every node (HA schedules freely).
helper_yaml() { # helper_yaml <ro|rw>
  local ro="$1" attr="false"
  [ "$ro" = "ro" ] && attr="true"
  cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $HELPER
  namespace: $NS
  labels:
    app.kubernetes.io/managed-by: umkim-thin
    umkim-thin/ci: certs
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 0
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: certs
      image: busybox:1.36
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c", "sleep 600"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        runAsUser: 0
        runAsGroup: 0
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: $attr
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: traefik
        readOnly: $attr
YAML
}

# traefik_uid: the UID the Traefik container runs as (root when unset).
traefik_uid() {
  local uid
  # The chart sets runAsUser at the pod level; the container may inherit it.
  uid=$(node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS get deploy traefik -o jsonpath='{.spec.template.spec.securityContext.runAsUser}'" 2>/dev/null | tr -d '[:space:]')
  if [ -z "$uid" ]; then
    uid=$(node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS get deploy traefik -o jsonpath='{.spec.template.spec.containers[0].securityContext.runAsUser}'" 2>/dev/null | tr -d '[:space:]')
  fi
  # The k3s Traefik chart runs as 65532 when neither is set.
  printf '%s' "${uid:-65532}"
}

scale() { # scale <replicas>
  node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS scale deploy/traefik --replicas=$1" >/dev/null 2>&1 || true
}

wait_traefik_gone() {
  local i
  for i in $(seq 1 36); do
    local n
    n=$(node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS get pods -l app.kubernetes.io/name=traefik -o name" 2>/dev/null | wc -l)
    [ "${n:-0}" -eq 0 ] && return 0
    sleep 5
  done
  return 1
}

wait_traefik_up() {
  node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS rollout status deploy/traefik --timeout=180s" >/dev/null 2>&1
}

# The gateway DNATs to the Traefik pod IP it resolved at startup. Scaling
# Traefik replaces that pod, so restart the gateway after the certificate work.
restart_gateway() {
  node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n wireguard get ds wireguard-gateway" >/dev/null 2>&1 || return 0
  node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n wireguard rollout restart ds/wireguard-gateway" >/dev/null 2>&1 || true
  node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n wireguard rollout status ds/wireguard-gateway --timeout=180s" >/dev/null 2>&1 || true
}

apply_helper() { # apply_helper <ro|rw>
  node_root >/dev/null 2>&1 <<EOS
set -eu
K="sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl"
\$K -n $NS delete pod $HELPER --ignore-not-found >/dev/null 2>&1 || true
cat <<'YAML' | \$K apply -f - >/dev/null
$(helper_yaml "$1")
YAML
\$K -n $NS wait --for=condition=Ready pod/$HELPER --timeout=180s >/dev/null
EOS
}

drop_helper() {
  node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS delete pod $HELPER --ignore-not-found --grace-period=0 --force" >/dev/null 2>&1 || true
}

do_save() {
  mkdir -p "$(dirname "$CACHE")"
  scale 0
  wait_traefik_gone || warn "traefik pod did not go away; reading anyway"
  apply_helper ro || { warn "could not create the cert helper"; scale 1; return 1; }
  local tmp="$CACHE.tmp.$$"
  if node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS exec $HELPER -- cat /data/acme.json" >"$tmp" 2>/dev/null && [ -s "$tmp" ]; then
    if python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$tmp" 2>/dev/null; then
      mv "$tmp" "$CACHE"
      chmod 600 "$CACHE"
      say "saved $CACHE ($(wc -c <"$CACHE") bytes)"
    else
      warn "the ACME file is not valid JSON; keeping the previous cache"
      rm -f "$tmp"
    fi
  else
    warn "could not read /data/acme.json"
    rm -f "$tmp"
  fi
  drop_helper
  scale 1
  wait_traefik_up || warn "traefik did not come back"
  restart_gateway
}

do_restore() {
  if [ "$REGEN" = 1 ]; then
    say "regen requested; not restoring the cache"
    return 0
  fi
  if [ ! -s "$CACHE" ]; then
    # A stale/truncated acme.json disables the resolver entirely. Clear it so
    # Traefik starts ACME fresh instead of serving the default certificate.
    say "no cache at $CACHE; clearing any stale ACME state and letting ACME issue"
    scale 0
    wait_traefik_gone || warn "traefik pod did not go away"
    if apply_helper rw; then
      node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl -n $NS exec $HELPER -- rm -f /data/acme.json" >/dev/null 2>&1 || true
      drop_helper
    fi
    scale 1
    wait_traefik_up || warn "traefik did not come back"
    restart_gateway
    return 0
  fi
  python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$CACHE" 2>/dev/null || {
    warn "$CACHE is not valid JSON; not restoring"
    return 1
  }
  local uid b64
  uid=$(traefik_uid)
  b64=$(base64 -w0 "$CACHE")
  scale 0
  wait_traefik_gone || warn "traefik pod did not go away; restoring anyway"
  apply_helper rw || { warn "could not create the cert helper"; scale 1; return 1; }
  # Copy the payload as a file instead of passing it in the exec command.
  # A single argv string is capped at 128 KiB (E2BIG) and the ssh layers
  # re-quote the command, which truncated a large acme.json. The heredoc goes
  # over stdin (no size limit) and kubectl cp streams it as a tar.
  if node_root <<EOS
set -eu
K="sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl"
cat > /tmp/umkim-acme.b64 <<'UMKIM_ACME'
$b64
UMKIM_ACME
base64 -d /tmp/umkim-acme.b64 > /tmp/umkim-acme.json
rm -f /tmp/umkim-acme.b64
\$K -n $NS cp /tmp/umkim-acme.json $HELPER:/data/acme.json >/dev/null
rm -f /tmp/umkim-acme.json
\$K -n $NS exec $HELPER -- chown $uid:$uid /data/acme.json
\$K -n $NS exec $HELPER -- chmod 600 /data/acme.json
EOS
  then
    say "restored $CACHE into kube-system/traefik"
  else
    warn "could not write /data/acme.json"
  fi
  drop_helper
  scale 1
  wait_traefik_up || warn "traefik did not come back"
  restart_gateway
}

case "$MODE" in
  save) do_save ;;
  restore) do_restore ;;
  *) echo "usage: ci/certs.sh save|restore <single|ha> [--regen]" >&2; exit 2 ;;
esac
