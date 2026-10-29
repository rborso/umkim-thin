#!/usr/bin/env bash
# Interactive wrapper for the supported single-node provisioning path.
#
# It only collects the site-specific values, writes the ignored
# deploy/provision-single.env, and optionally runs deploy/provision.sh.
#
#   ./install-single.sh
#   ./install-single.sh --dry-run
#   ./install-single.sh --no-run
#   ./install-single.sh --force
#   ./install-single.sh --force --yes
#
# Single node means one Rocky Linux 9/10 server, local-path storage and the
# host storage agent's NFS export. The wrapper validates the inputs it can
# validate locally; the Ansible run performs the host/DNS/Kubernetes checks.
set -euo pipefail

cd "$(dirname "$0")"

FORCE=false
RUN=true
DRY_RUN=false
ASSUME_YES=false
ENV_FILE="${SINGLE_ENV_FILE:-deploy/provision-single.env}"

usage() {
  cat <<'EOF'
Usage: ./install-single.sh [options]

Options:
  --dry-run       Print the generated environment and do not write or run.
  --no-run        Write the environment file, but do not run provisioning.
  --force         Overwrite an existing provision-single.env.
  --yes           Accept preflight warnings and run without another prompt.
  --env FILE      Destination environment file (default: deploy/provision-single.env).
  -h, --help      Show this help.

The prompts ask for the domain, the single server, the jump host, WireGuard
and the TLS mode. Storage stays local-path plus the host storage agent; add
external NFS later in the console if media must live on another host.

Run this from a workstation with Go, Helm, podman and SSH, like install-ha.sh.
On the target host itself use ./install.sh, which installs k3s, Helm and
podman and needs no Go toolchain.

To uninstall the host (removes k3s and cluster data):
  sudo ./teardown.sh
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=true ;;
    --no-run) RUN=false ;;
    --force) FORCE=true ;;
    -y|--yes) ASSUME_YES=true ;;
    --env)
      shift
      [ "$#" -gt 0 ] || { echo "error: --env needs a path" >&2; exit 2; }
      ENV_FILE=$1
      ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown option $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [ "$RUN" = true ] && [ "$DRY_RUN" = false ]; then
  missing=()
  for tool in go helm podman ssh; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    echo "error: missing on the machine running install-single.sh: ${missing[*]}" >&2
    echo "       this wrapper provisions a host from a workstation (like install-ha.sh);" >&2
    echo "       it is not the on-host installer." >&2
    echo "       On the target host itself, run:" >&2
    echo "         sudo ./install.sh --zone <domain> --email <address>" >&2
    echo "       install.sh installs k3s, helm and podman and needs no Go toolchain." >&2
    echo "       Use --no-run to only write the environment file." >&2
    exit 1
  fi
fi

if [ "$DRY_RUN" = false ] && [ -e "$ENV_FILE" ] && [ "$FORCE" = false ]; then
  echo "error: $ENV_FILE already exists; use --force to overwrite it" >&2
  exit 1
fi

ask() {
  local var=$1 label=$2 def=${3:-} value
  if [ -n "$def" ]; then
    read -r -p "$label [$def]: " value
    [ -n "$value" ] || value=$def
  else
    read -r -p "$label: " value
  fi
  printf -v "$var" '%s' "$value"
}

ask_required() {
  local var=$1 label=$2 value
  while :; do
    ask "$var" "$label"
    if [ -n "${!var}" ]; then return 0; fi
    echo "  a value is required"
  done
}

