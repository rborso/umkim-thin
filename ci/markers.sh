#!/usr/bin/env bash
# Create, verify or delete a data marker in every installed app.
#
#   ./ci/markers.sh create single
#   ./ci/markers.sh verify ha
#   ./ci/markers.sh delete single
#
# API markers go through each app's own API on its ClusterIP (on the node, so
# no portal and no external DNS). OIDC-only apps have no admin password in a
# Secret, so they get a marker file on their data PVC through a small helper
# pod. The marker name defaults to ci-marker-<run id>.
#
# The same script is used before the backup and after the restore: create,
# backup, delete, restore, verify.
set -uo pipefail
cd "$(dirname "$0")/.."

ACTION="${1:?usage: ci/markers.sh create|verify|delete <single|ha> [--mark name]}"
TARGET="${2:?usage: ci/markers.sh create|verify|delete <single|ha> [--mark name]}"
shift 2
MARK="${UMKIM_THIN_MARK:-ci-marker-${UMKIM_THIN_CI_RUN:-$(date +%s)}}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --mark) MARK="$2"; shift 2 ;;
    --mark=*) MARK="${1#*=}"; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
case "$ACTION" in create|verify|delete) ;; *) echo "action must be create, verify or delete" >&2; exit 2 ;; esac
MARK=$(printf '%s' "$MARK" | tr -cd 'A-Za-z0-9._-')
[ -n "$MARK" ] || { echo "empty marker" >&2; exit 2; }
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

# A helper pod killed mid-run (a reboot during the fault phase) stays in the
# Unknown phase and would make the "every pod is back" assertion fail later.
# Clean up anything a previous run left behind.
node_root >/dev/null 2>&1 <<'EOS' || true
K="sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/bin/kubectl"
for ns in $($K get pods -A -l app=ci-markers -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u); do
  $K -n "$ns" delete pod -l app=ci-markers --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true
done
EOS

remote() {
  cat <<REMOTE
set -u
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
MARK='$MARK'
ACTION='$ACTION'
fails=0
ok()  { echo "  ok    \$*"; }
bad() { echo "  FAIL  \$*"; fails=\$((fails + 1)); }
skip() { echo "  skip  \$*"; }
sec() { \$K -n "\$1" get secret "\$2" -o jsonpath="{.data.\$3}" 2>/dev/null | base64 -d; }
ip()  { \$K -n "\$1" get svc "\$2" -o jsonpath='{.spec.clusterIP}' 2>/dev/null; }
has() { \$K -n "\$1" get deploy "\$2" >/dev/null 2>&1; }
code() { curl -s -o /tmp/ci-marker.out -w '%{http_code}' "\$@"; }
jget() { python3 -c "import json,sys; d=json.load(sys.stdin); print(\$1)" 2>/dev/null; }

gitea() {
  has gitea gitea || { skip gitea; return; }
  local pw ip c
  pw=\$(sec gitea gitea-secrets admin-password); ip=\$(ip gitea gitea)
  case \$ACTION in
    create)
      c=\$(code -u "gitea-admin:\$pw" -H 'Content-Type: application/json' \
        -d "{\"name\":\"\$MARK\",\"auto_init\":true,\"private\":true}" \
        "http://\$ip:3000/api/v1/user/repos")
      [ "\$c" = 201 ] || [ "\$c" = 409 ] && ok "gitea repo (\$c)" || bad "gitea repo (\$c)" ;;
    verify)
      c=\$(code -u "gitea-admin:\$pw" "http://\$ip:3000/api/v1/repos/gitea-admin/\$MARK")
      [ "\$c" = 200 ] && ok "gitea repo" || bad "gitea repo (\$c)" ;;
    delete)
      c=\$(code -X DELETE -u "gitea-admin:\$pw" "http://\$ip:3000/api/v1/repos/gitea-admin/\$MARK")
      { [ "\$c" = 204 ] || [ "\$c" = 404 ]; } && ok "gitea repo removed" || bad "gitea repo delete (\$c)" ;;
  esac
}

nextcloud() {
  has nextcloud nextcloud || { skip nextcloud; return; }
  local pw ip c
  pw=\$(sec nextcloud nextcloud-secrets admin-password); ip=\$(ip nextcloud nextcloud)
  case \$ACTION in
    create)
      c=\$(printf 'marker %s\\n' "\$MARK" | code -u "nc-admin:\$pw" -T - "http://\$ip/remote.php/dav/files/nc-admin/\$MARK.txt")
      { [ "\$c" = 201 ] || [ "\$c" = 204 ]; } && ok "nextcloud file (\$c)" || bad "nextcloud file (\$c)" ;;
    verify)
      c=\$(code -u "nc-admin:\$pw" "http://\$ip/remote.php/dav/files/nc-admin/\$MARK.txt")
      [ "\$c" = 200 ] && ok "nextcloud file" || bad "nextcloud file (\$c)" ;;
    delete)
      c=\$(code -X DELETE -u "nc-admin:\$pw" "http://\$ip/remote.php/dav/files/nc-admin/\$MARK.txt")
      { [ "\$c" = 204 ] || [ "\$c" = 404 ]; } && ok "nextcloud file removed" || bad "nextcloud file delete (\$c)" ;;
  esac
}

