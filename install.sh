#!/usr/bin/env bash
# umkim-thin — one-command install on a single Rocky Linux 9/10 host.
#
#   sudo ./install.sh --zone example.com --email you@example.com
#   sudo ./install.sh --zone example.com --email you@example.com --image <ref>
#   sudo ./install.sh --backup
#   sudo ./install.sh --doctor
#
# The script is idempotent: re-running it upgrades the charts and keeps the
# generated secrets. It never regenerates a secret that already exists.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$SCRIPT_DIR"
VERSION="0.1.0"
ZONE=""
EMAIL=""
LAN_CIDR="${UMKIM_THIN_LAN_CIDR:-192.168.1.0/24}"
IMAGE=""
IMAGE_PULL_POLICY=""
IMAGE_TAR=""
BUILD_IMAGE=1
DRY_RUN=0
RENDER_ONLY=0
ASSUME_YES=0
ACME_STAGING=0
SKIP_PREFLIGHT=0
SKIP_DNS_CHECK=0
SKIP_PORT_CHECK=0
REUSE_K3S=0
QUIET=0
OUT=""
MODE="install"

DATA_DIR="${UMKIM_THIN_DATA_DIR:-/var/lib/umkim-thin}"
STATE_DIR="${UMKIM_THIN_STATE_DIR:-/etc/umkim-thin}"
HELM_VERSION="3.16.3"
K3S_CHANNEL="${K3S_CHANNEL:-stable}"
WG_PORT="${WG_PORT:-51820}"
WG_ENDPOINT="${WG_ENDPOINT:-}"
HELM=""
BASE_DN="dc=umkim,dc=thin"
CONSOLE_NAMESPACE="umkim-thin"
IDENTITY_NAMESPACE="identity"

log()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
umkim-thin installer — single-node Rocky Linux 9/10

Usage:
  sudo ./install.sh --zone <domain> --email <address> [options]
  sudo ./install.sh --backup [--out <dir>]
  sudo ./install.sh --doctor [--insecure]
  sudo ./install.sh --uninstall [--purge-data]   (runs teardown.sh)

Required for an install:
  --zone <domain>       Public zone; auth.<zone>, console.<zone> and apps use it.
  --email <address>     ACME account email (also the directory admin email).

Options:
  --build-image         Build the console image from this checkout (the default;
                        needs podman or docker).
  --image <ref>         Use an already-available console image instead.
  --image-tar <file>    Import an image archive into k3s; pass --image with the
                        archive's tag (default localhost/umkim-thin:novice).
  --acme-staging        Use the Let's Encrypt staging directory (testing only).
  --reuse-k3s           Use an existing k3s installation.
  --skip-preflight      Skip OS/resource/command checks (not recommended).
  --skip-dns-check      Do not require auth.<zone>/console.<zone> to resolve.
  --skip-port-check     Do not require ports 80/443/6443/51820 to be free.
  --dry-run             Check and print the plan; make no changes.
  --render-only         Write the generated values to --out (no root, no changes).
  --out <dir>           Output directory for --render-only or --backup.
  --quiet               Do not print generated passwords at the end.
  -y, --yes             Do not ask for confirmation.
  -h, --help            This help.
  --version             Print the installer version.

Examples:
  sudo ./install.sh --zone example.com --email admin@example.com
  sudo ./install.sh --render-only --zone example.com --email admin@example.com --out /tmp/umkim
  sudo ./install.sh --backup --out /var/backups/umkim-thin
  sudo ./install.sh --doctor
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --zone)            ZONE="${2:?--zone needs a value}"; shift 2 ;;
    --zone=*)          ZONE="${1#*=}"; shift ;;
    --email)           EMAIL="${2:?--email needs a value}"; shift 2 ;;
    --email=*)         EMAIL="${1#*=}"; shift ;;
    --image)           IMAGE="${2:?--image needs a value}"; BUILD_IMAGE=0; shift 2 ;;
    --image=*)         IMAGE="${1#*=}"; BUILD_IMAGE=0; shift ;;
    --image-tar)       IMAGE_TAR="${2:?--image-tar needs a value}"; BUILD_IMAGE=0; shift 2 ;;
    --image-tar=*)     IMAGE_TAR="${1#*=}"; BUILD_IMAGE=0; shift ;;
    --out)             OUT="${2:?--out needs a value}"; shift 2 ;;
    --out=*)           OUT="${1#*=}"; shift ;;
    --build-image)     BUILD_IMAGE=1; shift ;;
    --acme-staging)    ACME_STAGING=1; shift ;;
    --reuse-k3s)       REUSE_K3S=1; shift ;;
    --skip-preflight)  SKIP_PREFLIGHT=1; shift ;;
    --skip-dns-check)  SKIP_DNS_CHECK=1; shift ;;
    --skip-port-check) SKIP_PORT_CHECK=1; shift ;;
    --dry-run)         DRY_RUN=1; shift ;;
    --render-only)     RENDER_ONLY=1; shift ;;
    --version)         printf 'umkim-thin installer %s\n' "$VERSION"; exit 0 ;;
    --quiet)           QUIET=1; shift ;;
    -y|--yes)          ASSUME_YES=1; shift ;;
    --insecure)        INSECURE=1; shift ;;
    --backup)          MODE="backup"; shift ;;
    --doctor)          MODE="doctor"; shift ;;
    --uninstall)       MODE="uninstall"; shift ;;
    --purge-data)      PURGE_DATA=1; shift ;;
    -h|--help)         usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

validate_inputs() {
  if [ "$RENDER_ONLY" -eq 1 ] || [ "$MODE" = "install" ]; then
    [ -f charts/apps/gitea/Chart.yaml ] || \
      die "charts/apps submodule is missing; run: git submodule update --init --recursive"
    [ -n "$ZONE" ] || die "--zone is required (for example --zone example.com)"
    [ -n "$EMAIL" ] || die "--email is required (the ACME account and directory admin email)"
    ZONE=$(printf '%s' "$ZONE" | tr '[:upper:]' '[:lower:]' | sed 's/\.$//')
    case "$ZONE" in
      *[!a-z0-9.-]*|.*|*.-*|*-.*|*..*) die "invalid --zone: $ZONE" ;;
    esac
    case "$ZONE" in
      *.*) ;;
      *) die "--zone must be a domain with a dot, for example example.com" ;;
    esac
    case "$EMAIL" in
      *@*.*) ;;
      *) die "invalid --email: $EMAIL" ;;
    esac
  fi
  if [ "$MODE" = "install" ]; then
    if [ -z "$IMAGE" ]; then
      if [ "$BUILD_IMAGE" -eq 1 ] || [ -n "$IMAGE_TAR" ]; then
        IMAGE="localhost/umkim-thin:novice"
      else
        die "--image is required when image building is disabled"
      fi
    fi
    if [ -z "$IMAGE_PULL_POLICY" ]; then
      if [ "$BUILD_IMAGE" -eq 1 ] || [ -n "$IMAGE_TAR" ]; then
        IMAGE_PULL_POLICY="Never"
      else
        IMAGE_PULL_POLICY="IfNotPresent"
      fi
    fi
  fi
  WG_ENDPOINT="${WG_ENDPOINT:-$ZONE}"
}

