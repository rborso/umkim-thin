#!/usr/bin/env bash
# Build the Servarr bootstrap image locally and import it into the target's
# containerd, then enable the chart's bootstrap Job.
#
# This is the fallback path: the console does the same thing on demand (probe,
# build on the pinned node, import, run). Both use the vendored source in
# images/servarr-bootstrap and the same VERSION + commit tag, so neither can
# silently reuse an image built from a different tree.
#
# Nothing is prebuilt or published: the build is podman, and the tag is derived
# from the vendored version + platform commit, so a rebuild is reproducible and
# skippable.
#
#   ./ci/enable-servarr-bootstrap.sh single|ha
#
# The Job needs a Secret with the stack passwords and the chart-owned API keys.
# It has no tracker dependency: indexers are added in Prowlarr after setup.
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/enable-servarr-bootstrap.sh single|ha}"
export TARGET
export PROVISION_ENV="deploy/provision-${TARGET}.env"
[ -f "$PROVISION_ENV" ] || { echo "missing $PROVISION_ENV" >&2; exit 2; }
# shellcheck source=ci/lib.sh
source ci/lib.sh

repo=images/servarr-bootstrap
if [ ! -d "$repo" ]; then
  echo "the vendored bootstrap source is missing ($repo)" >&2
  exit 2
fi
chart="charts/apps/servarr"
[ -f "$chart/Chart.yaml" ] || { echo "no chart in $chart" >&2; exit 2; }

ver="$(cat "$repo/VERSION")"
sha="$(git rev-parse --short HEAD)"
# Import under the same locally built reference the console uses so
# Never/IfNotPresent finds it without a registry. The console uses the same
# tag scheme (VERSION + the platform commit).
tag="localhost/umkim-thin/servarr-bootstrap:${ver}-${sha}"

echo ">> image $tag (from $repo)"
if podman image exists "$tag" 2>/dev/null; then
  echo "   already built"
else
  podman build -t "$tag" "$repo"
fi

echo ">> importing into $TARGET's containerd"
# The Job has no node affinity, so the image must exist on every node of the
# target (HA would otherwise schedule it somewhere without the image).
tar=/tmp/servarr-bootstrap-image.tar
podman save -o "$tar" "$tag"
for h in ${SERVERS:-$VHOST}; do
  ssh -i "$KEY" -p "$JUMP_PORT" -o StrictHostKeyChecking=no -o BatchMode=yes "$JUMP" \
    "ssh -o StrictHostKeyChecking=no -o BatchMode=yes ${VUSER}@${h} 'sudo /usr/local/bin/k3s ctr -n k8s.io images import -'" \
    <"$tar" >/dev/null
  echo "   imported on $h"
done
rm -f "$tar"