paperless() {
  has paperless paperless-web || { skip paperless; return; }
  local pw ip tok c id
  pw=\$(sec paperless paperless-secrets admin-password); ip=\$(ip paperless paperless-web)
  tok=""
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
    tok=\$(curl -s -d "username=admin&password=\$pw" "http://\$ip:8000/api/token/" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    [ -n "\$tok" ] && break
    sleep 10
  done
  [ -n "\$tok" ] || { bad "paperless token"; return; }
  case \$ACTION in
    create)
      c=\$(code -H "Authorization: Token \$tok" -H 'Content-Type: application/json' \
        -d "{\"name\":\"\$MARK\"}" "http://\$ip:8000/api/tags/")
      # Creating is idempotent: a marker left by an earlier phase (or a
      # previous run) answers 400/409, so accept it when the tag is there.
      if [ "\$c" = 201 ]; then
        ok "paperless tag"
      elif [ "\$c" = 400 ] || [ "\$c" = 409 ]; then
        n=\$(curl -s -H "Authorization: Token \$tok" "http://\$ip:8000/api/tags/?name__iexact=\$MARK" | grep -c "\$MARK")
        [ "\$n" -ge 1 ] && ok "paperless tag (exists)" || bad "paperless tag (\$c)"
      else
        bad "paperless tag (\$c)"
      fi ;;
    verify)
      c=\$(curl -s -H "Authorization: Token \$tok" "http://\$ip:8000/api/tags/?name__iexact=\$MARK" | grep -c "\$MARK")
      [ "\$c" -ge 1 ] && ok "paperless tag" || bad "paperless tag missing" ;;
    delete)
      id=\$(curl -s -H "Authorization: Token \$tok" "http://\$ip:8000/api/tags/?name__iexact=\$MARK" | jget "d['results'][0]['id']")
      [ -n "\$id" ] || { ok "paperless tag already gone"; return; }
      c=\$(code -X DELETE -H "Authorization: Token \$tok" "http://\$ip:8000/api/tags/\$id/")
      [ "\$c" = 204 ] && ok "paperless tag removed" || bad "paperless tag delete (\$c)" ;;
  esac
}

mealie() {
  has mealie mealie || { skip mealie; return; }
  local pw ip tok c id
  pw=\$(sec mealie mealie-secrets admin-password); ip=\$(ip mealie mealie)
  tok=""
  for _ in 1 2 3 4 5 6; do
    tok=\$(curl -s -d "username=changeme@example.com&password=\$pw" "http://\$ip:9000/api/auth/token" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
    [ -n "\$tok" ] && break
    sleep 5
  done
  [ -n "\$tok" ] || { bad "mealie token"; return; }
  case \$ACTION in
    create)
      c=\$(code -H "Authorization: Bearer \$tok" -H 'Content-Type: application/json' \
        -d "{\"name\":\"\$MARK\"}" "http://\$ip:9000/api/organizers/tags")
      # Idempotent: an existing tag answers 400/409; accept it if it is there.
      if [ "\$c" = 200 ] || [ "\$c" = 201 ]; then
        ok "mealie tag (\$c)"
      elif [ "\$c" = 400 ] || [ "\$c" = 409 ]; then
        n=\$(curl -s -H "Authorization: Bearer \$tok" "http://\$ip:9000/api/organizers/tags" | grep -c "\$MARK")
        [ "\$n" -ge 1 ] && ok "mealie tag (exists)" || bad "mealie tag (\$c)"
      else
        bad "mealie tag (\$c)"
      fi ;;
    verify)
      c=\$(curl -s -H "Authorization: Bearer \$tok" "http://\$ip:9000/api/organizers/tags" | grep -c "\$MARK")
      [ "\$c" -ge 1 ] && ok "mealie tag" || bad "mealie tag missing" ;;
    delete)
      id=\$(curl -s -H "Authorization: Bearer \$tok" "http://\$ip:9000/api/organizers/tags" | jget "next((x['id'] for x in (d.get('items') or d) if x.get('name')=='$MARK'), '')")
      [ -n "\$id" ] || { ok "mealie tag already gone"; return; }
      c=\$(code -X DELETE -H "Authorization: Bearer \$tok" "http://\$ip:9000/api/organizers/tags/\$id")
      { [ "\$c" = 200 ] || [ "\$c" = 204 ]; } && ok "mealie tag removed" || bad "mealie tag delete (\$c)" ;;
  esac
}