need_root() {
  [ "$(id -u)" -eq 0 ] || die "run as root (sudo ./install.sh ...)"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

check_os() {
  [ -r /etc/os-release ] || die "/etc/os-release is missing; this installer supports Rocky Linux 9/10"
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID:-}" in
    rocky) ;;
    *) die "unsupported OS '${ID:-unknown}'; this path supports Rocky Linux 9/10 (the Ansible path covers other RHEL-family hosts)" ;;
  esac
  case "${VERSION_ID:-}" in
    9|9.*|10|10.*) ;;
    *) warn "Rocky Linux ${VERSION_ID:-unknown} is not a tested release; 9 and 10 are supported" ;;
  esac
  info "OS: ${PRETTY_NAME:-$ID}"
}

check_arch() {
  case "$(uname -m)" in
    x86_64|aarch64) info "architecture: $(uname -m)" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

check_resources() {
  local mem_kb disk_kb
  mem_kb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
  [ "${mem_kb:-0}" -ge 3500000 ] || die "at least 4 GB of RAM is required (found $((mem_kb / 1024)) MB)"
  disk_kb=$(df -Pk /var | awk 'NR==2{print $4}')
  [ "${disk_kb:-0}" -ge 15000000 ] || die "at least 15 GB free on /var is required (found $((disk_kb / 1024)) MB)"
  info "RAM: $((mem_kb / 1024)) MB; free on /var: $((disk_kb / 1024)) MB"
}

check_commands() {
  local missing=""
  for c in curl openssl tar gzip ss hostname; do
    command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
  done
  if [ -n "$missing" ]; then
    info "installing:$missing"
    if [ "$DRY_RUN" -eq 0 ]; then
      command -v dnf >/dev/null 2>&1 || die "dnf not found; this installer targets Rocky Linux"
      # iproute provides ss; ca-certificates is needed for ACME.
      dnf -y install curl ca-certificates openssl tar gzip iproute hostname >/dev/null
    fi
  fi
}

prepare_host() {
  log "preparing the Rocky Linux host"
  if [ "$DRY_RUN" -eq 0 ]; then
    if [ -n "$(swapon --show --noheadings 2>/dev/null)" ]; then
      swapoff -a
      info "swap disabled for this boot"
    fi
    if grep -qE '^[^#].*[[:space:]]swap[[:space:]]' /etc/fstab 2>/dev/null; then
      sed -i.bak -E 's/^([^#].*[[:space:]]swap[[:space:]].*)$/#\1/' /etc/fstab
      info "swap entries commented out in /etc/fstab"
    fi
  else
    info "+ swapoff -a; comment swap entries in /etc/fstab"
  fi
  if [ "$DRY_RUN" -eq 0 ]; then
    printf 'net.ipv4.ip_forward = 1\n' > /etc/sysctl.d/99-k3s.conf
    sysctl --system >/dev/null 2>&1 || warn "could not apply sysctl settings"
  else
    info "+ net.ipv4.ip_forward=1"
  fi
  if systemctl cat chronyd.service >/dev/null 2>&1; then
    run systemctl enable --now chronyd >/dev/null 2>&1 || warn "chronyd did not start"
    # A VM restored from a snapshot boots with a stale clock; ACME and Authelia
    # sessions both care. Step it as soon as chrony has a source (the command is
    # harmless when it does not).
    if command -v chronyc >/dev/null 2>&1; then
      run chronyc makestep >/dev/null 2>&1 || true
    fi
  fi
  local u
  for u in nm-cloud-setup.service nm-cloud-setup.timer; do
    if systemctl cat "$u" >/dev/null 2>&1; then
      run systemctl disable --now "$u" >/dev/null 2>&1 || true
    fi
  done
  if [ "$DRY_RUN" -eq 0 ]; then
    dnf -y install iptables iptables-nft conntrack-tools >/dev/null ||
      warn "could not install iptables/conntrack"
  fi
  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" = "Enforcing" ]; then
    if [ "$DRY_RUN" -eq 0 ]; then
      dnf -y install container-selinux policycoreutils-python-utils chrony >/dev/null ||
        warn "could not install the host tooling"
      if ! rpm -q k3s-selinux >/dev/null 2>&1; then
        cat > /etc/yum.repos.d/rancher-k3s-common.repo <<'REPO'
[rancher-k3s-common-stable]
name=Rancher K3s Common (stable)
baseurl=https://rpm.rancher.io/k3s/stable/common/centos/9/noarch
enabled=1
gpgcheck=1
repo_gpgcheck=0
gpgkey=https://rpm.rancher.io/public.key
REPO
        if ! dnf -y install k3s-selinux >/dev/null; then
          warn "k3s-selinux could not be installed; SELinux may block pods"
        fi
      fi
    else
      info "+ install container-selinux, policycoreutils-python-utils and k3s-selinux"
    fi
  else
    info "SELinux is not enforcing; skipping the k3s SELinux policy"
  fi
  if [ "$DRY_RUN" -eq 0 ] &&
     ! find "/lib/modules/$(uname -r)" -name 'xt_mark.ko*' -print -quit 2>/dev/null | grep -q .; then
    die "kernel $(uname -r) has no xt_mark module; install a stock kernel (sudo dnf install kernel kernel-modules), reboot, and rerun"
  fi
}

port_in_use() {
  ss -lntu 2>/dev/null | awk '{print $5}' | grep -E "[:.]$1\$" >/dev/null
}

check_ports() {
  if [ "$REUSE_K3S" -eq 1 ]; then
    info "port checks skipped (--reuse-k3s)"
    return 0
  fi
  local p
  for p in 80 443 6443 "$WG_PORT"; do
    if port_in_use "$p"; then
      if [ "$p" = "6443" ] && [ "$REUSE_K3S" -eq 1 ]; then
        info "port $p is in use (existing k3s)"
        continue
      fi
      die "port $p is already in use; stop the service or pass --skip-port-check"
    fi
  done
  info "ports 80, 443, 6443 and ${WG_PORT}/udp are free"
}