echo ">> passwords + API keys in the servarr-bootstrap Secret"
node_root <<EOS
set -euo pipefail
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
NS=servarr
# random, run-scoped passwords (the bootstrap sets them in the apps); an
# existing Secret is preserved so a CI rerun does not reset the apps.
gen() { head -c 18 /dev/urandom | base64 | tr -d '/+=' | head -c 24; }
get() { \$K -n \$NS get secret servarr-bootstrap -o jsonpath="{.data.\$1}" 2>/dev/null | base64 -d 2>/dev/null || true; }
qbtuser="\$(get QBITTORRENT_USERNAME)"; [ -n "\$qbtuser" ] || qbtuser=admin
arruser="\$(get SERVARR_USERNAME)"; [ -n "\$arruser" ] || arruser=admin
jellyuser="\$(get JELLYFIN_ADMIN_USER)"; [ -n "\$jellyuser" ] || jellyuser=jellyfin-admin
qbt="\$(get QBITTORRENT_PASSWORD)"; [ -n "\$qbt" ] || qbt="\$(gen)"
arr="\$(get SERVARR_PASSWORD)"; [ -n "\$arr" ] || arr="\$(gen)"
jelly="\$(get JELLYFIN_ADMIN_PASSWORD)"; [ -n "\$jelly" ] || jelly="\$(gen)"
# the chart owns the *arr API keys
sonarr=\$(\$K -n \$NS get secret servarr-api-keys -o jsonpath='{.data.sonarr-api-key}' | base64 -d)
radarr=\$(\$K -n \$NS get secret servarr-api-keys -o jsonpath='{.data.radarr-api-key}' | base64 -d)
prowlarr=\$(\$K -n \$NS get secret servarr-api-keys -o jsonpath='{.data.prowlarr-api-key}' | base64 -d)
{
  echo "QBITTORRENT_USERNAME=\$qbtuser"
  echo "QBITTORRENT_PASSWORD=\$qbt"
  echo "SERVARR_USERNAME=\$arruser"
  echo "SERVARR_PASSWORD=\$arr"
  # A local admin with a name distinct from the directory's: Jellyfin resolves a
  # local account before an LDAP one, so "admin" would shadow the directory user.
  echo "JELLYFIN_ADMIN_USER=\$jellyuser"
  echo "JELLYFIN_ADMIN_PASSWORD=\$jelly"
  echo "SONARR_API_KEY=\$sonarr"
  echo "RADARR_API_KEY=\$radarr"
  echo "PROWLARR_API_KEY=\$prowlarr"
} > /tmp/servarr-bootstrap.env
\$K -n \$NS create secret generic servarr-bootstrap --from-env-file=/tmp/servarr-bootstrap.env \
  --dry-run=client -o yaml | \$K apply -f - >/dev/null
rm -f /tmp/servarr-bootstrap.env
echo "   secret servarr-bootstrap updated"
EOS

echo ">> restart qBittorrent so its init container writes the new WebUI password"
node_root <<'EOS'
set -euo pipefail
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
$K -n servarr rollout restart deploy/qbittorrent >/dev/null
$K -n servarr rollout status deploy/qbittorrent --timeout=300s
EOS

echo ">> enabling the bootstrap Job on the release"
# Package the chart here and push it: in cluster mode the console keeps the chart
# mirror inside its PVC (not on the host), and --reuse-values avoids depending on
# where the installer wrote the values file.
pkgdir="$(mktemp -d)"
helm package "$chart" -d "$pkgdir" >/dev/null
pkg="$pkgdir/servarr-${ver}.tgz"
pkg_b64="$(base64 -w0 "$pkg")"
rm -rf "$pkgdir"
node_root <<EOS
set -euo pipefail
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
H=/usr/local/bin/helm
NS=servarr
echo '${pkg_b64}' | base64 -d > /tmp/servarr-${ver}.tgz
# A killed run can leave a pending operation, which blocks every later upgrade.
st=\$("\$H" -n \$NS status servarr -o json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["info"]["status"])' 2>/dev/null || true)
case "\$st" in
  pending-*) echo "   clearing a \$st release"; "\$H" -n \$NS rollback servarr >/dev/null 2>&1 || true ;;
esac
\$H upgrade servarr /tmp/servarr-${ver}.tgz -n \$NS \
  --kubeconfig /etc/rancher/k3s/k3s.yaml \
  --reuse-values \
  --set bootstrap.enabled=true \
  --set bootstrap.image=${tag} \
  --set bootstrap.secretName=servarr-bootstrap \
  --set bootstrap.mediaLanguage=hu \
  --set bootstrap.seerrAutoApprove=true \
  --wait --timeout 15m 2>&1 | tail -4
EOS

echo ">> waiting for the Job"
node_root <<'EOS' || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K=/usr/local/bin/kubectl
for i in $(seq 1 90); do
  st=$($K -n servarr get job servarr-bootstrap -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)
  case "$st" in
    *True*) [ "$($K -n servarr get job servarr-bootstrap -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}')" = "True" ] && break || { echo "   bootstrap completed"; break; } ;;
  esac
  sleep 10
done
$K -n servarr get job servarr-bootstrap 2>/dev/null || true
echo "--- last log lines"
$K -n servarr logs job/servarr-bootstrap --tail=15 2>&1 | tail -15
EOS
