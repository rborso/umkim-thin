#!/usr/bin/env bash
# Tier 1 static checks: everything that needs no cluster. Run from the repo root.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f charts/apps/gitea/Chart.yaml ] || {
  echo "charts/apps submodule is missing; run: git submodule update --init --recursive" >&2
  exit 1
}

identity_values="ci/test-values/identity.yaml"
console_args=(--set consoleHost=console.example.com)

echo ">> gofmt"
unformatted=$(gofmt -l ./cmd ./internal)
if [ -n "$unformatted" ]; then echo "unformatted: $unformatted" >&2; exit 1; fi

echo ">> go build / vet / test"
go build ./...
go vet ./...
go test -race ./...

echo ">> helm lint"
helm lint charts/identity -f "$identity_values" >/dev/null && echo "   ok charts/identity"
helm lint charts/umkim-thin "${console_args[@]}" >/dev/null && echo "   ok charts/umkim-thin"
helm lint charts/wireguard-gateway >/dev/null && echo "   ok charts/wireguard-gateway"

echo ">> no Authelia secret in a ConfigMap"
identity_render=$(helm template identity charts/identity -f "$identity_values")
for v in ci-only-authelia-session ci-only-authelia-storage \
         ci-only-authelia-reset ci-only-lldap-admin; do
  if printf '%s\n' "$identity_render" |
     awk '/^kind: ConfigMap/{cm=1} cm && /^---/{cm=0} cm' | grep -F "$v" >/dev/null; then
    echo "secret $v is rendered into a ConfigMap" >&2
    exit 1
  fi
done
if ! printf '%s\n' "$identity_render" | awk '/^kind: Secret/{found=1} END{exit(found?0:1)}'; then
  echo "identity chart did not render a Secret" >&2
  exit 1
fi
if ! printf '%s\n' "$identity_render" | grep -F 'secretName: authelia-config' >/dev/null; then
  echo "Authelia does not mount authelia-config as a Secret" >&2
  exit 1
fi
if ! printf '%s\n' "$identity_render" | grep -F 'host: "valkey.identity.svc.cluster.local"' >/dev/null; then
  echo "Authelia is not configured to use the Valkey session store" >&2
  exit 1
fi
if ! printf '%s\n' "$identity_render" | grep -F 'name: valkey' >/dev/null; then
  echo "identity chart did not render the Valkey session store" >&2
  exit 1
fi
if ! printf '%s\n' "$identity_render" | grep -F 'theme: "dark"' >/dev/null; then
  echo "identity chart did not render the dark Authelia theme by default" >&2
  exit 1
fi
identity_light=$(helm template identity charts/identity -f "$identity_values" --set authelia.theme=light 2>/dev/null)
if ! printf '%s\n' "$identity_light" | grep -F 'theme: "light"' >/dev/null; then
  echo "identity chart ignored authelia.theme" >&2
  exit 1
fi
echo "   ok"

echo ">> identity portal TLS is driven by portalEntrypoint/portalTLS"
identity_http=$(helm template identity charts/identity -f "$identity_values" \
  --set lldap.exposeWebUI=true --set lldap.webUIHost=lldap.example.com \
  --set traefik.portalEntrypoint=websecure 2>/dev/null)
printf '%s\n' "$identity_http" | grep -F 'certresolver: letsencrypt' >/dev/null || {
  echo "the portal renders no certresolver with the default (Traefik + ACME) mode" >&2
  exit 1
}
lldap_doc=$(printf '%s\n' "$identity_http" | awk 'BEGIN{RS="---\n"} /name: lldap-portal\n/')
if ! printf '%s\n' "$lldap_doc" | grep -F 'router.entrypoints: websecure' >/dev/null; then
  echo "the LLDAP ingress is not on the portal entrypoint (websecure)" >&2
  exit 1
fi
identity_web=$(helm template identity charts/identity -f "$identity_values" \
  --set lldap.exposeWebUI=true --set lldap.webUIHost=lldap.example.com \
  --set traefik.portalEntrypoint=web --set traefik.portalTLS=false 2>/dev/null)
for host in 'host: "auth.example.com"' 'host: "lldap.example.com"'; do
  if ! printf '%s\n' "$identity_web" | grep -F "$host" >/dev/null; then
    echo "external-proxy render is missing $host" >&2
    exit 1
  fi
done
if printf '%s\n' "$identity_web" | grep -F 'certresolver:' >/dev/null; then
  echo "the external-proxy render still asks Traefik for a certificate" >&2
  exit 1
fi
if printf '%s\n' "$identity_web" | grep -F 'router.tls' >/dev/null; then
  echo "the external-proxy render still terminates TLS in Traefik" >&2
  exit 1
fi
if ! printf '%s\n' "$identity_web" | grep -F 'router.entrypoints: web' >/dev/null; then
  echo "the external-proxy render does not publish on the web entrypoint" >&2
  exit 1
fi
echo "   ok"

echo ">> identity VPN middlewares and default cert"
identity_vpn=$(helm template identity charts/identity -f "$identity_values" \
  --set traefik.defaultCertificateSecret=wildcard-ci 2>/dev/null)
