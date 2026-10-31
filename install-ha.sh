#!/usr/bin/env bash
# Interactive wrapper for the supported three-node HA provisioning path.
#
# It only collects the site-specific values, writes the ignored
# deploy/provision-ha.env, and optionally runs deploy/provision.sh.
#
#   ./install-ha.sh
#   ./install-ha.sh --dry-run
#   ./install-ha.sh --no-run
#   ./install-ha.sh --force
#   ./install-ha.sh --force --yes
#
# HA needs three servers and a control-plane VIP. External NFS is optional and
# can be added later in the console. The wrapper validates the inputs it can
# validate locally; the Ansible run performs the host/DNS/Kubernetes checks.
set -euo pipefail

cd "$(dirname "$0")"

FORCE=false
RUN=true
DRY_RUN=false
ASSUME_YES=false
ENV_FILE="${HA_ENV_FILE:-deploy/provision-ha.env}"

usage() {
  cat <<'EOF'
Usage: ./install-ha.sh [options]

Options:
  --dry-run       Print the generated environment and do not write or run.
  --no-run        Write the environment file, but do not run provisioning.
  --force         Overwrite an existing provision-ha.env.
  --yes           Accept preflight warnings and run without another prompt.
  --env FILE      Destination environment file (default: deploy/provision-ha.env).
  -h, --help      Show this help.

The prompts treat external NFS as optional (add it later in the console), accept
an optional fixed public/NAT IP for the DNS preflight (leave blank for dynamic
DNS), and ask for Longhorn replicas (3 default, 2 for a small lab).

To uninstall the HA site (primary last):
  ./uninstall-ha.sh --env deploy/provision-ha.env
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
  for tool in go helm podman ssh; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "error: $tool is required on the machine running install-ha.sh" >&2
      echo "       install the build/provisioning tools here, or use --no-run to only write the env" >&2
      exit 1
    fi
  done
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
  local ip=$1 a b c d
  [[ $ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  IFS=. read -r a b c d <<<"$ip"
  for n in "$a" "$b" "$c" "$d"; do
    [ "$n" -ge 0 ] 2>/dev/null && [ "$n" -le 255 ] 2>/dev/null || return 1
  done
}

ip_to_int() {
  local a b c d
  IFS=. read -r a b c d <<<"$1"
  echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

int_to_ip() {
  local n=$1
  printf '%d.%d.%d.%d\n' "$(( (n >> 24) & 255 ))" "$(( (n >> 16) & 255 ))" "$(( (n >> 8) & 255 ))" "$(( n & 255 ))"
}

expand_servers() {
  local raw=${1//,/ }
  local token start end begin finish n
  SERVER_IPS=()
  for token in $raw; do
    if [[ $token == *-* ]]; then
      start=${token%%-*}
      end=${token#*-}
      valid_ip "$start" || { echo "  invalid start address: $start"; return 1; }
      if [[ $end != *.* ]]; then
        end="${start%.*}.$end"
      fi
      valid_ip "$end" || { echo "  invalid end address: $end"; return 1; }
      begin=$(ip_to_int "$start")
      finish=$(ip_to_int "$end")
      if [ "$begin" -gt "$finish" ]; then
        echo "  range start is after range end"
        return 1
      fi
      for ((n=begin; n<=finish; n++)); do
        SERVER_IPS+=("$(int_to_ip "$n")")
      done
    else
      valid_ip "$token" || { echo "  invalid address: $token"; return 1; }
      SERVER_IPS+=("$token")
    fi
  done
}

valid_domain() {
  [[ $1 =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]
}

derive_base_dn() {
  local domain=$1 part out=
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

echo "umkim-thin HA setup"
echo "==================="
echo
echo "This writes $ENV_FILE and, by default, runs deploy/provision.sh."
echo "The HA target needs three servers and a control-plane VIP; external NFS is optional."
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
  ask_required SERVER_INPUT "Server IPs (3 servers; e.g. 10.0.0.11-13)"
  if ! expand_servers "$SERVER_INPUT"; then
    continue
  fi
  if [ "${#SERVER_IPS[@]}" -ne 3 ]; then
    echo "  HA needs exactly three servers; got ${#SERVER_IPS[@]}"
    continue
  fi
  break
done
read -r -p "SSH user [$([ -n "${VUSER:-}" ] && echo "$VUSER" || echo linux)]: " VUSER_IN
VUSER=${VUSER_IN:-${VUSER:-linux}}
VHOST=${SERVER_IPS[0]}
SERVERS=${SERVER_IPS[*]}

while :; do
  ask_required VIP "Control-plane VIP (free IP, not one of the servers)"
  valid_ip "$VIP" || { echo "  invalid IPv4 address"; continue; }
  conflict=false
  for ip in "${SERVER_IPS[@]}"; do
    [ "$ip" = "$VIP" ] && conflict=true
  done
  if [ "$conflict" = true ]; then
    echo "  the VIP must differ from all server addresses"
    continue
  fi
  break
done

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
echo "External NFS is optional. Leave the server blank to add it later in the"
echo "console (Storage); until then app media uses Longhorn RWX."
ask NFS_SERVER "External NFS server (optional; IP or hostname only)" ""
if [ -n "$NFS_SERVER" ]; then
  ask NFS_EXPORT "NFS export path (absolute)" "/srv/nfs/umkim-thin"
else
  NFS_EXPORT=""
fi

while :; do
  read -r -p "Longhorn replicas per volume (2 or 3) [3]: " REPLICAS
  REPLICAS=${REPLICAS:-3}
  case "$REPLICAS" in
    2|3) break ;;
    *) echo "  choose 2 or 3" ;;
  esac
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
ask JUMP "Ansible executor/jump host" "$VHOST"
ask JUMP_PORT "JUMP SSH port" "22"
ask VSSH_KEY "SSH key for JUMP" "${HOME}/.ssh/id_ed25519"
if [ ! -f "$VSSH_KEY" ]; then
  echo "  warning: $VSSH_KEY does not exist on this machine"
fi
if [[ " $SERVERS " == *" $JUMP "* ]]; then
  echo "  warning: JUMP is one of the HA target nodes."
  echo "  The common role may reboot it during the first run; that kills the"
  echo "  Ansible process and this run. Rerun after boot, or use a separate"
  echo "  management host for the first run."
fi
echo
echo "The executor must have ansible and passwordless SSH to all three servers,"
echo "including itself if it is the first/bootstrap server."
echo

ask WG_ENDPOINT "WireGuard endpoint (public hostname clients dial)" "wg.$ZONE"
ask WG_SUBNET "WireGuard tunnel subnet" "192.168.169.0/24"
ask WG_SERVER_IP "WireGuard gateway address" "192.168.169.1"
ask WG_PORT "WireGuard UDP port" "51820"

mkdir -p "$(dirname "$ENV_FILE")"
umask 077
tmp=$(mktemp "${ENV_FILE}.tmp.XXXXXX")
cleanup() { rm -f "$tmp"; }
trap cleanup EXIT

{
  echo "# Generated by install-ha.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# Keep this file private: it contains addresses and key paths."
  echo
  printf 'JUMP=%s\n' "$(quote_env "$JUMP")"
  printf 'JUMP_PORT=%s\n' "$(quote_env "$JUMP_PORT")"
  printf 'VSSH_KEY=%s\n' "$(quote_env "$VSSH_KEY")"
  printf 'VUSER=%s\n' "$(quote_env "$VUSER")"
  printf 'VHOST=%s\n' "$(quote_env "$VHOST")"
  printf 'REMOTE_WORKDIR=%s\n' "$(quote_env "/home/$VUSER/umkim-thin-deploy-ha")"
  printf 'servers=%s\n' "$(quote_env "$SERVERS")"
  echo
  printf 'k3s_control_plane_vip=%s\n' "$(quote_env "$VIP")"
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
  echo 'storage_backend=longhorn'
  echo 'storage_class_config=longhorn-rwo'
  if [ -n "$NFS_SERVER" ]; then
    echo 'storage_class_media=umkim-thin-nfs'
    echo 'nfs_storage_class=umkim-thin-nfs'
    echo 'nfs_mode=external'
    printf 'nfs_server=%s\n' "$(quote_env "$NFS_SERVER")"
    printf 'nfs_export=%s\n' "$(quote_env "$NFS_EXPORT")"
  else
    echo 'storage_class_media=longhorn-rwx'
    echo '# External NFS can be added later in the console (Storage).'
    echo 'nfs_storage_class=umkim-thin-nfs'
  fi
  printf 'longhorn_replica_count=%s\n' "$REPLICAS"
  echo 'storage_agent=false'
  echo 'storage_socket=""'
  echo
  printf 'wg_endpoint=%s\n' "$(quote_env "$WG_ENDPOINT")"
  printf 'wg_subnet=%s\n' "$(quote_env "$WG_SUBNET")"
  printf 'wg_server_ip=%s\n' "$(quote_env "$WG_SERVER_IP")"
  echo
  printf 'acme_staging=%s\n' "$ACME_STAGING"
  printf 'lab_ca_default=%s\n' "$LAB_CA_DEFAULT"
  echo 'secrets_encryption=false'
  printf 'public_ip=%s\n' "$(quote_env "$PUBLIC_IP")"
  echo
  echo 'chart_repos=""'
  printf 'remote_dir=%s\n' "$(quote_env "/opt/umkim-thin")"
  printf 'state_dir=%s\n' "$(quote_env "/etc/umkim-thin")"
} >"$tmp"
chmod 600 "$tmp"
if [ "$DRY_RUN" = true ]; then
  echo
  echo "# generated HA environment (dry run; not written)"
  cat "$tmp"
  exit 0
fi
mv "$tmp" "$ENV_FILE"
trap - EXIT
chmod 600 "$ENV_FILE"

echo
echo "Wrote $ENV_FILE (mode 0600)."
echo

only_jump_warning=false
if [ "$RUN" = true ]; then
  verify_log=$(mktemp)
  set +e
  ./verify-ha.sh "$ENV_FILE" | tee "$verify_log"
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
    if [ "$warning_count" -eq 1 ] && grep -q 'JUMP is one of the target nodes' "$verify_log"; then
      only_jump_warning=true
      if [ "$ASSUME_YES" = false ]; then
        echo
        echo "Only warning: JUMP is a target node. A first-run reboot may stop this run; rerun if it does."
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
