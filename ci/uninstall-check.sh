#!/usr/bin/env bash
# Prove that `app uninstall` without purge keeps the app's data, and that a
# reinstall reuses it.
#
#   ./ci/uninstall-check.sh single [--app homebox] [--env deploy/provision-single.env]
#
# It drives the console CLI inside the pod, so it exercises the same path the
# Applications page and the invite/setup flows use: write a marker on the app's
# data volume, uninstall (no purge), assert the PVC and the generated Secret
# survived, reinstall, and assert the marker is still readable and the app is
# Ready again. A purge is deliberately not exercised: it deletes the namespace
# and is covered by the docs, not by a data-preservation claim.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/uninstall-check.sh single|ha [--app NAME] [--env FILE]}"
shift || true
APP=homebox
while [ "$#" -gt 0 ]; do
  case "$1" in
    --app) APP="$2"; shift 2 ;;
    --app=*) APP="${1#*=}"; shift ;;
    --env) PROVISION_ENV="$2"; shift 2 ;;
    --env=*) PROVISION_ENV="${1#*=}"; shift ;;
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

fail=0
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fail=1; }
mark="ci-uninstall-${UMKIM_THIN_CI_RUN:-$(date +%s)}"

# k and podsh as in ci/backup-check.sh.
k() { node "sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl $*"; }
podsh() {
  node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n umkim-thin exec deploy/umkim-thin-console -- sh -c '$1'
EOS
}

# marker_pod <action>: read/write the marker on the app's first RWO data claim
# through a helper pod, so no app credential is needed.
marker() {
  local action=$1 pvc pod="ci-uninstall-mark"
  node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
pvc=\$(/usr/local/bin/kubectl -n $APP get pvc -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1)
[ -n "\$pvc" ] || { echo "no pvc"; exit 1; }
node=\$(/usr/local/bin/kubectl -n $APP get pods -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
/usr/local/bin/kubectl -n $APP delete pod $pod --ignore-not-found --grace-period=0 --force >/dev/null 2>&1
cat <<YAML | /usr/local/bin/kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: $pod, namespace: $APP, labels: {app: ci-markers}}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 0
  securityContext: {seccompProfile: {type: RuntimeDefault}}
  \${node:+nodeName: \$node}
  containers:
    - name: marker
      image: busybox:1.36
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh","-c","sleep 300"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, runAsUser: 0, runAsGroup: 0}
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: \$pvc}}]
YAML
/usr/local/bin/kubectl -n $APP wait --for=condition=Ready pod/$pod --timeout=180s >/dev/null 2>&1 || { echo "helper did not start"; exit 1; }
case $action in
  write) /usr/local/bin/kubectl -n $APP exec $pod -- sh -c 'echo "$mark" > /data/$mark.txt' && echo "wrote $mark" ;;
  read)  /usr/local/bin/kubectl -n $APP exec $pod -- cat /data/$mark.txt ;;
esac
rc=\$?
/usr/local/bin/kubectl -n $APP delete pod $pod --grace-period=0 --force >/dev/null 2>&1
exit \$rc
EOS
}

echo "== uninstall keeps data: $APP =="
if ! cli app status "$APP" >/dev/null 2>&1; then
  skip "$APP is not installed on $TARGET"
  exit 0
fi

# 1. Marker on the data volume.
if out=$(marker write 2>&1) && printf '%s' "$out" | grep -q "$mark"; then
  ok "wrote the marker on the data volume"
else
  bad "could not write the marker: $out"
  echo "uninstall check failed" >&2
  exit 1
fi

# The generated credential Secret must survive too: the charts use `lookup` to
# keep existing values, so a deleted Secret would be re-generated on reinstall
# and no longer match the retained database. Hash the whole payload, whatever
# keys this chart uses.
secret_hash() {
  k -n "$APP" get secret "${APP}-secrets" -o jsonpath='{.data}' 2>/dev/null | sha256sum | cut -d' ' -f1
}
have_secret=0
k -n "$APP" get secret "${APP}-secrets" >/dev/null 2>&1 && have_secret=1
pw_before=""
[ "$have_secret" -eq 1 ] && pw_before=$(secret_hash)


# 2. Uninstall without purge.
if out=$(cli app uninstall "$APP" 2>&1); then
  printf '%s\n' "$out" | sed 's/^/    /'
  ok "uninstalled $APP without purge"
else
  bad "uninstall failed: $out"
  echo "uninstall check failed" >&2
  exit 1
fi

# 3. The claim, the data and the generated Secret are still there.
if [ -n "$(k -n "$APP" get pvc -o name 2>/dev/null)" ]; then
  ok "the data claim survived the uninstall"
else
  bad "no PVC is left; the uninstall deleted the data"
fi
if out=$(marker read 2>&1) && printf '%s' "$out" | grep -q "$mark"; then
  ok "the marker is still readable"
else
  bad "the marker is gone: $out"
fi
if [ "$have_secret" -eq 0 ]; then
  skip "$APP keeps no generated Secret to compare"
else
  pw_after=$(secret_hash)
  if [ -n "$pw_before" ] && [ "$pw_before" = "$pw_after" ]; then
    ok "the generated credentials were kept (reinstall can still log in)"
  else
    bad "the generated credentials changed; a reinstall would not match the database"
  fi
fi

# 4. Reinstall adopts the kept objects and the data is still there.
if out=$(cli app install "$APP" 2>&1); then
  ok "reinstalled $APP"
else
  bad "reinstall failed: $out"
  echo "uninstall check failed" >&2
  exit 1
fi
if out=$(marker read 2>&1) && printf '%s' "$out" | grep -q "$mark"; then
  ok "the marker survived the reinstall"
else
  bad "the marker is gone after the reinstall: $out"
fi
if k -n "$APP" rollout status deploy --timeout=300s >/dev/null 2>&1; then
  ok "the app is Ready again"
else
  bad "the app did not become Ready after the reinstall"
fi

# 5. Clean up the marker so a later run starts clean.
marker remove >/dev/null 2>&1 || true

if [ "$fail" -ne 0 ]; then
  echo "uninstall check failed" >&2
  exit 1
fi
echo "uninstall check passed"