check_dns() {
  local h
  for h in "auth.$ZONE" "console.$ZONE" "lldap.$ZONE"; do
    if ! getent ahostsv4 "$h" >/dev/null 2>&1; then
      die "$h does not resolve. Create the DNS records first (a wildcard covers lldap and apps), or pass --skip-dns-check"
    fi
  done
  info "DNS: auth.$ZONE, console.$ZONE and lldap.$ZONE resolve"
}

preflight() {
  [ "$SKIP_PREFLIGHT" -eq 1 ] && { warn "preflight skipped"; return 0; }
  check_os
  check_arch
  check_resources
  check_commands
  [ "$SKIP_PORT_CHECK" -eq 1 ] || check_ports
  [ "$SKIP_DNS_CHECK" -eq 1 ] || check_dns
  if [ -f /etc/rancher/k3s/k3s.yaml ] && [ "$REUSE_K3S" -eq 0 ]; then
    die "k3s is already installed; pass --reuse-k3s to use it, or remove it first"
  fi
}

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '    + %s\n' "$*"
    return 0
  fi
  "$@"
}

kubectl() {
  local kc="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
  if [ -x /usr/local/bin/kubectl ]; then
    KUBECONFIG="$kc" /usr/local/bin/kubectl "$@"
  elif bin=$(type -P kubectl) && [ -n "$bin" ]; then
    KUBECONFIG="$kc" "$bin" "$@"
  else
    KUBECONFIG="$kc" /usr/local/bin/k3s kubectl "$@"
  fi
}

have_k3s() {
  [ -x /usr/local/bin/k3s ] || command -v k3s >/dev/null 2>&1
}

install_k3s() {
  if [ "$REUSE_K3S" -eq 1 ] && have_k3s; then
    log "using the existing k3s"
  else
    log "installing k3s"
    if [ "$DRY_RUN" -eq 0 ]; then
      curl -sfL https://get.k3s.io |
        INSTALL_K3S_CHANNEL="$K3S_CHANNEL" \
        INSTALL_K3S_EXEC="server --cluster-init --write-kubeconfig-mode 600 --tls-san auth.$ZONE --tls-san console.$ZONE" \
        sh -
    else
      info "+ install k3s (channel $K3S_CHANNEL, embedded etcd, TLS SANs auth.$ZONE and console.$ZONE)"
    fi
  fi
  [ "$DRY_RUN" -eq 1 ] && return 0
  local i
  for i in $(seq 1 90); do
    if kubectl get nodes >/dev/null 2>&1; then break; fi
    sleep 2
    [ "$i" -eq 90 ] && die "k3s did not become ready"
  done
  kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
  # sudo's secure PATH on Rocky/RHEL omits /usr/local/bin; expose the k3s
  # symlink through /usr/bin so admin commands can use `sudo kubectl`.
  if [ -x /usr/local/bin/kubectl ]; then
    if [ ! -e /usr/bin/kubectl ] ||
       [ "$(readlink /usr/bin/kubectl 2>/dev/null || true)" = "/usr/local/bin/kubectl" ]; then
      ln -sfn /usr/local/bin/kubectl /usr/bin/kubectl
    fi
  fi
  mkdir -p "$STATE_DIR"
  chmod 700 "$STATE_DIR"
  cp -f /etc/rancher/k3s/k3s.yaml "$STATE_DIR/kubeconfig"
  chmod 600 "$STATE_DIR/kubeconfig"
  info "k3s is ready"
}

install_helm() {
  if [ -x /usr/local/bin/helm ]; then
    HELM=/usr/local/bin/helm
    info "helm: $("$HELM" version --short 2>/dev/null || echo present)"
    return 0
  fi
  if command -v helm >/dev/null 2>&1; then
    HELM=$(type -P helm)
    info "helm: $("$HELM" version --short 2>/dev/null || echo present)"
    return 0
  fi
  log "installing helm $HELM_VERSION"
  local arch tmp tarball
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) die "unsupported architecture" ;;
  esac
  tarball="helm-v${HELM_VERSION}-linux-${arch}.tar.gz"
  tmp=$(mktemp -d)
  run curl -fsSL "https://get.helm.sh/$tarball" -o "$tmp/$tarball"
  run curl -fsSL "https://get.helm.sh/$tarball.sha256sum" -o "$tmp/$tarball.sha256"
  if [ "$DRY_RUN" -eq 0 ]; then
    (cd "$tmp" && sha256sum -c "$tarball.sha256")
    tar -xzf "$tmp/$tarball" -C "$tmp"
    install -m 0755 "$tmp/linux-${arch}/helm" /usr/local/bin/helm
    HELM=/usr/local/bin/helm
  fi
  rm -rf "$tmp"
}

rand() { openssl rand -base64 "${1:-32}" | tr -d '\n'; }

generate_secrets() {
  mkdir -p "$STATE_DIR"
  chmod 700 "$STATE_DIR"
  if [ "$RENDER_ONLY" -eq 1 ]; then
    SECRET_LLDAP_ADMIN="ci-only-lldap-admin"
    SECRET_LLDAP_JWT="ci-only-lldap-jwt"
    SECRET_LLDAP_BIND="ci-only-lldap-bind"
    SECRET_AUTHELIA_SESSION="ci-only-authelia-session"
    SECRET_AUTHELIA_STORAGE="ci-only-authelia-storage"
    SECRET_AUTHELIA_RESET="ci-only-authelia-reset"
    return 0
  fi
  if [ ! -f "$STATE_DIR/secrets.yml" ]; then
    log "generating directory, portal and console secrets (first run only)"
    umask 077
    cat > "$STATE_DIR/secrets.yml" <<EOF
secret_lldap_admin_password: "$(rand 24)"
secret_lldap_jwt: "$(rand 32)"
secret_lldap_bind: "$(rand 24)"
secret_authelia_session: "$(rand 32)"
secret_authelia_storage: "$(rand 32)"
secret_authelia_reset: "$(rand 32)"
secret_console_localadmin_password: "$(openssl rand -base64 18 | tr -d '=+/')"
EOF
  fi
  chmod 600 "$STATE_DIR/secrets.yml"
  SECRET_LLDAP_ADMIN=$(secret_value secret_lldap_admin_password)
  SECRET_LLDAP_JWT=$(secret_value secret_lldap_jwt)
  SECRET_LLDAP_BIND=$(secret_value secret_lldap_bind)
  SECRET_AUTHELIA_SESSION=$(secret_value secret_authelia_session)
  SECRET_AUTHELIA_STORAGE=$(secret_value secret_authelia_storage)
  SECRET_AUTHELIA_RESET=$(secret_value secret_authelia_reset)
  [ -n "$SECRET_LLDAP_ADMIN" ] || die "secrets.yml has no secret_lldap_admin_password"
  [ -f "$STATE_DIR/console-session-key" ] || head -c 32 /dev/urandom | base64 | tr -d '\n' > "$STATE_DIR/console-session-key"
  [ -f "$STATE_DIR/authelia-oidc-hmac" ] || head -c 32 /dev/urandom | base64 | tr -d '\n' > "$STATE_DIR/authelia-oidc-hmac"
  [ -f "$STATE_DIR/authelia-oidc-key.pem" ] || openssl genrsa -out "$STATE_DIR/authelia-oidc-key.pem" 2048 2>/dev/null
  [ -f "$STATE_DIR/identity-apps.yml" ] || printf '{}\n' > "$STATE_DIR/identity-apps.yml"
  chmod 600 "$STATE_DIR"/console-session-key "$STATE_DIR"/authelia-oidc-hmac "$STATE_DIR"/authelia-oidc-key.pem "$STATE_DIR"/identity-apps.yml
}

