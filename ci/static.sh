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
echo "   ok"

echo ">> identity does not shadow ACME with an HTTP ingress"
identity_http=$(helm template identity charts/identity -f "$identity_values" \
  --set lldap.exposeWebUI=true --set lldap.webUIHost=lldap.example.com \
  --set traefik.portalEntrypoint=websecure 2>/dev/null)
if printf '%s\n' "$identity_http" | grep -F 'name: lldap-portal-http' >/dev/null; then
  echo "the LLDAP HTTP ingress is rendered with the portal on websecure; it shadows /\\.well-known/acme-challenge/" >&2
  exit 1
fi
identity_web=$(helm template identity charts/identity -f "$identity_values" \
  --set lldap.exposeWebUI=true --set lldap.webUIHost=lldap.example.com \
  --set traefik.portalEntrypoint=web 2>/dev/null)
printf '%s\n' "$identity_web" | grep -F 'name: lldap-portal-http' >/dev/null || {
  echo "the LLDAP HTTP ingress is missing when the portal is explicitly on web" >&2
  exit 1
}
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
         "serviceAccountName: umkim-thin-console"; do
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
echo "   ok"

echo ">> no floating image tags"
if grep -RIn -E 'image:.*:(latest|release|stable)([[:space:]]|$)|tag:[[:space:]]*"?(latest|release|stable)' charts catalog; then
  echo "floating image tag found; pin an exact version or digest" >&2
  exit 1
fi
echo "   ok"

echo ">> app charts"
for d in charts/apps/*; do
  [ -d "$d" ] || continue
  helm lint "$d" >/dev/null || { echo "helm lint $d failed" >&2; exit 1; }
done
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
for f in ci/*.sh deploy/provision.sh install.sh install-ha.sh verify-ha.sh teardown.sh uninstall-ha.sh tests/e2e/*.sh tests/e2e/migration/*.sh; do
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