printf '%s\n' "$identity_vpn" | grep -F 'vpn-only' >/dev/null || { echo "identity: vpn-only middleware missing" >&2; exit 1; }
printf '%s\n' "$identity_vpn" | grep -F 'vpn-local' >/dev/null || { echo "identity: vpn-local middleware missing" >&2; exit 1; }
printf '%s\n' "$identity_vpn" | grep -F 'certs.yml' >/dev/null || { echo "identity: default certificate store missing" >&2; exit 1; }
echo "   ok"

echo ">> console pod baseline"
console_render=$(helm template t charts/umkim-thin "${console_args[@]}" --set persistence.storageClass=nfs)
for v in "runAsNonRoot: true" "readOnlyRootFilesystem: true" \
         "allowPrivilegeEscalation: false" "type: RuntimeDefault" \
         "drop:" "mountPath: /tmp"; do
  if ! printf '%s\n' "$console_render" | grep -F "$v" >/dev/null; then
    echo "console chart is missing $v" >&2
    exit 1
  fi
done
# The console always runs cluster-style: PVC state, ConfigMap config, Secret
# seed, ServiceAccount kubeconfig, no host network, no hostPath state.
for v in "claimName: umkim-thin-state" "mountPath: /data" \
         "mountPath: /etc/umkim-thin" "name: umkim-thin-config" \
         "secretName: umkim-thin-secrets" "name: umkim-thin-mirror" \
         "serviceAccountName: umkim-thin-console" \
         "name: umkim-thin-home" "home.example.com" "containerPort: 8081"; do
  if ! printf '%s\n' "$console_render" | grep -F "$v" >/dev/null; then
    echo "console chart is missing $v" >&2
    exit 1
  fi
done
if printf '%s\n' "$console_render" | grep -F 'hostNetwork: true' >/dev/null; then
  echo "console chart still enables hostNetwork" >&2
  exit 1
fi
# The single-node storage socket is the only allowed hostPath, and only when
# explicitly enabled.
if printf '%s\n' "$console_render" | grep -F 'hostPath' >/dev/null; then
  echo "console chart rendered an unexpected hostPath with no storageSocket" >&2
  exit 1
fi
socket_render=$(helm template t charts/umkim-thin "${console_args[@]}" --set storageSocket=/run/umkim-thin)
printf '%s\n' "$socket_render" | grep -F 'path: "/run/umkim-thin"' >/dev/null || {
  echo "console chart did not render the storage socket" >&2
  exit 1
}
echo "   ok"

echo ">> console external-proxy (TLS terminated by a front proxy)"
proxy_render=$(helm template t charts/umkim-thin "${console_args[@]}" \
  --set persistence.storageClass=nfs --set externalProxy=true)
# Every console/home ingress moves to the web entrypoint and drops TLS.
if printf '%s\n' "$proxy_render" | grep -F 'certresolver:' >/dev/null; then
  echo "console external-proxy render still requests a certificate" >&2
  exit 1
fi
if printf '%s\n' "$proxy_render" | grep -F 'router.tls' >/dev/null; then
  echo "console external-proxy render still terminates TLS in Traefik" >&2
  exit 1
fi
for v in "router.entrypoints: web" "entryPoints: [web]"; do
  if ! printf '%s\n' "$proxy_render" | grep -F "$v" >/dev/null; then
    echo "console external-proxy render is missing $v" >&2
    exit 1
  fi
done
if printf '%s\n' "$console_render" | grep -E 'router\.entrypoints: web$' >/dev/null; then
  echo "the default console render lost its TLS entrypoint" >&2
  exit 1
fi
echo "   ok"

echo ">> required values fail closed"
if helm template t charts/identity >/dev/null 2>&1; then
  echo "charts/identity rendered without its required values" >&2
  exit 1
fi
if helm template t charts/umkim-thin >/dev/null 2>&1; then
  echo "charts/umkim-thin rendered without consoleHost" >&2
  exit 1
fi
echo "   ok"

echo ">> helm template (single and HA storage classes)"
helm template t charts/umkim-thin "${console_args[@]}" --set persistence.storageClass=local-path >/dev/null
helm template t charts/umkim-thin "${console_args[@]}" --set persistence.storageClass=longhorn-rwo >/dev/null
helm template t charts/identity -f "$identity_values" >/dev/null
helm template t charts/identity -f "$identity_values" \
  --set lldap.deploy=false \
  --set lldap.externalUrl=ldap://directory.example.com:3890 \
  --set authelia.ldapPassword=ci-only-external >/dev/null
helm template t charts/wireguard-gateway >/dev/null
grep -q -- '-d "${GATEWAY_IP}"' charts/wireguard-gateway/templates/configmap.yaml || {
  echo "wireguard DNAT is not destination-scoped" >&2
  exit 1
}
grep -q 'name: wireguard-vpn-dns' charts/wireguard-gateway/templates/vpn-dns.yaml || {
  echo "wireguard split-DNS ConfigMap is missing" >&2
  exit 1
}
echo "   ok"