immich() {
  has immich immich-server || { skip immich; return; }
  local pw ip tok c id
  pw=\$(sec immich immich-secrets admin-password); ip=\$(ip immich immich-server)
  tok=\$(curl -s -H 'Content-Type: application/json' \
    -d "{\"email\":\"admin@immich.local\",\"password\":\"\$pw\"}" "http://\$ip:2283/api/auth/login" |
    sed -n 's/.*"accessToken":"\([^"]*\)".*/\1/p')
  if [ -z "\$tok" ] && [ "\$ACTION" = create ]; then
    curl -s -o /dev/null -H 'Content-Type: application/json' \
      -d "{\"email\":\"admin@immich.local\",\"password\":\"\$pw\",\"name\":\"Admin\"}" \
      "http://\$ip:2283/api/auth/admin-sign-up" || true
    tok=\$(curl -s -H 'Content-Type: application/json' \
      -d "{\"email\":\"admin@immich.local\",\"password\":\"\$pw\"}" "http://\$ip:2283/api/auth/login" |
      sed -n 's/.*"accessToken":"\([^"]*\)".*/\1/p')
  fi
  [ -n "\$tok" ] || { bad "immich login"; return; }
  case \$ACTION in
    create)
      c=\$(code -H "Authorization: Bearer \$tok" -H 'Content-Type: application/json' \
        -d "{\"albumName\":\"\$MARK\"}" "http://\$ip:2283/api/albums")
      { [ "\$c" = 200 ] || [ "\$c" = 201 ]; } && ok "immich album (\$c)" || bad "immich album (\$c)" ;;
    verify)
      c=\$(curl -s -H "Authorization: Bearer \$tok" "http://\$ip:2283/api/albums" | grep -c "\$MARK")
      [ "\$c" -ge 1 ] && ok "immich album" || bad "immich album missing" ;;
    delete)
      id=\$(curl -s -H "Authorization: Bearer \$tok" "http://\$ip:2283/api/albums" | jget "next((x['id'] for x in d if x.get('albumName')=='$MARK'), '')")
      [ -n "\$id" ] || { ok "immich album already gone"; return; }
      c=\$(code -X DELETE -H "Authorization: Bearer \$tok" "http://\$ip:2283/api/albums/\$id")
      [ "\$c" = 204 ] && ok "immich album removed" || bad "immich album delete (\$c)" ;;
  esac
}

arr() { # arr <sonarr|radarr|prowlarr>
  local app=\$1 ns=servarr api=v3 ip key c id
  has servarr \$app || { skip \$app; return; }
  [ "\$app" = prowlarr ] && api=v1
  ip=\$(ip servarr \$app); key=\$(sec servarr servarr-api-keys "\$app-api-key")
  [ -n "\$key" ] || { bad "\$app api key"; return; }
  case \$ACTION in
    create)
      c=\$(code -H "X-Api-Key: \$key" -H 'Content-Type: application/json' \
        -d "{\"label\":\"\$MARK\"}" "http://\$ip/api/\$api/tag")
      { [ "\$c" = 200 ] || [ "\$c" = 201 ]; } && ok "\$app tag (\$c)" || bad "\$app tag (\$c)" ;;
    verify)
      c=\$(curl -s -H "X-Api-Key: \$key" "http://\$ip/api/\$api/tag" | grep -c "\$MARK")
      [ "\$c" -ge 1 ] && ok "\$app tag" || bad "\$app tag missing" ;;
    delete)
      id=\$(curl -s -H "X-Api-Key: \$key" "http://\$ip/api/\$api/tag" | jget "next((x['id'] for x in d if x.get('label')=='$MARK'), '')")
      [ -n "\$id" ] || { ok "\$app tag already gone"; return; }
      c=\$(code -X DELETE -H "X-Api-Key: \$key" "http://\$ip/api/\$api/tag/\$id")
      [ "\$c" = 200 ] && ok "\$app tag removed" || bad "\$app tag delete (\$c)" ;;
  esac
}