valid_ip() {
  local ip=$1 a b c d n
  [[ $ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  IFS=. read -r a b c d <<<"$ip"
  for n in "$a" "$b" "$c" "$d"; do
    [ "$n" -ge 0 ] 2>/dev/null && [ "$n" -le 255 ] 2>/dev/null || return 1
  done
}

valid_cidr() {
  local addr bits
  addr=${1%/*}
  bits=${1#*/}
  [ "$addr" != "$1" ] || return 1
  valid_ip "$addr" || return 1
  [[ $bits =~ ^[0-9]+$ ]] && [ "$bits" -ge 0 ] && [ "$bits" -le 32 ]
}

valid_port() {
  [[ $1 =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

ip_in_cidr() {
  local ip=$1 cidr=$2 base bits a b c d ip_int base_int mask
  base=${cidr%/*}
  bits=${cidr#*/}
  valid_ip "$ip" && valid_cidr "$cidr" || return 1
  IFS=. read -r a b c d <<<"$ip"
  ip_int=$(( (a << 24) + (b << 16) + (c << 8) + d ))
  IFS=. read -r a b c d <<<"$base"
  base_int=$(( (a << 24) + (b << 16) + (c << 8) + d ))
  if [ "$bits" -eq 0 ]; then return 0; fi
  mask=$(( (0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF ))
  [ $(( ip_int & mask )) -eq $(( base_int & mask )) ]
}

valid_domain() {
  [[ $1 =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]
}

derive_base_dn() {
  local domain=$1 part out= parts
  IFS=. read -r -a parts <<<"$domain"
  for part in "${parts[@]}"; do
    [ -n "$out" ] && out+=,
    out+="dc=$part"
  done
  printf '%s\n' "$out"
}

quote_env() {
  local v=$1
  printf '%q' "$v"
}

echo "umkim-thin single-node setup"
echo "============================"
echo
echo "This writes $ENV_FILE and, by default, runs deploy/provision.sh."
echo "The target is one Rocky Linux 9/10 server; storage is local-path plus the"
echo "host storage agent."
echo

ask_required DOMAIN "Domain (example.com)"
while ! valid_domain "$DOMAIN"; do
  echo "  enter a DNS domain without a scheme or trailing dot"
  ask_required DOMAIN "Domain (example.com)"
done
ZONE=$DOMAIN
PORTAL_HOST="auth.$ZONE"
CONSOLE_HOST="console.$ZONE"

while :; do
  ask_required SERVER_IP "Server IPv4 address"
  valid_ip "$SERVER_IP" && break
  echo "  invalid IPv4 address"
done
VHOST=$SERVER_IP
SERVERS=$SERVER_IP

read -r -p "SSH user [$([ -n "${VUSER:-}" ] && echo "$VUSER" || echo linux)]: " VUSER_IN
VUSER=${VUSER_IN:-${VUSER:-linux}}

DERIVED_BASE_DN=$(derive_base_dn "$ZONE")
ask BASE_DN "Directory base DN" "$DERIVED_BASE_DN"
ask_required ADMIN_EMAIL "Admin email"
ACME_EMAIL=$ADMIN_EMAIL

ask LAB_CIDR "Management CIDR" "$(echo "${VHOST%.*}.0/24")"
ask PUBLIC_IP "Fixed public/NAT IP (optional; leave blank for dynamic DNS)" ""
while [ -n "$PUBLIC_IP" ] && ! valid_ip "$PUBLIC_IP"; do
  echo "  enter a valid IPv4 address, or leave blank for dynamic DNS"
  ask PUBLIC_IP "Fixed public/NAT IP (optional; leave blank for dynamic DNS)" ""
done

echo
echo "TLS mode:"
echo "  1) Lab/self-signed CA (default; no public ACME)"
echo "  2) Let's Encrypt staging (untrusted test certificates)"
echo "  3) Let's Encrypt production (trusted; rate-limited)"
read -r -p "Choose [1]: " TLS_CHOICE
case "${TLS_CHOICE:-1}" in
  1) LAB_CA_DEFAULT=true;  ACME_STAGING=true ;;
  2) LAB_CA_DEFAULT=false; ACME_STAGING=true ;;
  3) LAB_CA_DEFAULT=false; ACME_STAGING=false ;;
  *) echo "error: invalid TLS choice" >&2; exit 2 ;;
esac
if [ "$ACME_STAGING" = false ]; then
  echo "WARNING: production ACME is rate-limited (50 certificates per registered domain per 7 days)."
fi

echo
ask JUMP "Ansible executor/jump host ([user@]host)" "$VHOST"
ask JUMP_PORT "JUMP SSH port" "22"
ask VSSH_KEY "SSH key for JUMP" "${HOME}/.ssh/id_ed25519"
if [ ! -f "$VSSH_KEY" ]; then
  echo "  warning: $VSSH_KEY does not exist on this machine"
fi
if [ "${JUMP##*@}" = "$VHOST" ]; then
  echo "  warning: JUMP is the target server."
  echo "  The common role may reboot it during the first run, which kills the"
  echo "  Ansible process and this run. Rerun after boot if that happens, or"
  echo "  use a separate management host as JUMP."
fi
echo
echo "The executor must have ansible and passwordless SSH to $VHOST."
echo

ask WG_ENDPOINT "WireGuard endpoint (public IP or hostname)" "$VHOST"
ask WG_SUBNET "WireGuard tunnel subnet" "192.168.168.0/24"
ask WG_SERVER_IP "WireGuard gateway address" "192.168.168.1"
ask WG_PORT "WireGuard UDP port" "51820"

mkdir -p "$(dirname "$ENV_FILE")"
umask 077
tmp=$(mktemp "${ENV_FILE}.tmp.XXXXXX")
cleanup() { rm -f "$tmp"; }
trap cleanup EXIT

{
  echo "# Generated by install-single.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# Keep this file private: it contains addresses and key paths."
  echo
  printf 'JUMP=%s\n' "$(quote_env "$JUMP")"
  printf 'JUMP_PORT=%s\n' "$(quote_env "$JUMP_PORT")"
  printf 'VSSH_KEY=%s\n' "$(quote_env "$VSSH_KEY")"
  printf 'VUSER=%s\n' "$(quote_env "$VUSER")"
  printf 'VHOST=%s\n' "$(quote_env "$VHOST")"
  printf 'REMOTE_WORKDIR=%s\n' "$(quote_env "/home/$VUSER/umkim-thin-deploy-single")"
  printf 'servers=%s\n' "$(quote_env "$SERVERS")"
  echo
  printf 'zone=%s\n' "$(quote_env "$ZONE")"
  printf 'portal_host=%s\n' "$(quote_env "$PORTAL_HOST")"
  printf 'console_host=%s\n' "$(quote_env "$CONSOLE_HOST")"
  printf 'base_dn=%s\n' "$(quote_env "$BASE_DN")"
  printf 'admin_email=%s\n' "$(quote_env "$ADMIN_EMAIL")"
  printf 'acme_email=%s\n' "$(quote_env "$ACME_EMAIL")"
  echo
  echo 'pub_web=80'
  echo 'pub_tls=443'
  echo 'pub_wg=51820'
  echo 'web_nodeport=30080'
  echo 'tls_nodeport=30443'
  echo 'git_nodeport=30222'
  echo 'pub_git=2222'
  printf 'wg_port=%s\n' "$(quote_env "$WG_PORT")"
  echo
  printf 'pod_cidr=%s\n' "$(quote_env "10.244.0.0/16")"
  printf 'service_cidr=%s\n' "$(quote_env "10.43.0.0/16")"
  printf 'lab_cidr=%s\n' "$(quote_env "$LAB_CIDR")"
  echo
  printf 'k3s_channel=%s\n' "$(quote_env "stable")"
  echo 'k3s_version=""'
  echo
  echo '# Single node: local config storage plus the host storage agent for'
  echo '# shared media. Add external NFS later in the console if needed.'
  echo 'storage_backend=""'
  echo 'storage_agent=true'
  echo 'storage_socket=/run/umkim-thin'
  echo
  printf 'wg_endpoint=%s\n' "$(quote_env "$WG_ENDPOINT")"
  printf 'wg_direct_endpoint=%s\n' "$(quote_env "$VHOST:$WG_PORT")"
  printf 'wg_subnet=%s\n' "$(quote_env "$WG_SUBNET")"
  printf 'wg_server_ip=%s\n' "$(quote_env "$WG_SERVER_IP")"
  echo
  printf 'acme_staging=%s\n' "$ACME_STAGING"
  printf 'lab_ca_default=%s\n' "$LAB_CA_DEFAULT"
  printf 'public_ip=%s\n' "$(quote_env "$PUBLIC_IP")"
  echo
  echo 'chart_repos=""'
  printf 'remote_dir=%s\n' "$(quote_env "/opt/umkim-thin")"
  printf 'state_dir=%s\n' "$(quote_env "/etc/umkim-thin")"
} >"$tmp"
chmod 600 "$tmp"
if [ "$DRY_RUN" = true ]; then
  echo
  echo "# generated single-node environment (dry run; not written)"
  cat "$tmp"
  exit 0
fi
mv "$tmp" "$ENV_FILE"
trap - EXIT
chmod 600 "$ENV_FILE"

echo
echo "Wrote $ENV_FILE (mode 0600)."
echo

# Read-only preflight for the generated environment. Exit codes: 0 = passed,
# 2 = passed with warnings, 1 = failed. Kept in this script (rather than a
# separate verify-single.sh) because a single node has only one host to check.
preflight() {
  local fails=0 warns=0
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  ok()   { printf '  ok    %s\n' "$*"; }
  info() { printf '  info  %s\n' "$*"; }
  warn() { printf '  warn  %s\n' "$*"; warns=$((warns + 1)); }
  fail() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }

  echo "Single-node preflight: $ENV_FILE"
  echo "================================"

  echo
  echo "Build host"
  local tool
  for tool in git go helm podman ssh; do
    if command -v "$tool" >/dev/null 2>&1; then
      ok "$tool is installed"
    else
      fail "$tool is missing on the machine running provision.sh"
    fi
  done

  if command -v go >/dev/null 2>&1; then
    local go_version major rest minor
    go_version=$(go env GOVERSION 2>/dev/null || true)
    go_version=${go_version#go}
    if [ -n "$go_version" ]; then
      major=${go_version%%.*}
      rest=${go_version#*.}
      minor=${rest%%.*}
      if [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge 24 ]; }; then
        ok "Go $go_version is >= 1.24"
      else
        fail "Go $go_version is older than 1.24"
      fi
    fi
  fi

  if command -v helm >/dev/null 2>&1; then
    local helm_version
    helm_version=$(helm version --short 2>/dev/null | sed -n 's/^v\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p')
    if [ -n "$helm_version" ]; then
      set -- $helm_version
      if [ "$1" -gt 3 ] || { [ "$1" -eq 3 ] && [ "$2" -ge 16 ]; }; then
        ok "Helm $1.$2 is >= 3.16"
      else
        fail "Helm $1.$2 is older than 3.16"
      fi
    fi
  fi

  echo
  echo "Configuration"
  if valid_domain "${zone:-}"; then ok "domain: $zone"; else fail "invalid domain: ${zone:-<empty>}"; fi
  if [[ ${admin_email:-} == *@*.* ]]; then ok "admin email: $admin_email"; else fail "invalid admin email: ${admin_email:-<empty>}"; fi
  if [[ ${base_dn:-} == dc=* ]]; then ok "base DN: $base_dn"; else fail "base DN does not look like dc=...: ${base_dn:-<empty>}"; fi

  read -r -a nodes <<<"${servers:-}"
  if [ "${#nodes[@]}" -eq 1 ]; then
    ok "one server listed"
    if [ "${nodes[0]}" = "${VHOST:-}" ]; then ok "VHOST is the server"; else fail "VHOST is not the server"; fi
  elif [ "${#nodes[@]}" -eq 0 ]; then
    fail "servers is empty"
  else
    fail "expected one server, found ${#nodes[@]} (use install-ha.sh for HA)"
  fi
  local ip
  for ip in "${nodes[@]}"; do
    if valid_ip "$ip"; then ok "server address: $ip"; else fail "invalid server address: $ip"; fi
  done

  if valid_cidr "${lab_cidr:-}"; then ok "management CIDR: $lab_cidr"; else warn "invalid management CIDR: ${lab_cidr:-<empty>}"; fi
  if valid_cidr "${pod_cidr:-}"; then ok "pod CIDR: $pod_cidr"; else fail "invalid pod CIDR: ${pod_cidr:-<empty>}"; fi
  if valid_cidr "${service_cidr:-}"; then ok "service CIDR: $service_cidr"; else fail "invalid service CIDR: ${service_cidr:-<empty>}"; fi
  if valid_cidr "${wg_subnet:-}"; then ok "WireGuard subnet: $wg_subnet"; else fail "invalid WireGuard subnet: ${wg_subnet:-<empty>}"; fi
  if valid_ip "${wg_server_ip:-}" && ip_in_cidr "${wg_server_ip:-}" "${wg_subnet:-}"; then
    ok "WireGuard gateway address: $wg_server_ip"
  else
    fail "WireGuard gateway address is invalid or outside wg_subnet: ${wg_server_ip:-<empty>}"
  fi
  if valid_port "${wg_port:-}"; then ok "WireGuard port: $wg_port"; else fail "invalid WireGuard port: ${wg_port:-<empty>}"; fi
  if [ -n "${wg_endpoint:-}" ]; then ok "WireGuard endpoint: $wg_endpoint"; else fail "WireGuard endpoint is empty"; fi
  if valid_port "${JUMP_PORT:-}"; then ok "JUMP port: $JUMP_PORT"; else fail "invalid JUMP port: ${JUMP_PORT:-<empty>}"; fi
  if [ -f "${VSSH_KEY:-}" ]; then ok "SSH key exists: $VSSH_KEY"; else fail "SSH key not found: ${VSSH_KEY:-<empty>}"; fi
  case "${storage_agent:-}" in
    true) ok "host storage agent enabled" ;;
    *) warn "storage_agent is ${storage_agent:-<empty>}; single-node installs normally use the host storage agent" ;;
  esac
  if [ -n "${storage_backend:-}" ]; then
    warn "storage_backend is ${storage_backend}; single-node installs normally leave it empty"
  fi

  echo
  echo "DNS"
  resolve_ip() { getent ahostsv4 "$1" 2>/dev/null | awk 'NR==1 {print $1}'; }
  local dns_required=false dns_missing=false host resolved
  if [ "${acme_staging:-}" = false ] && [ "${lab_ca_default:-}" = false ]; then
    dns_required=true
  fi
  local dns_ips=()
  for host in "auth.${zone:-}" "console.${zone:-}" "lldap.${zone:-}"; do
    [ -n "${zone:-}" ] || break
    ip=$(resolve_ip "$host")
    if [ -z "$ip" ]; then
      if [ "$dns_required" = true ]; then fail "$host does not resolve"; else warn "$host does not resolve (lab CA mode may still work)"; fi
      dns_missing=true
    else
      dns_ips+=("$ip")
    fi
  done
  if [ "${#dns_ips[@]}" -gt 0 ]; then
    local unique_ips
    unique_ips=$(printf '%s\n' "${dns_ips[@]}" | sort -u)
    if [ "$(printf '%s\n' "$unique_ips" | wc -l | tr -d ' ')" -ne 1 ]; then
      fail "DNS records resolve to different addresses: $(printf '%s' "$unique_ips" | tr '\n' ' ')"
    else
      resolved=$unique_ips
      if [ "$resolved" = "${VHOST:-}" ]; then
        ok "auth/console/lldap -> $resolved"
      elif [ -n "${public_ip:-}" ] && [ "$resolved" = "$public_ip" ]; then
        ok "auth/console/lldap -> public/NAT address $resolved"
      elif [ -n "${public_ip:-}" ]; then
        fail "auth/console/lldap -> $resolved (public_ip is $public_ip)"
      else
        ok "auth/console/lldap -> $resolved (dynamic public/NAT address; accepted)"
      fi
    fi
  fi
  if [ "$dns_missing" = true ] && [ "$dns_required" = true ]; then
    fail "ACME production/staging requires every checked name to resolve"
  elif [ "$dns_required" = true ]; then
    ok "ACME production/staging DNS records were checked"
  fi

  echo
  echo "SSH and Ansible"
  local ssh_opts=(-n -i "${VSSH_KEY:-/nonexistent}" -p "${JUMP_PORT:-22}" -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new)
  local jump_ok=false
  jump() { ssh "${ssh_opts[@]}" "$JUMP" "$1"; }
  node() { ssh "${ssh_opts[@]}" "$JUMP" "ssh -n -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new ${VUSER:-linux}@${VHOST} '$1'"; }

  if [ -f "${VSSH_KEY:-}" ] && jump 'true' >/dev/null 2>&1; then
    jump_ok=true
    ok "JUMP is reachable: $JUMP"
    if jump 'command -v ansible-playbook >/dev/null 2>&1'; then
      ok "ansible-playbook is installed on JUMP"
      local collection
      for collection in ansible.posix kubernetes.core; do
        if jump "ansible-galaxy collection list 2>/dev/null | grep -q '$collection'"; then
          ok "Ansible collection: $collection"
        else
          fail "Ansible collection missing on JUMP: $collection"
        fi
      done
    else
      fail "ansible-playbook is not installed on JUMP"
    fi
  else
    fail "cannot SSH to JUMP with $VSSH_KEY"
  fi

  if [ "${JUMP##*@}" = "${VHOST:-}" ]; then
    warn "JUMP is the target server; a first-run reboot will kill the Ansible process (use a separate host or rerun)"
  fi

  if [ "$jump_ok" = true ]; then
    if ! node 'true' >/dev/null 2>&1; then
      fail "cannot SSH from JUMP to ${VUSER:-linux}@${VHOST}"
    else
      ok "SSH from JUMP to $VHOST"
      local hostname os_id
      hostname=$(node 'hostname' 2>/dev/null | tr -d '\r')
      [ -n "$hostname" ] && ok "hostname $VHOST: $hostname"
      if node 'sudo -n true' >/dev/null 2>&1; then
        ok "passwordless sudo on $VHOST"
      else
        fail "passwordless sudo is not available on $VHOST"
      fi
      os_id=$(node 'grep -E ^ID= /etc/os-release 2>/dev/null | cut -d= -f2' 2>/dev/null | tr -d '\r"')
      case "$os_id" in
        rocky|rhel|centos) ok "OS on $VHOST: $os_id" ;;
        *) warn "OS on $VHOST is ${os_id:-unknown}; supported target is Rocky/RHEL" ;;
      esac
      if node 'sudo -n test -e /etc/rancher/k3s/k3s.yaml && echo present || echo absent' 2>/dev/null | grep -q present; then
        warn "k3s state already exists on $VHOST; this is not a clean first install"
      fi
      if node 'lsmod | grep -q ^xt_ && echo xt-ok || echo xt-missing' 2>/dev/null | grep -q xt-missing; then
        warn "kernel on $VHOST may need the first-run reboot to load xt_* modules"
      fi
    fi
  fi

  echo
  if [ "$fails" -gt 0 ]; then
    echo "PREFLIGHT FAILED: $fails error(s), $warns warning(s)."
    return 1
  fi
  if [ "$warns" -gt 0 ]; then
    echo "PREFLIGHT PASSED WITH WARNINGS: $warns warning(s)."
    return 2
  fi
  echo "PREFLIGHT PASSED."
  return 0
}