echo ">> no floating image tags"
if grep -RIn -E 'image:.*:(latest|release|stable)([[:space:]]|$)|tag:[[:space:]]*"?(latest|release|stable)' charts catalog; then
  echo "floating image tag found; pin an exact version or digest" >&2
  exit 1
fi
# The grep above cannot see a tag that is chosen in a template expression
# (`{{ .app.tag | default "latest" }}`). Render every chart and reject a
# rendered image whose tag is floating or absent.
render_float=0
for d in charts/apps/*/; do
  app=$(basename "$d")
  if ! out=$(helm template floatcheck "$d" 2>&1); then
    echo "$app: does not render" >&2; exit 1
  fi
  bad=$(printf '%s\n' "$out" | awk '
    /^[[:space:]]*image:/ {
      img = $2; gsub(/"/, "", img)
      n = split(img, parts, ":")
      if (n < 2 || parts[n] == "latest" || parts[n] == "release" || parts[n] == "stable") print img
    }' | sort -u)
  if [ -n "$bad" ]; then
    echo "$app: floating or untagged image after rendering:" >&2
    printf '  %s\n' "$bad" >&2
    render_float=1
  fi
done
[ "$render_float" -eq 0 ] || exit 1
echo "   ok"

# A Deployment that mounts a ReadWriteOnce claim must use strategy: Recreate.
# With RollingUpdate the replacement pod is scheduled before the old one
# releases the volume, so it sits in FailedAttachVolume and the upgrade hits
# its deadline (this is what broke the Mealie upgrade).
echo ">> RWO workloads recreate instead of rolling"
rwo_bad=0
for f in charts/apps/*/templates/*.yaml; do
  [ -f "$f" ] || continue
  grep -q '^kind: Deployment' "$f" || continue
  grep -q 'persistentVolumeClaim' "$f" || continue
  if grep -q 'ReadWriteMany' "$f"; then continue; fi
  if ! grep -q 'type: Recreate' "$f"; then
    echo "  $f mounts a PVC but has no strategy: Recreate" >&2
    rwo_bad=1
  fi
done
if [ "$rwo_bad" -ne 0 ]; then
  echo "an app Deployment with a ReadWriteOnce claim would hang on upgrade" >&2
  exit 1
fi
echo "   ok"

echo ">> app charts"
for d in charts/apps/*; do
  [ -d "$d" ] || continue
  helm lint "$d" >/dev/null || { echo "helm lint $d failed" >&2; exit 1; }
done
echo "   ok"

echo ">> servarr Seerr data lives on the config PVC"
servarr_render=$(mktemp)
helm template servarr charts/apps/servarr >"$servarr_render"
grep -q 'name: CONFIG_DIRECTORY' "$servarr_render" || {
  echo "servarr: Seerr does not set CONFIG_DIRECTORY (its database stays in the container layer)" >&2; exit 1;
}
grep -q 'value: "/config"' "$servarr_render" || {
  echo "servarr: Seerr's CONFIG_DIRECTORY does not point at the config PVC" >&2; exit 1;
}
grep -q 'fsGroup: 1000' "$servarr_render" || {
  echo "servarr: Seerr's config volume is not writable by the image's UID 1000" >&2; exit 1;
}
rm -f "$servarr_render"
echo "   ok"

echo ">> servarr qBittorrent limits reach the bootstrap and keep an explicit false"
# The limits only work if they arrive as env: the bootstrap reads
# QBITTORRENT_MAX_ACTIVE_*, and `default true` in the template would turn an
# operator's explicit `dontCountSlowTorrents: false` back into "true".
qbt_default=$(mktemp); qbt_false=$(mktemp)
helm template servarr charts/apps/servarr --set bootstrap.enabled=true >"$qbt_default"
for k in QBITTORRENT_MAX_ACTIVE_TORRENTS QBITTORRENT_MAX_ACTIVE_DOWNLOADS QBITTORRENT_MAX_ACTIVE_UPLOADS QBITTORRENT_DONT_COUNT_SLOW_TORRENTS; do
  grep -q "$k:" "$qbt_default" || {
    echo "servarr: $k is not in the bootstrap ConfigMap, so the limit never reaches qBittorrent" >&2; exit 1;
  }
done
helm template servarr charts/apps/servarr --set bootstrap.enabled=true \
  --set bootstrap.qbittorrentLimits.dontCountSlowTorrents=false >"$qbt_false"
grep -q 'QBITTORRENT_DONT_COUNT_SLOW_TORRENTS: "false"' "$qbt_false" || {
  echo "servarr: qbittorrentLimits.dontCountSlowTorrents=false is rendered as true (sprig default treats false as empty)" >&2; exit 1;
}
rm -f "$qbt_default" "$qbt_false"
echo "   ok"

echo ">> OIDC apps build their own CA bundle"
paperless_render=$(mktemp)
helm template paperless charts/apps/paperless \
  --set oidc.enabled=true --set oidc.clientSecret=ci-only \
  --set oidc.issuerUrl=https://auth.example.com >"$paperless_render"
grep -q 'name: ca-bundle' "$paperless_render" || { echo "paperless: CA init container missing" >&2; exit 1; }
grep -q 'value: /etc/ssl/combined/ca.crt' "$paperless_render" || { echo "paperless: requests not pointed at the combined CA" >&2; exit 1; }
if grep -q 'value: /etc/ssl/umkim-thin/ca.crt' "$paperless_render"; then
  echo "paperless: raw lab-CA path breaks a public install" >&2; exit 1
fi
rm -f "$paperless_render"

immich_render=$(mktemp)
helm template immich charts/apps/immich \
  --set bootstrap.oauth.enabled=true --set bootstrap.oauth.clientSecret=ci-only \
  --set bootstrap.oauth.issuerUrl=https://auth.example.com >"$immich_render"
grep -q 'name: ca-bundle' "$immich_render" || { echo "immich: CA init container missing" >&2; exit 1; }
grep -q 'value: /etc/ssl/combined/ca.crt' "$immich_render" || { echo "immich: node not pointed at the combined CA" >&2; exit 1; }
if grep -q 'value: /etc/ssl/umkim-thin/ca.crt' "$immich_render"; then
  echo "immich: raw lab-CA path breaks a public install" >&2; exit 1
fi
rm -f "$immich_render"
if grep -Rq 'value: /etc/ssl/umkim-thin/ca.crt' catalog; then
  echo "the catalog must not hard-code the lab CA path" >&2; exit 1
fi

actual_render=$(mktemp)
helm template actualbudget charts/apps/actualbudget \
  --set externalUrl=https://actual.example.com \
  --set oidc.enabled=true --set oidc.clientSecret=ci-only \
  --set oidc.issuerUrl=https://auth.example.com >"$actual_render"
grep -q 'name: ca-bundle' "$actual_render" || { echo "actualbudget: CA init container missing" >&2; exit 1; }
grep -q 'NODE_EXTRA_CA_CERTS' "$actual_render" || { echo "actualbudget: Node CA path missing" >&2; exit 1; }
grep -q 'ACTUAL_OPENID_CLIENT_ID' "$actual_render" || { echo "actualbudget: OIDC client id missing" >&2; exit 1; }
rm -f "$actual_render"

homebox_render=$(mktemp)
helm template homebox charts/apps/homebox \
  --set externalUrl=https://homebox.example.com --set hostname=homebox.example.com \
  --set oidc.enabled=true --set oidc.clientSecret=ci-only \
  --set oidc.issuerUrl=https://auth.example.com >"$homebox_render"
grep -q 'name: ca-bundle' "$homebox_render" || { echo "homebox: CA init container missing" >&2; exit 1; }
grep -q 'SSL_CERT_FILE' "$homebox_render" || { echo "homebox: Go CA path missing" >&2; exit 1; }
grep -q 'HBOX_OIDC_CLIENT_ID' "$homebox_render" || { echo "homebox: OIDC client id missing" >&2; exit 1; }
rm -f "$homebox_render"
echo "   ok"

echo ">> catalog charts match the tree"
for f in catalog/*.yaml; do
  ref=$(awk '/^chart:/{c=1} c && /^  ref:/{print $2; exit}' "$f")
  ver=$(awk '/^chart:/{c=1} c && /^  version:/{print $2; exit}' "$f")
  name=${ref##*/}
  chart_dir=""
  [ -d "charts/apps/$name" ] && chart_dir="charts/apps/$name"
  [ -d "charts/$name" ] && chart_dir="charts/$name"
  [ -n "$chart_dir" ] || { echo "$f: no local chart for $name" >&2; exit 1; }
  chart_ver=$(helm show chart "$chart_dir" | awk '/^version:/{print $2; exit}')
  [ "$chart_ver" = "$ver" ] || {
    echo "$f: chart $chart_dir is $chart_ver, catalog pins $ver" >&2
    exit 1
  }