secret_value() {
  sed -n "s/^$1: \"\\(.*\\)\"/\\1/p" "$STATE_DIR/secrets.yml" | head -1
}

identity_version() {
  awk '/^version:/{print $2; exit}' charts/identity/Chart.yaml
}

write_identity_values() {
  cat > "$STATE_DIR/identity-values.yml" <<EOF
# Generated by install.sh. Contains secrets; keep it 0600.
namespace: $IDENTITY_NAMESPACE
createNamespace: false

cluster:
  dnsServiceIP: 10.43.0.10

persistence:
  storageClass: local-path

lldap:
  baseDN: "$BASE_DN"
  adminEmail: "$EMAIL"
  adminPassword: "$SECRET_LLDAP_ADMIN"
  jwtSecret: "$SECRET_LLDAP_JWT"
  # Public but only behind the portal: the household admin uses it for the
  # occasional password reset or removal. The wildcard DNS record covers it.
  exposeWebUI: true
  webUIHost: "lldap.$ZONE"
  bindAccount:
    enabled: true
    password: "$SECRET_LLDAP_BIND"

valkey:
  # Reuse the generated Authelia session secret as the internal Valkey
  # password; it is already rendered into the identity Secret.
  password: "$SECRET_AUTHELIA_SESSION"

authelia:
  portalHost: "auth.$ZONE"
  publicURL: "https://auth.$ZONE"
  cookieDomain: "$ZONE"
  sessionSecret: "$SECRET_AUTHELIA_SESSION"
  storageEncryptionKey: "$SECRET_AUTHELIA_STORAGE"
  resetPasswordJWTSecret: "$SECRET_AUTHELIA_RESET"
  accessDomains:
    - "*.$ZONE"
    - "$ZONE"

traefik:
  deployMiddleware: true
  middlewareNamespace: kube-system
  portalEntrypoint: websecure
  portalTLS: true
  portalCertResolver: letsencrypt
  vpnSourceRanges:
    - "192.168.168.0/24"
  vpnLocalSourceRanges:
    - "192.168.168.0/24"
    - "$LAN_CIDR"
    - "127.0.0.1/32"
    # NodePort traffic can be SNATed to the node/CNI networks.
    - "10.42.0.0/16"
    - "10.43.0.0/16"

signup:
  enabled: true
  redeemURL: "http://umkim-thin-console.umkim-thin.svc.cluster.local:8080/api/invites/redeem"
EOF
  chmod 600 "$STATE_DIR/identity-values.yml"
}

write_console_values() {
  cat > "$STATE_DIR/console-values.yml" <<EOF
# Generated by install.sh.
image: "$IMAGE"
imagePullPolicy: "$IMAGE_PULL_POLICY"
consoleHost: "console.$ZONE"
forwardAuthMiddleware: identity-authelia@file
persistence:
  storageClass: local-path
  size: 2Gi
configMapName: umkim-thin-config
seedSecretName: umkim-thin-secrets
mirrorConfigMap: umkim-thin-mirror
storageSocket: /run/umkim-thin
podSecurity:
  seLinuxType: ""
EOF
  chmod 644 "$STATE_DIR/console-values.yml"
}

write_config_env() {
  local identity_tgz
  identity_tgz="identity-$(identity_version).tgz"
  cat > "$STATE_DIR/config.env" <<EOF
# Generated by install.sh. Read by the console in the cluster.
UMKIM_THIN_KUBECONFIG=/tmp/kubeconfig
UMKIM_THIN_STATE_DIR=/data
UMKIM_THIN_CHART_MIRROR=/data/chart-mirror
UMKIM_THIN_ZONE=$ZONE
UMKIM_THIN_PORTAL_HOST=auth.$ZONE
UMKIM_THIN_TENANT=default-tenant
UMKIM_THIN_STORAGE_CONFIG_CLASS=local-path
UMKIM_THIN_STORAGE_MEDIA_CLASS=local-path
UMKIM_THIN_STORAGE_MEDIA_ACCESS_MODE=ReadWriteOnce
UMKIM_THIN_SERVICE_CIDR=10.43.0.0/16
UMKIM_THIN_POD_CIDR=10.42.0.0/16
UMKIM_THIN_DNS_IP=10.43.0.10
UMKIM_THIN_MIDDLEWARE=identity-authelia@file
UMKIM_THIN_IDENTITY_NAMESPACE=$IDENTITY_NAMESPACE
UMKIM_THIN_IDENTITY_CHART=/data/chart-mirror/$identity_tgz
UMKIM_THIN_IDENTITY_VALUES=/data/identity-values.yml
UMKIM_THIN_IDENTITY_APPS=/data/identity-apps.yml
UMKIM_THIN_BIND_SECRET=lldap-service-accounts
UMKIM_THIN_BASE_DN=$BASE_DN
UMKIM_THIN_LLDAP_HOST=lldap.identity.svc.cluster.local
UMKIM_THIN_LLDAP_PORT=3890
UMKIM_THIN_SECRETS_FILE=/data/secrets.yml
UMKIM_THIN_CONSOLE_SESSION_KEY_FILE=/data/console-session-key
UMKIM_THIN_INVITES=/data/invites.json
UMKIM_THIN_WG_STATE=/data/wg-clients.json
UMKIM_THIN_APPS_STATE=/data/apps-state.json
UMKIM_THIN_HELM=/usr/local/bin/helm
UMKIM_THIN_KUBECTL=/usr/local/bin/kubectl
UMKIM_THIN_CONSOLE_IMAGE=$IMAGE
UMKIM_THIN_BOOTSTRAP_IMAGE=localhost/umkim-thin/servarr-bootstrap
UMKIM_THIN_BOOTSTRAP_CONTEXT=/opt/umkim-thin/bootstrap
UMKIM_THIN_BUILDER_IMAGE=gcr.io/kaniko-project/executor:v1.23.2-debug@sha256:fb6d29da5792b6b9886bb132d1bb600bec79a47a31e4c152df3e52de9c561a44
UMKIM_THIN_BUILD_NAMESPACE=umkim-thin-build
UMKIM_THIN_BOOTSTRAP_NODE=$(hostname)
UMKIM_THIN_CONSOLE_ADMIN_USER=admin
UMKIM_THIN_CONSOLE_LOCAL_USER=localadmin
UMKIM_THIN_CONSOLE_LOCAL_PASSWORD_FILE=/data/console-localadmin-password
UMKIM_THIN_WG_NAMESPACE=wireguard
UMKIM_THIN_WG_SECRET=wireguard-peers
UMKIM_THIN_WG_SUBNET=192.168.168.0/24
UMKIM_THIN_WG_SERVER_IP=192.168.168.1
UMKIM_THIN_WG_PORT=$WG_PORT
UMKIM_THIN_WG_ENDPOINT=$WG_ENDPOINT:$WG_PORT
UMKIM_THIN_WG_ALLOWED_IPS=10.42.0.0/16,10.43.0.0/16,192.168.168.0/24
EOF
  chmod 644 "$STATE_DIR/config.env"
}