only_jump_warning=false
if [ "$RUN" = true ]; then
  verify_log=$(mktemp)
  set +e
  preflight | tee "$verify_log"
  verify_rc=${PIPESTATUS[0]}
  set -e
  if [ "$verify_rc" -eq 1 ]; then
    rm -f "$verify_log"
    echo
    echo "Fix the errors above; the environment file was kept at $ENV_FILE."
    exit 1
  fi
  if [ "$verify_rc" -eq 2 ]; then
    warning_count=$(grep -c '^  warn ' "$verify_log" || true)
    if [ "$warning_count" -eq 1 ] && grep -q 'JUMP is the target server' "$verify_log"; then
      only_jump_warning=true
      if [ "$ASSUME_YES" = false ]; then
        echo
        echo "Only warning: JUMP is the target server. A first-run reboot may stop this run; rerun if it does."
        read -r -p "Run provisioning now? [Y/n]: " ONLY_RUN
        case "${ONLY_RUN:-Y}" in
          n|N|no|NO)
            rm -f "$verify_log"
            echo "Not running. Command:"
            echo "  PROVISION_ENV=$ENV_FILE ./deploy/provision.sh"
            exit 0
            ;;
        esac
      fi
    elif [ "$ASSUME_YES" = true ]; then
      echo "Preflight warnings accepted (--yes)."
    else
      read -r -p "Preflight passed with warnings. Continue provisioning? [y/N]: " CONTINUE_WARN
      case "${CONTINUE_WARN:-N}" in
        y|Y|yes|YES) ;;
        *)
          rm -f "$verify_log"
          echo "Not running. Fix the warnings, then run:"
          echo "  PROVISION_ENV=$ENV_FILE ./deploy/provision.sh"
          exit 0
          ;;
      esac
    fi
  fi
  rm -f "$verify_log"
  echo
fi

if [ "$RUN" = false ]; then
  echo "Run it when ready:"
  echo "  PROVISION_ENV=$ENV_FILE ./deploy/provision.sh"
  exit 0
fi

if [ "$ASSUME_YES" = true ] || [ "$only_jump_warning" = true ]; then
  RUN_NOW=Y
  if [ "$ASSUME_YES" = true ]; then
    echo "Run provisioning now? [Y/n]: Y (--yes)"
  fi
else
  read -r -p "Run provisioning now? [Y/n]: " RUN_NOW
fi
case "${RUN_NOW:-Y}" in
  n|N|no|NO) echo "Not running. Command:"; echo "  PROVISION_ENV=$ENV_FILE ./deploy/provision.sh"; exit 0 ;;
esac

exec env PROVISION_ENV="$ENV_FILE" ./deploy/provision.sh