done
echo "   ok"

echo ">> the mirror keeps the catalogued rollback versions"
# The catalog's versions[] is the upgrade/rollback set the console resolves
# through chartRef. Pruning the mirror must not drop a version still listed there
# (it would fall back to a GitHub tree URL), and it must not wipe the platform
# charts the playbook installs but the catalog does not mention.
prune_py=deploy/ansible/roles/console/files/prune-chart-mirror.py
[ -f "$prune_py" ] || { echo "the mirror pruner is missing: $prune_py" >&2; exit 1; }
work=$(mktemp -d)
mkdir -p "$work/mirror"
# Seed a current version, a catalogued N-1, a platform chart and a stale file.
for n in $(python3 - "$work" <<'PY'
import sys, yaml, glob, pathlib
work = sys.argv[1]
out = []
for f in sorted(glob.glob("catalog/*.yaml")):
    d = yaml.safe_load(open(f)) or {}
    c = d.get("chart") or {}
    ref = (c.get("ref") or "").rstrip("/")
    chart = ref.split("/")[-1] if ref else (d.get("name") or pathlib.Path(f).stem)
    vers = [v.get("version") for v in (c.get("versions") or []) if v.get("version")]
    if vers:
        out.append(f"{chart}-{vers[0]}")
        if len(vers) > 1:
            out.append(f"{chart}-{vers[1]}")
    else:
        out.append(f"{chart}-{c.get('version')}")
print("\n".join(out))
PY
); do
  : > "$work/mirror/$n.tgz"