write_traefik_config() {
  local acme_extra=""
  if [ "$ACME_STAGING" -eq 1 ]; then
    acme_extra='      - "--certificatesresolvers.letsencrypt.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory"'
  fi
  cat > "$STATE_DIR/traefik-config.yaml" <<EOF
# Generated by install.sh. k3s ships Traefik as a HelmChart; this config adds
# ACME, the file provider for the identity middleware, and a stable volume.
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    service:
      spec:
        type: LoadBalancer
    ports:
      web:
        # Pin the NodePorts the lab router forwards to (WAN 80/443 ->
        # 192.0.2.10:30080/30443). The Ansible path pins the same ports.
        nodePort: 30080
        allowACMEByPass: true
        http:
          redirections:
            entryPoint:
              to: websecure
              scheme: https
              permanent: true
      websecure:
        nodePort: 30443
      vpn:
        # Internal entrypoint for VPN/LAN-only routes. The public router does
        # not forward this NodePort; the host firewall only allows the LAN and
        # the WireGuard gateway. The route's IPAllowList is the second layer.
        port: 9443
        exposedPort: 9443
        nodePort: 30444
        protocol: TCP
        expose:
          default: true
      gitssh:
        port: 2222
        exposedPort: 2222
        protocol: TCP
        nodePort: 30222
        expose:
          default: true
    additionalArguments:
      - "--certificatesresolvers.letsencrypt.acme.email=$EMAIL"
      - "--certificatesresolvers.letsencrypt.acme.storage=/data/acme.json"
      - "--certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=web"
$acme_extra
      - "--providers.file.directory=/etc/traefik/dynamic"
      - "--providers.file.watch=true"
    persistence:
      enabled: true
      size: 128Mi
      storageClass: local-path
    volumes:
      - name: identity-traefik
        mountPath: /etc/traefik/dynamic
        type: configMap
    podSecurityContext:
      fsGroup: 65532
      fsGroupChangePolicy: OnRootMismatch
    deployment:
      strategy:
        type: Recreate
      initContainers:
        - name: volume-permissions
          image: busybox:1.36.1
          command: ["sh", "-c", "touch /data/acme.json && chmod 600 /data/acme.json"]
          volumeMounts:
            - name: data
              mountPath: /data
EOF
  chmod 644 "$STATE_DIR/traefik-config.yaml"
}

render_all() {
  generate_secrets
  write_identity_values
  write_console_values
  write_config_env
  write_traefik_config
}