jellyfin() {
  has servarr jellyfin || { skip jellyfin; return; }
  local user pw ip tok c id
  user=\$(sec servarr servarr-bootstrap JELLYFIN_ADMIN_USER)
  pw=\$(sec servarr servarr-bootstrap JELLYFIN_ADMIN_PASSWORD)
  ip=\$(ip servarr jellyfin)
  tok=\$(curl -s -H 'Content-Type: application/json' \
    -H 'X-Emby-Authorization: MediaBrowser Client="ci", Device="ci", DeviceId="ci", Version="1.0"' \
    -d "{\"Username\":\"\$user\",\"Pw\":\"\$pw\"}" "http://\$ip/Users/AuthenticateByName" |
    sed -n 's/.*"AccessToken":"\([^"]*\)".*/\1/p')
  [ -n "\$tok" ] || { skip "jellyfin (admin not ready)"; return; }
  case \$ACTION in
    create)
      c=\$(code -X POST -H "X-Emby-Token: \$tok" "http://\$ip/Collections?Name=\$MARK")
      [ "\$c" = 200 ] && ok "jellyfin collection" || bad "jellyfin collection (\$c)" ;;
    verify)
      c=\$(curl -s -H "X-Emby-Token: \$tok" "http://\$ip/Items?Recursive=true&IncludeItemTypes=BoxSet&Fields=Name" | grep -c "\$MARK")
      [ "\$c" -ge 1 ] && ok "jellyfin collection" || bad "jellyfin collection missing" ;;
    delete)
      id=\$(curl -s -H "X-Emby-Token: \$tok" "http://\$ip/Items?Recursive=true&IncludeItemTypes=BoxSet&Fields=Name" | jget "next((x['Id'] for x in d.get('Items',[]) if x.get('Name')=='$MARK'), '')")
      [ -n "\$id" ] || { ok "jellyfin collection already gone"; return; }
      c=\$(code -X DELETE -H "X-Emby-Token: \$tok" "http://\$ip/Items/\$id")
      [ "\$c" = 204 ] && ok "jellyfin collection removed" || bad "jellyfin collection delete (\$c)" ;;
  esac
}

volume() { # volume <namespace> <pvc>
  local ns=\$1 pvc=\$2 pod=ci-mark-\$1 node c
  \$K -n "\$ns" get pvc "\$pvc" >/dev/null 2>&1 || { skip "\$ns/\$pvc"; return; }
  # An RWO volume is attached to the app's node; schedule the helper there.
  node=\$(\$K -n "\$ns" get pods -o json 2>/dev/null | python3 -c '
import json,sys
pvc=sys.argv[1]
d=json.load(sys.stdin)
for p in d.get("items",[]):
    for v in p.get("spec",{}).get("volumes",[]) or []:
        if (v.get("persistentVolumeClaim") or {}).get("claimName")==pvc:
            print(p.get("spec",{}).get("nodeName","")); raise SystemExit
' "\$pvc")
  if [ "\$ACTION" = delete ]; then
    # The helper is only needed to remove the file.
    :
  fi
  \$K -n "\$ns" delete pod "\$pod" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true
  cat <<YAML | \$K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: \$pod
  namespace: \$ns
  labels: {app: ci-markers}
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
  if ! \$K -n "\$ns" wait --for=condition=Ready "pod/\$pod" --timeout=180s >/dev/null 2>&1; then
    bad "\$ns/\$pvc helper did not start"; \$K -n "\$ns" delete pod "\$pod" --grace-period=0 --force >/dev/null 2>&1 || true; return
  fi
  case \$ACTION in
    create) \$K -n "\$ns" exec "\$pod" -- sh -c "echo '\$MARK' > /data/\$MARK.txt" >/dev/null 2>&1 && ok "\$ns/\$pvc marker" || bad "\$ns/\$pvc marker write" ;;
    verify) \$K -n "\$ns" exec "\$pod" -- test -f "/data/\$MARK.txt" >/dev/null 2>&1 && ok "\$ns/\$pvc marker" || bad "\$ns/\$pvc marker missing" ;;
    delete) \$K -n "\$ns" exec "\$pod" -- rm -f "/data/\$MARK.txt" >/dev/null 2>&1 && ok "\$ns/\$pvc marker removed" || bad "\$ns/\$pvc marker delete" ;;
  esac
  \$K -n "\$ns" delete pod "\$pod" --grace-period=0 --force >/dev/null 2>&1 || true
}

echo "== markers: \$ACTION \$MARK =="
gitea
nextcloud
paperless
mealie
immich
arr sonarr
arr radarr
arr prowlarr
jellyfin
volume homebox homebox-data
volume actualbudget actualbudget-data
volume sparkyfitness sparkyfitness-uploads
volume immich immich-data
volume servarr qbittorrent-config
exit \$fails
REMOTE
}

out=$(remote | node_root 2>&1)
rc=$?
printf '%s\n' "$out"
if [ "$rc" -ne 0 ]; then
  echo "markers: $ACTION failed" >&2
  exit 1
fi
echo "markers: $ACTION ok ($MARK)"