done
: > "$work/mirror/identity-0.0.1.tgz"
: > "$work/mirror/umkim-thin-0.0.1.tgz"
: > "$work/mirror/stale-9.9.9.tgz"
python3 "$prune_py" "$work/mirror" catalog >/dev/null 2>&1 || {
  echo "the mirror pruner failed" >&2; exit 1;
}
if [ -e "$work/mirror/stale-9.9.9.tgz" ]; then
  echo "the pruner kept a version no catalog entry advertises" >&2; exit 1
fi
# Every catalogued version that was seeded must survive.
missing=$(python3 - "$work" <<'PY'
import sys, yaml, glob, pathlib
work = sys.argv[1]
for f in sorted(glob.glob("catalog/*.yaml")):
    d = yaml.safe_load(open(f)) or {}
    c = d.get("chart") or {}
    ref = (c.get("ref") or "").rstrip("/")
    chart = ref.split("/")[-1] if ref else (d.get("name") or pathlib.Path(f).stem)
    for v in (c.get("versions") or [])[:2]:
        name = f"{chart}-{v['version']}.tgz"
        if not pathlib.Path(work, "mirror", name).exists():
            print(name)
PY
)
rm -rf "$work"
if [ -n "$missing" ]; then
  echo "the pruner dropped catalogued rollback tarballs: $missing" >&2; exit 1
fi
# The Ansible role must not remove the mirror directory: that deleted rollback
# versions the catalog still advertised. Scoping to the chart-mirror path keeps
# this from matching an unrelated `state: absent` elsewhere in the role.
if grep -B2 -A2 'path: "{{ remote_dir }}/chart-mirror"' \
     deploy/ansible/roles/console/tasks/main.yml | grep -q "state: absent"; then
  echo "the console role removes the chart mirror; prune it instead" >&2; exit 1
fi
echo "   ok"

echo ">> uninstall without purge keeps app data"
# Uninstall (no purge) runs a plain `helm uninstall`, which deletes every
# chart-rendered PVC and the generated credential Secret; the storage class
# then reclaims the bytes and a reinstall would mint a password that no longer
# matches the data. Every app PVC and every generated Secret must carry
# helm.sh/resource-policy: keep. `purge` deletes the namespace, so it still works.
for d in charts/apps/*/; do
  app=$(basename "$d")
  if ! render=$(helm template keepcheck "$d" 2>&1); then
    echo "$app: does not render" >&2; exit 1
  fi
  missing=$(printf '%s\n' "$render" | python3 -c '
import re, sys
bad = []
for doc in sys.stdin.read().split("\n---\n"):
    kind = re.search(r"^kind:\s*(\S+)", doc, re.M)
    if not kind or kind.group(1) not in ("PersistentVolumeClaim", "Secret"):
        continue
    name = re.search(r"^  name:\s*(\S+)", doc, re.M)
    if "helm.sh/resource-policy: keep" not in doc:
        bad.append(kind.group(1) + "/" + (name.group(1) if name else "?"))
print(" ".join(bad))
')
  [ -n "$missing" ] || continue
  echo "$app: these would be deleted by an uninstall without purge: $missing" >&2
  echo "add helm.sh/resource-policy: keep to their metadata.annotations" >&2
  exit 1
done
echo "   ok"

echo ">> catalog versions exist in the chart history"
# A catalogued version that was never committed (only ever packaged from a dirty
# tree) is a promise the mirror cannot keep on a fresh install: chartRef misses
# the tgz and hands Helm a GitHub tree URL. Fail instead of offering it.
if [ -e charts/apps/.git ]; then
  for f in catalog/*.yaml; do
    # The chart directory comes from the ref, not the catalog name: the
    # wireguard entry ships the wireguard-gateway chart.
    ref=$(awk '/^chart:/{c=1} c && /^  ref:/{print $2; exit}' "$f")
    app=${ref%/}; app=${app##*/}
    [ -n "$app" ] || app=$(basename "$f" .yaml)
    if [ -d "charts/apps/$app" ]; then
      hist=$(git -C charts/apps log --all -p -- "$app/Chart.yaml" 2>/dev/null \
        | sed -n 's/^+version: *//p' | sort -u)
    elif [ -d "charts/$app" ]; then
      hist=$(git log --all -p -- "charts/$app/Chart.yaml" 2>/dev/null \
        | sed -n 's/^+version: *//p' | sort -u)
    else
      continue
    fi
    for v in $(awk '/^  versions:/{c=1;next} c && /^    - version:/{print $3;next} c && /^[a-z]/{c=0}' "$f"); do
      case "$hist" in
        *"$v"*) ;;
        *) echo "$f: version $v is not in $app/Chart.yaml history (never committed)" >&2; exit 1 ;;
      esac
    done
  done