package_charts() {
  log "packaging charts into $DATA_DIR/chart-mirror"
  mkdir -p "$DATA_DIR/chart-mirror"
  local d
  for d in charts/identity charts/umkim-thin charts/wireguard-gateway charts/apps/*; do
    [ -d "$d" ] || continue
    "$HELM" package "$d" --destination "$DATA_DIR/chart-mirror" >/dev/null
  done
  # Keep catalogued versions for upgrade/rollback. Pruning by age would delete
  # the N-1 chart that the catalog still advertises; prune explicitly instead.

  info "$(find "$DATA_DIR/chart-mirror" -name '*.tgz' | wc -l) charts packaged"
}

build_or_import_image() {
  if [ -n "$IMAGE_TAR" ]; then
    [ -f "$IMAGE_TAR" ] || die "image archive not found: $IMAGE_TAR"
    log "importing $IMAGE_TAR into k3s"
    run /usr/local/bin/k3s ctr images import "$IMAGE_TAR"
    return 0
  fi
  if [ "$BUILD_IMAGE" -eq 0 ]; then
    info "using image $IMAGE"
    return 0
  fi
  log "building the console image (this can take a few minutes)"
  local builder=""
  if command -v podman >/dev/null 2>&1; then builder=podman
  elif command -v docker >/dev/null 2>&1; then builder=docker
  else
    info "installing podman"
    run dnf -y install podman >/dev/null
    builder=podman
  fi
  run "$builder" build --build-arg "UMKIM_THIN_GIT_SHA=$(git rev-parse --short HEAD 2>/dev/null || echo novice)" -t "$IMAGE" .
  log "importing $IMAGE into k3s"
  if [ "$DRY_RUN" -eq 1 ]; then
    info "+ $builder save $IMAGE | k3s ctr images import -"
  else
    "$builder" save --format docker-archive "$IMAGE" | /usr/local/bin/k3s ctr images import -
  fi
}

install_storage_agent() {
  [ "$DRY_RUN" -eq 1 ] && return 0
  log "installing the host storage agent (single node)"
  run dnf -y install mdadm xfsprogs nfs-utils >/dev/null 2>&1 || warn "could not install storage packages"
  local builder="" tmp
  if command -v podman >/dev/null 2>&1; then builder=podman
  elif command -v docker >/dev/null 2>&1; then builder=docker
  fi
  if [ -n "$builder" ]; then
    tmp="umkim-thin-agent-extract-$$"
    if "$builder" create --name "$tmp" "$IMAGE" /bin/true >/dev/null 2>&1; then
      if "$builder" cp "$tmp:/usr/local/bin/umkim-thin" /usr/local/bin/umkim-thin >/dev/null 2>&1; then
        chmod 755 /usr/local/bin/umkim-thin
      else
        warn "could not extract the storage agent binary from the console image"
      fi
      "$builder" rm -f "$tmp" >/dev/null 2>&1 || true
    else
      warn "could not create a container to extract the storage agent binary"
    fi
  fi
  [ -x /usr/local/bin/umkim-thin ] || { warn "storage agent binary not installed; console storage stays unavailable"; return 0; }

  cat >/etc/systemd/system/umkim-thin-storage.service <<'UNIT'
[Unit]
Description=umkim-thin host storage agent
After=network-online.target
[Service]
Type=simple
ExecStart=/usr/local/bin/umkim-thin storage-agent --socket /run/umkim-thin/storage.sock --state /etc/umkim-thin/storage.yml
Restart=on-failure
RuntimeDirectory=umkim-thin
RuntimeDirectoryMode=0750
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl enable --now umkim-thin-storage.service >/dev/null 2>&1 || warn "could not start the storage agent"
  # The console runs as UID 65532 and needs to traverse the socket directory.
  chown root:65532 /run/umkim-thin 2>/dev/null || true
  chmod 0750 /run/umkim-thin 2>/dev/null || true
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd --permanent --zone=trusted --add-service=nfs >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
  fi
  info "storage agent: /run/umkim-thin/storage.sock"
}

seed_console() {
  log "seeding the console state"
  kubectl create namespace "$CONSOLE_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  local seed_args=(
    --from-file=secrets.yml="$STATE_DIR/secrets.yml"
    --from-file=identity-values.yml="$STATE_DIR/identity-values.yml"
    --from-file=identity-apps.yml="$STATE_DIR/identity-apps.yml"
    --from-file=console-session-key="$STATE_DIR/console-session-key"
    --from-file=authelia-oidc-hmac="$STATE_DIR/authelia-oidc-hmac"
    --from-file=authelia-oidc-key.pem="$STATE_DIR/authelia-oidc-key.pem"
  )
  local f
  for f in invites.json wg-clients.json apps-state.json console-localadmin-password; do
    [ -f "$STATE_DIR/$f" ] && seed_args+=(--from-file="$f=$STATE_DIR/$f")
  done
  kubectl -n "$CONSOLE_NAMESPACE" create secret generic umkim-thin-secrets \
    "${seed_args[@]}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$CONSOLE_NAMESPACE" create configmap umkim-thin-config \
    --from-file=config.env="$STATE_DIR/config.env" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  local tarfile
  tarfile=$(mktemp)
  tar cf "$tarfile" -C "$DATA_DIR/chart-mirror" .
  kubectl -n "$CONSOLE_NAMESPACE" create configmap umkim-thin-mirror \
    --from-file=mirror.tar="$tarfile" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  rm -f "$tarfile"
}

install_identity() {
  log "installing the identity stack (LLDAP + Authelia)"
  run "$HELM" upgrade --install identity charts/identity \
    --namespace "$IDENTITY_NAMESPACE" --create-namespace \
    --kubeconfig /etc/rancher/k3s/k3s.yaml \
    --values "$STATE_DIR/identity-values.yml" \
    --wait --timeout 10m
}

install_vpn_dns() {
  [ "$DRY_RUN" -eq 1 ] && return 0
  [ -n "$ZONE" ] || return 0
  local cm tmp
  cm=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' 2>/dev/null || true)
  [ -n "$cm" ] || { warn "coredns ConfigMap not found; VPN DNS not installed"; return 0; }
  if printf '%s' "$cm" | grep -q "vpn.$ZONE:53"; then
    info "VPN DNS zone vpn.$ZONE already present"
    return 0
  fi
  tmp=$(mktemp)
  {
    printf '%s

' "$cm"
    printf 'vpn.%s:53 {
' "$ZONE"
    printf '    errors
    cache 30
'
    printf '    template IN A vpn.%s {
' "$ZONE"
    printf '        match "^.*\\.vpn\\.%s\\.$"
' "$(printf '%s' "$ZONE" | sed 's/\./\\./g')"
    printf '        answer "{{ .Name }} 60 IN A 192.168.168.1"
'
    printf '    }
'
    printf '    template IN AAAA vpn.%s {
' "$ZONE"
    printf '        match "^.*\\.vpn\\.%s\\.$"
' "$(printf '%s' "$ZONE" | sed 's/\./\\./g')"
    printf '        rcode NXDOMAIN
'
    printf '    }
}
'
  } > "$tmp"
  if kubectl -n kube-system create configmap coredns --from-file=Corefile="$tmp" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; then
    info "VPN DNS zone vpn.$ZONE -> 192.168.168.1"
  else
    warn "could not update CoreDNS for the VPN DNS zone"
  fi
  rm -f "$tmp"
}

apply_traefik() {
  log "configuring Traefik (ACME + the identity middleware)"
  run kubectl apply -f "$STATE_DIR/traefik-config.yaml"
  if [ "$DRY_RUN" -eq 0 ]; then
    kubectl -n kube-system rollout status deploy/traefik --timeout=300s >/dev/null
  fi
  validate_traefik
}

# A router that forwards WAN 80/443 to the pinned NodePorts cannot work if
# Traefik picked different ports. Fail here instead of leaving the site refused
# and, worse, burning ACME failed-authorization limits on a hidden mismatch.
validate_traefik() {
  [ "$DRY_RUN" -eq 1 ] && return 0
  local i ports
  for i in $(seq 1 30); do
    ports=$(kubectl -n kube-system get svc traefik \
      -o jsonpath='{range .spec.ports[*]}{.port}:{.nodePort}{" "}{end}' 2>/dev/null || true)
    case "$ports" in
      *"80:30080"*"443:30443"*) info "traefik ports: $ports"; return 0 ;;
    esac
    sleep 2
  done
  die "traefik service ports are '${ports:-unknown}', expected 80:30080 and 443:30443; check the router mapping and the HelmChartConfig"
}

install_console() {
  log "installing the console"
  run "$HELM" upgrade --install umkim-thin charts/umkim-thin \
    --namespace "$CONSOLE_NAMESPACE" --create-namespace \
    --kubeconfig /etc/rancher/k3s/k3s.yaml \
    --values "$STATE_DIR/console-values.yml" \
    --wait --timeout 5m
  # The image tag and the chart spec do not change on a rebuild, so Helm will
  # not roll the pod. Force one so the new image and the re-seeded chart mirror
  # are actually picked up (the seed init extracts the mirror on start).
  if [ "$DRY_RUN" -eq 0 ] &&
     kubectl -n "$CONSOLE_NAMESPACE" get deploy umkim-thin-console >/dev/null 2>&1; then
    kubectl -n "$CONSOLE_NAMESPACE" rollout restart deploy/umkim-thin-console >/dev/null
    kubectl -n "$CONSOLE_NAMESPACE" rollout status deploy/umkim-thin-console --timeout=300s >/dev/null
  fi
}

open_firewall() {
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    log "opening 80/443, Traefik NodePorts, ${WG_PORT}/udp and the cluster ports in firewalld"
    local p
    for p in 80/tcp 443/tcp 2222/tcp 30080/tcp 30222/tcp 30443/tcp 30444/tcp "$WG_PORT"/udp 8472/udp 10250/tcp; do
      run firewall-cmd --permanent --add-port="$p" >/dev/null || warn "could not open $p"
    done
    run firewall-cmd --permanent --add-service=ssh >/dev/null || true
    run firewall-cmd --permanent --add-masquerade >/dev/null || true
    run firewall-cmd --permanent --zone=trusted --add-source=10.42.0.0/16 >/dev/null || true
    run firewall-cmd --permanent --zone=trusted --add-source=10.43.0.0/16 >/dev/null || true
    run firewall-cmd --permanent --zone=trusted --add-source=192.168.168.0/24 >/dev/null || true
    run firewall-cmd --permanent --zone=trusted --add-source="$LAN_CIDR" >/dev/null || true
    run firewall-cmd --reload >/dev/null || warn "firewalld reload failed"
    return 0
  fi
  info "firewalld is not running; no host firewall to configure"
}

print_summary() {
  local admin localpass
  admin=$(secret_value secret_lldap_admin_password 2>/dev/null || true)
  localpass=$(secret_value secret_console_localadmin_password 2>/dev/null || true)
  cat <<EOF

============================================================================
umkim-thin is installed.

  Portal     https://auth.$ZONE
  Console    https://console.$ZONE
  Home       https://home.$ZONE
  Directory  https://lldap.$ZONE  (admin / the password below)
  Breakglass localadmin / (the password printed by the installer)

EOF
  if [ "$QUIET" -eq 0 ] && [ "$RENDER_ONLY" -eq 0 ] && [ -t 1 ]; then
    printf '  Admin password:      %s\n' "${admin:-see $STATE_DIR/secrets.yml}"
    printf '  Breakglass password: %s\n' "${localpass:-see $STATE_DIR/secrets.yml}"
    cat <<EOF

  Save these now. They are also in $STATE_DIR/secrets.yml (mode 0600).
  The console is behind Authelia: sign in to the portal first, then the console.
  Next: docs/quickstart/02-first-login.md
============================================================================
EOF
  else
    cat <<EOF
  Secrets: $STATE_DIR/secrets.yml
  Next:    docs/quickstart/02-first-login.md
============================================================================
EOF
  fi
  cat <<EOF
  Show the directory admin password later:
    sudo sed -n 's/^secret_lldap_admin_password: "\\(.*\\)"/\\1/p' $STATE_DIR/secrets.yml
  Show the console breakglass password:
    sudo sed -n 's/^secret_console_localadmin_password: "\\(.*\\)"/\\1/p' $STATE_DIR/secrets.yml
============================================================================
EOF
}

do_backup() {
  need_root
  have_k3s || die "k3s is not installed"
  local dir="${OUT:-/var/backups/umkim-thin}"
  local stamp
  stamp=$(date +%Y%m%d-%H%M%S)
  mkdir -p "$dir"
  chmod 700 "$dir"
  log "saving an etcd snapshot"
  if ! /usr/local/bin/k3s etcd-snapshot save --name "umkim-thin-$stamp"; then
    warn "etcd snapshot failed (a sqlite-backed k3s cannot take one); archiving the datastore instead"
    tar czf "$dir/k3s-db-$stamp.tgz" -C /var/lib/rancher/k3s/server db
    chmod 600 "$dir/k3s-db-$stamp.tgz"
  fi
  log "archiving the console and identity state"
  tar czf "$dir/umkim-thin-state-$stamp.tgz" -C "$STATE_DIR" .
  chmod 600 "$dir"/*.tgz
  # Cluster mode keeps the runtime state (identity-apps.yml, invites.json,
  # apps-state.json, wg-clients.json, and the mutable secrets.yml) on the
  # console's PVC, not on the host. Stream it out through the pod.
  if kubectl -n "$CONSOLE_NAMESPACE" get deploy umkim-thin-console >/dev/null 2>&1; then
    if kubectl -n "$CONSOLE_NAMESPACE" exec deploy/umkim-thin-console --          tar czf - -C /data . > "$dir/console-data-$stamp.tgz" 2>/dev/null; then
      chmod 600 "$dir/console-data-$stamp.tgz"
    else
      warn "could not archive the console PVC; the etcd snapshot and host state are still valid"
      rm -f "$dir/console-data-$stamp.tgz"
    fi
  fi
  cp -f /var/lib/rancher/k3s/server/db/snapshots/umkim-thin-"$stamp"* "$dir"/ 2>/dev/null || true
  chmod 600 "$dir"/* 2>/dev/null || true
  cat <<EOF

Backup written to $dir:
  - etcd snapshot (the cluster state)
  - umkim-thin-state-$stamp.tgz (the host seed: secrets, values, keys)
  - console-data-$stamp.tgz (the console PVC: invites, app state, WireGuard state)

Application data lives in PersistentVolumeClaims. This backup does NOT copy it:
snapshot the VM (or use your storage's volume snapshots) for Nextcloud photos,
Immich, Paperless and the media stack. See docs/quickstart/06-backup.md.
EOF
}

do_doctor() {
  if [ -z "$ZONE" ] && [ -r "$STATE_DIR/config.env" ]; then
    ZONE=$(sed -n 's/^UMKIM_THIN_ZONE=//p' "$STATE_DIR/config.env" | head -1)
  fi
  local fails=0
  section() { printf '\n== %s ==\n' "$*"; }
  ok()  { printf '  ok    %s\n' "$*"; }
  bad() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }

  section "host"
  if have_k3s; then ok "k3s is installed"; else bad "k3s is not installed"; fi
  if [ -r /etc/rancher/k3s/k3s.yaml ]; then ok "kubeconfig is readable"; else bad "kubeconfig is missing"; fi

  section "cluster"
  if kubectl get nodes >/dev/null 2>&1; then
    ok "the API answers"
    while read -r name status rest; do
      [ -z "$name" ] && continue
      case "$status" in
        Ready) ok "node $name is Ready" ;;
        *)     bad "node $name is $status" ;;
      esac
    done < <(kubectl get nodes --no-headers 2>/dev/null)
  else
    bad "cannot reach the Kubernetes API"
  fi

  section "workloads"
  if kubectl -n "$IDENTITY_NAMESPACE" get pods >/dev/null 2>&1; then
    while read -r name ready rest; do
      [ -z "$name" ] && continue
      case "$rest" in
        Completed*|Succeeded*) continue ;;
      esac
      if [ "${ready%%/*}" = "${ready##*/}" ]; then
        ok "identity/$name is ready"
      else
        bad "identity/$name is $ready"
      fi
    done < <(kubectl -n "$IDENTITY_NAMESPACE" get pods --no-headers 2>/dev/null)
  else
    bad "the identity namespace is missing"
  fi
  if kubectl -n "$CONSOLE_NAMESPACE" get deploy umkim-thin-console >/dev/null 2>&1; then
    ok "the console deployment exists"
  else
    bad "the console deployment is missing"
  fi
  if kubectl -n servarr get job servarr-bootstrap >/dev/null 2>&1; then
    local bootstrap_failed
    bootstrap_failed=$(kubectl -n servarr get job servarr-bootstrap \
      -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null)
    if [ "$bootstrap_failed" = "True" ]; then
      bad "the media stack bootstrap failed (console: Applications -> Run setup)"
    else
      ok "the media stack bootstrap is not in a failed state"
    fi
  fi

  section "routes"
  if [ -z "$ZONE" ]; then
    bad "no zone in $STATE_DIR/config.env"
  else
    local h code hosts="auth.$ZONE console.$ZONE home.$ZONE"
    if kubectl -n "$IDENTITY_NAMESPACE" get ingress lldap-portal >/dev/null 2>&1; then
      hosts="$hosts lldap.$ZONE"
    fi
    for h in $hosts; do
      if getent ahostsv4 "$h" >/dev/null 2>&1; then
        ok "$h resolves"
      else
        bad "$h does not resolve"
      fi
      code=000
      if [ "${INSECURE:-0}" -eq 1 ]; then
        code=$(curl -ksS -o /dev/null -m 10 -w '%{http_code}' "https://$h/" 2>/dev/null) || code=000
      else
        code=$(curl -fsS -o /dev/null -m 10 -w '%{http_code}' "https://$h/" 2>/dev/null) || code=000
      fi
      # A lab/VPN host may not have a public hairpin; the node's own ingress is
      # still the right place to answer.
      if [ "$code" = "000" ]; then
        code=$(curl -ksS --resolve "$h:443:127.0.0.1" -o /dev/null -m 10 -w '%{http_code}' "https://$h/" 2>/dev/null) || code=000
      fi
      case "$code" in
        200|302|401|403) ok "https://$h/ -> $code" ;;
        *) bad "https://$h/ -> $code (DNS, firewall or certificate?)" ;;
      esac
    done
  fi

  section "helm"
  local helm_bin="${HELM:-/usr/local/bin/helm}"
  [ -x "$helm_bin" ] || helm_bin=$(command -v helm || true)
  if [ -n "$helm_bin" ] && [ -x "$helm_bin" ]; then
    local pending
    pending=$("$helm_bin" list -A --pending --kubeconfig /etc/rancher/k3s/k3s.yaml 2>/dev/null | tail -n +2)
    if [ -n "$pending" ]; then
      bad "a Helm release is pending: $pending (see docs/quickstart/07-upgrade.md)"
    else
      ok "no pending Helm releases"
    fi
  else
    info "helm not found; skipping the release check"
  fi

  section "storage"
  if kubectl get storageclass >/dev/null 2>&1; then
    while read -r name rest; do
      [ -z "$name" ] && continue
      ok "storage class $name"
    done < <(kubectl get storageclass --no-headers 2>/dev/null)
  else
    bad "cannot list storage classes"
  fi
  df -h /var | tail -1 | awk '{printf "  info  /var has %s free (%s used)\n", $4, $5}'

  section "backups"
  if [ -d /var/backups/umkim-thin ] && [ -n "$(ls -A /var/backups/umkim-thin 2>/dev/null)" ]; then
    ok "a backup directory exists"
  else
    bad "no backup found; run: sudo ./install.sh --backup"
  fi

  printf '\n'
  [ "$fails" -eq 0 ] && { echo "All checks passed."; return 0; }
  echo "$fails check(s) failed. See docs/quickstart/08-troubleshooting.md."
  return 1
}