fi
echo "   ok"

echo ">> a chart change ships with a version bump"
# A template change without a new chart version never reaches an installed
# cluster: the console only upgrades when the catalog names a different chart
# version. The uninstall-keep fix touched 27 templates across all ten charts and
# moved no version, so no cluster would have received it.
if [ -e charts/apps/.git ]; then
  changed=$(git -C charts/apps diff --name-only HEAD -- '*templates*' 2>/dev/null | cut -d/ -f1 | sort -u)
  if [ -n "$changed" ]; then
    bumped=$(git -C charts/apps diff --name-only HEAD -- '*Chart.yaml' 2>/dev/null | cut -d/ -f1 | sort -u)
    for app in $changed; do
      case " $bumped " in
        *" $app "*) ;;
        *)
          echo "charts/apps/$app: templates changed but Chart.yaml version did not; the catalog cannot deliver it" >&2
          exit 1
          ;;
      esac
    done
  fi
fi
echo "   ok"

echo ">> catalog version is newer than the previous release"
# A catalog release is a tarball upload; a stale catalog/VERSION means an
# operator cannot tell the new entries from the ones they already have.
if git rev-parse --git-dir >/dev/null 2>&1 && \
   git rev-parse -q --verify HEAD >/dev/null 2>&1; then
  prev=$(git show HEAD:catalog/VERSION 2>/dev/null | tr -d '[:space:]')
  now=$(tr -d '[:space:]' < catalog/VERSION)
  if [ -n "$prev" ] && [ "$prev" != "$now" ]; then
    newer=$(printf '%s\n%s\n' "$prev" "$now" | sort -V | tail -1)
    if [ "$newer" != "$now" ]; then
      echo "catalog/VERSION went backwards ($prev -> $now)" >&2; exit 1
    fi
  fi
fi
echo "   ok"

echo ">> catalog loads and validates"
if ! go run ./cmd/umkim-thin --catalog catalog catalog list >/dev/null; then
  echo "the catalog does not load; fix the YAML before shipping it" >&2
  exit 1
fi
echo "   ok"