do_uninstall() {
  local args=()
  [ "${ASSUME_YES:-0}" -eq 1 ] && args+=(--yes)
  [ "${PURGE_DATA:-0}" -eq 1 ] && args+=(--purge-data)
  [ "${DRY_RUN:-0}" -eq 1 ] && args+=(--dry-run)
  [ -x "$SCRIPT_DIR/teardown.sh" ] || die "teardown.sh is missing next to install.sh"
  exec bash "$SCRIPT_DIR/teardown.sh" "${args[@]}"
}

main() {
  case "$MODE" in
    backup) validate_inputs; do_backup; return 0 ;;
    doctor) validate_inputs; do_doctor; return $? ;;
    uninstall) validate_inputs; do_uninstall; return 0 ;;
  esac

  validate_inputs

  if [ "$RENDER_ONLY" -eq 1 ]; then
    OUT="${OUT:-$SCRIPT_DIR/.umkim-thin-render}"
    STATE_DIR="$OUT"
    mkdir -p "$STATE_DIR"
    render_all
    printf '\nRendered to %s:\n' "$OUT"
    ls -1 "$OUT"
    return 0
  fi

  [ "$DRY_RUN" -eq 1 ] || need_root
  preflight
  prepare_host
  if [ "$DRY_RUN" -eq 1 ]; then
    log "dry run: the plan is"
    info "install k3s, helm; package charts into $DATA_DIR/chart-mirror"
    info "generate secrets in $STATE_DIR (if absent)"
    info "install identity, apply the Traefik config, seed and install the console"
    info "print the portal and console URLs"
    return 0
  fi

  install_k3s
  install_helm
  open_firewall
  render_all
  package_charts
  build_or_import_image
  install_identity
  apply_traefik
  install_vpn_dns
  install_storage_agent
  seed_console
  install_console
  print_summary
}

main "$@"