echo ">> chart packaging"
stage=$(mktemp -d)
helm package charts/identity charts/umkim-thin charts/wireguard-gateway -d "$stage" >/dev/null
for d in charts/apps/*; do
  [ -d "$d" ] || continue
  helm package "$d" -d "$stage" >/dev/null
done
ls "$stage" >/dev/null
rm -rf "$stage"

echo ">> novice installer"
./ci/novice-test.sh

echo ">> shell syntax"
for f in ci/*.sh deploy/provision.sh install.sh install-single.sh install-ha.sh verify-ha.sh teardown.sh uninstall-ha.sh tests/e2e/*.sh tests/e2e/migration/*.sh; do
  bash -n "$f" || exit 1
done
echo "   ok"

echo ">> license notices"
cmp -s LICENSE images/servarr-bootstrap/LICENSE || {
  echo "images/servarr-bootstrap/LICENSE is out of sync with LICENSE" >&2; exit 1;
}
cmp -s NOTICE images/servarr-bootstrap/NOTICE || {
  echo "images/servarr-bootstrap/NOTICE is out of sync with NOTICE" >&2; exit 1;
}
for c in charts/identity charts/umkim-thin charts/wireguard-gateway charts/apps/*; do
  [ -f "$c/Chart.yaml" ] || continue
  grep -q 'artifacthub.io/license: Apache-2.0' "$c/Chart.yaml" || {
    echo "$c/Chart.yaml does not declare its Apache-2.0 license" >&2; exit 1;
  }
done
echo "   ok"

echo ">> secrets"
if command -v gitleaks >/dev/null 2>&1; then
  gitleaks detect --source . --redact --no-banner
else
  echo "   skipped (gitleaks not installed)"
fi

echo ">> k3s api request-timeout is unconditional"
# Large volume restores stream through the API and need the long timeout. It
# must not be nested under an optional setting (it once was: HA runs with
# secrets-encryption off and silently lost request-timeout=1h).
if ! grep -q 'request-timeout=1h' deploy/ansible/roles/k3s/templates/config.yaml.j2; then
  echo "the k3s config template has no request-timeout=1h" >&2; exit 1
fi
python3 - deploy/ansible/roles/k3s/templates/config.yaml.j2 <<'PY' || exit 1
import re, sys
src = open(sys.argv[1]).read().splitlines()
depth = 0
for line in src:
    stripped = line.strip()
    if re.match(r"^\{%-?\s*if\b", stripped):
        depth += 1
    elif re.match(r"^\{%-?\s*endif\b", stripped):
        depth -= 1
    elif "request-timeout=1h" in stripped and depth != 0:
        sys.exit("request-timeout=1h is inside a conditional; it must always render")
PY
echo "   ok"

echo ">> kubelet reclaims images and evicts before it wedges"
# The kubelet defaults (image GC at ~85% of the disk, no eviction) let upgrades
# pile up every replaced image layer and let a memory-tight node wedge. Both
# install paths must set the thresholds; a template edit must not drop them.
if ! grep -q 'image-gc-high-threshold=70' deploy/ansible/roles/k3s/templates/config.yaml.j2; then
  echo "the k3s config template has no image GC thresholds" >&2; exit 1
fi
if ! grep -q 'image-gc-high-threshold=70' install.sh; then
  echo "install.sh does not set the kubelet image GC thresholds on a fresh install" >&2; exit 1
fi
if ! grep -q 'eviction-hard=' install.sh || ! grep -q 'eviction-hard=' deploy/ansible/roles/k3s/templates/config.yaml.j2; then
  echo "the kubelet eviction thresholds are missing from an install path" >&2; exit 1
fi
echo "   ok"

echo ">> chart mirror fits the seeding ConfigMap"
# install.sh and the Ansible console role seed the mirror as one ConfigMap. Its
# tarball must stay well under the 1 MiB object limit (and client-side apply's
# 256 KiB annotation limit), or the seed fails with "metadata.annotations: Too
# long" -- which is what happened once chart versions accumulated. Packaging here
# mirrors install.sh: the platform charts plus every app chart, each at ONE
# version, so the budget is deterministic.
if command -v helm >/dev/null 2>&1; then
  stage=$(mktemp -d)
  for d in charts/identity charts/umkim-thin charts/wireguard-gateway charts/apps/*; do
    [ -d "$d" ] || continue
    helm package "$d" --destination "$stage" >/dev/null
  done
  size=$(du -sb "$stage" | cut -f1)
  count=$(find "$stage" -name '*.tgz' | wc -l)
  rm -rf "$stage"
  # A tar of the directory is the payload; leave headroom for tar headers and
  # base64 expansion (Ansible's server-side apply still stores the object).
  if [ "$count" -gt 40 ] || [ "${size:-0}" -gt 350000 ]; then
    echo "the chart mirror is $count charts / $((${size:-0} / 1024)) KiB; it must fit the seeding ConfigMap" >&2
    echo "prune superseded chart versions (see prune_mirror in install.sh)" >&2
    exit 1
  fi
fi
echo "   ok"

echo ">> the mirror pruners retire a superseded platform chart"
# Both installers prune the chart mirror before seeding it into a ConfigMap, and
# both used to keep a *platform* chart by name prefix ("identity-*"). That kept
# identity-0.1.12 for ever next to 0.1.13, so the mirror grew by one file per
# platform-chart bump and crept toward the 1 MiB object cap. The budget check
# above cannot see it: it packages one version per chart.
#
# Exercise both pruners on a mirror that holds a superseded platform version.
if command -v helm >/dev/null 2>&1; then
  pr=$(mktemp -d)
  mkdir -p "$pr/staged" "$pr/catalog"
  cp catalog/*.yaml "$pr/catalog/" 2>/dev/null || true
  if ! ./scripts/package-charts.sh "$pr/staged" >/dev/null 2>&1; then
    echo "scripts/package-charts.sh failed while checking the pruners" >&2
    rm -rf "$pr"; exit 1
  fi
  find "$pr/staged" -name '*.tgz' -printf '%f\n' >"$pr/fresh"

  # --- Ansible pruner: keep catalog ∪ staged ---
  mkdir -p "$pr/mirror"
  cp "$pr"/staged/*.tgz "$pr/mirror/"
  : >"$pr/mirror/identity-0.0.1.tgz"
  : >"$pr/mirror/gitea-0.0.1.tgz"
  # The pruner takes a file of staged names, not a directory: the playbook cannot
  # pass a controller path that does not exist on the target.
  cp "$pr/fresh" "$pr/staged-list.txt"
  if ! python3 deploy/ansible/roles/console/files/prune-chart-mirror.py \
      "$pr/mirror" "$pr/catalog" "$pr/staged-list.txt" >/dev/null 2>&1; then
    echo "prune-chart-mirror.py failed" >&2; rm -rf "$pr"; exit 1
  fi
  stale=$(find "$pr/mirror" -name '*-0.0.1.tgz' | wc -l)
  left=$(find "$pr/mirror" -name '*.tgz' | wc -l)
  want=$(wc -l <"$pr/fresh")
  if [ "$stale" -ne 0 ] || [ "$left" -ne "$want" ]; then
    echo "prune-chart-mirror.py did not retire the superseded versions" >&2
    echo "  stale=$stale left=$left (expected 0 and $want)" >&2
    rm -rf "$pr"; exit 1
  fi

  # An unreadable keep-set must not be read as "the platform charts are not
  # wanted": that silently dropped identity and umkim-thin from a live mirror.
  mkdir -p "$pr/m2"; cp "$pr"/staged/*.tgz "$pr/m2/"
  : >"$pr/m2/identity-0.0.1.tgz"
  if python3 deploy/ansible/roles/console/files/prune-chart-mirror.py \
      "$pr/m2" "$pr/catalog" "$pr/does-not-exist.txt" >/dev/null 2>&1; then
    echo "prune-chart-mirror.py pruned with an unreadable staged list" >&2
    rm -rf "$pr"; exit 1
  fi
  if [ "$(find "$pr/m2" -name '*.tgz' | wc -l)" -ne $((want + 1)) ]; then
    echo "prune-chart-mirror.py changed the mirror despite an unreadable staged list" >&2
    rm -rf "$pr"; exit 1
  fi

  # --- install.sh prune_mirror: keep fresh ∪ current platform charts ---
  # It resolves the platform chart versions from the repo's own Chart.yaml files,
  # so run it from the repo root with the staged set as the work dir.
  rm -rf "$pr/run"; mkdir -p "$pr/run/staged" "$pr/run/chart-mirror"
  cp "$pr"/staged/*.tgz "$pr/run/staged/"
  cp "$pr"/staged/*.tgz "$pr/run/chart-mirror/"
  : >"$pr/run/chart-mirror/identity-0.0.1.tgz"
  : >"$pr/run/chart-mirror/umkim-thin-0.0.1.tgz"
  : >"$pr/run/chart-mirror/wireguard-gateway-0.0.1.tgz"
  find "$pr/run/staged" -name '*.tgz' -printf '%f\n' >"$pr/run/fresh"
  bash -c '
    set -uo pipefail
    cd "$1"
    DATA_DIR="$2"
    info() { :; }
    die() { echo "die: $*" >&2; exit 1; }
    '"$(sed -n '/^prune_mirror()/,/^}/p' install.sh)"'
    prune_mirror "$DATA_DIR/fresh"
  ' _ "$PWD" "$pr/run" >/dev/null 2>&1 || {
    echo "install.sh prune_mirror failed" >&2; rm -rf "$pr"; exit 1; }

  stale=$(find "$pr/run/chart-mirror" -name '*-0.0.1.tgz' 2>/dev/null | wc -l)
  plat=$(find "$pr/run/chart-mirror" \( -name 'identity-*.tgz' -o -name 'umkim-thin-*.tgz' \
    -o -name 'wireguard-gateway-*.tgz' \) 2>/dev/null | wc -l)
  if [ "$stale" -ne 0 ] || [ "$plat" -ne 3 ]; then
    echo "install.sh prune_mirror left a superseded platform version or dropped a current one" >&2
    echo "  stale=$stale platform=$plat (expected 0 and 3)" >&2
    rm -rf "$pr"; exit 1
  fi
  rm -rf "$pr"
fi
echo "   ok"

echo ">> every declared check resolves to a test, and the app docs match"
# The catalog claims each app is tested through verify.checks. Those names are
# only a claim if a test carries them, so resolve every one against the suites
# (and against a harness script for an app with no browser surface). Four entries
# once named checks that appeared in no test at all, and named a journeys/ suite
# as "the" verification even though that suite is informational - so this is the
# guard that makes the category honest rather than typed in.
if command -v go >/dev/null 2>&1; then
  if ! go run ./cmd/umkim-thin catalog verify --strict >/tmp/umkim-verify.txt 2>&1; then
    echo "a catalog entry declares a check no test carries:"
    grep -E "unresolved" /tmp/umkim-verify.txt | sed 's/^/  /' >&2
    echo "add an @check:<name> tag to the test that exercises it" >&2
    exit 1
  fi
  # The generated status doc must match the catalog: a chart bump or a new human
  # pass that is not regenerated would leave the doc describing the old state.
  if ! ./ci/gen-app-docs.py --check >/dev/null 2>&1; then
    echo "docs/apps/testing.md is out of date: run ./ci/gen-app-docs.py" >&2
    exit 1
  fi
fi
echo "   ok"

echo ">> every catalogued version is packaged (current or N-1)"
# docs/versioning-plan.md: "The chart tarball for each advertised version exists
# in the local mirror and its Chart.yaml version/appVersion match the catalog
# entry. The static check enforces this." scripts/package-charts.sh packages the
# current version and the previous one from charts/apps git history, so both must
# exist; anything else in versions[] is an install a user can pick and that will
# fail with "does not appear to be a gzipped archive".
if command -v helm >/dev/null 2>&1; then
  work=$(mktemp -d)
  if ! ./scripts/package-charts.sh "$work" >/dev/null 2>&1; then
    echo "scripts/package-charts.sh failed" >&2
    rm -rf "$work"
    exit 1
  fi
  problems=$(python3 ci/check-mirror-versions.py "$work")
  rm -rf "$work"
  if [ -n "$problems" ]; then
    echo "the catalog and the chart mirror disagree:" >&2
    printf '%s\n' "$problems" | sed 's/^/  /' >&2
    exit 1
  fi
fi
echo "   ok"

echo ">> ansible syntax-check"
if command -v ansible-playbook >/dev/null 2>&1; then
  inv=$(mktemp)
  printf '[server]\nci-host\n[primary]\nci-host\n' > "$inv"
  (cd deploy/ansible && ansible-playbook -i "$inv" --syntax-check site.yml)
  rm -f "$inv"
else
  echo "   skipped (ansible-playbook not installed)"
fi

echo ">> static checks passed"
