#!/usr/bin/env bash
# Full data backup, destructive wipe of the markers, restore, and verification.
# This is the last phase of an extended run: everything before it has already
# written data, so the archive contains the real app state.
#
#   ./ci/backup-restore.sh single
#   ./ci/backup-restore.sh ha --mark ci-marker-...
#
# The archive goes to a per-run directory on the NFS target when the console
# has one (the console PVC is only 2 GiB), otherwise to the console's own
# directory with a size check.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/backup-restore.sh single|ha [--mark name]}"
shift || true
MARK="${UMKIM_THIN_MARK:-ci-marker-${UMKIM_THIN_CI_RUN:-$(date +%s)}}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --mark) MARK="$2"; shift 2 ;;
    --mark=*) MARK="${1#*=}"; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

run="${UMKIM_THIN_CI_RUN:-$(date +%s)}"
fails=0
ok()  { printf '  ok    %s\n' "$*"; }
bad() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }
say() { printf '\n== %s ==\n' "$*"; }

# Pick the target: prefer a mounted NFS target (the console PVC is small). The
# target must be one the console discovered; a per-run subdirectory is not a
# target. The CI environment mounts a dedicated sub-export at /backups/nfs, so
# its archives do not mix with the user's.
targets=$(cli backup targets 2>/dev/null || true)
dir=""
if printf '%s\n' "$targets" | grep -q '^nfs '; then
  dir="/backups/nfs"
elif printf '%s\n' "$targets" | grep -q '^console '; then
  dir="/data/backups"
else
  echo "no backup target is available" >&2
  exit 1
fi
say "backup target $dir"
cli backup schedule --interval off --data copy --target "$dir" >/dev/null 2>&1 || {
  bad "could not set the backup target"; exit 1;
}

say "create the markers (if they are not there yet)"
./ci/markers.sh create "$TARGET" --mark "$MARK" || { bad "could not create the markers"; exit 1; }

say "full data backup"
out=$(cli backup create 2>&1)
rc=$?
printf '%s\n' "$out" | sed 's/^/   /'
name=$(printf '%s\n' "$out" | sed -n 's/^wrote \([^ ]*\) .*/\1/p' | head -1)
if [ "$rc" -ne 0 ] || [ -z "$name" ]; then
  bad "backup create failed"
  exit 1
fi
ok "archive $name"
cli backup verify "$name" >/dev/null 2>&1 && ok "archive verifies" || bad "archive verification failed"

say "delete the markers (the restore has to bring them back)"
./ci/markers.sh delete "$TARGET" --mark "$MARK" || bad "marker delete reported a problem"

say "restore (data + apps)"
out=$(cli backup restore "$name" --data --apps --confirm 2>&1)
rc=$?
printf '%s\n' "$out" | sed 's/^/   /'
if [ "$rc" -ne 0 ]; then
  bad "restore failed"
else
  ok "restore completed"
fi
if printf '%s\n' "$out" | grep -q 'failed:'; then
  for v in $(printf '%s\n' "$out" | sed -n 's/.*volume \([^ ]*\): failed:.*/\1/p'); do
    say "retry $v"
    cli backup restore "$name" --data --confirm --volume "$v" 2>&1 | tail -4 | sed 's/^/   /'
  done
  bad "at least one volume failed to restore"
fi

say "wait for the restored workloads"
for i in $(seq 1 60); do
  out=$(node_root <<'EOS' 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl get pods -A --no-headers
EOS
)
  bad_pods=$(printf '%s\n' "$out" | awk '
    $3 == "Completed" || $3 == "Error" { next }
    { split($2, a, "/"); if ($3 != "Running" || a[1] != a[2]) bad++ }
    END { print bad + 0 }')
  [ "${bad_pods:-1}" -eq 0 ] && break
  sleep 10
done
for h in "immich.$ZONE" "gitea.$ZONE" "paperless.$ZONE" "nextcloud.$ZONE"; do
  for i in $(seq 1 30); do
    c=$(curl -sk -m 10 -o /dev/null -w '%{http_code}' --resolve "$h:443:${k3s_control_plane_vip:-$VHOST}" "https://$h/" 2>/dev/null || echo 000)
    case "$c" in 200|301|302|303|307|308|401|403) break ;; esac
    sleep 10
  done
done

say "verify the markers after the restore"
if ./ci/markers.sh verify "$TARGET" --mark "$MARK"; then ok "all markers are back"; else bad "markers are missing after the restore"; fi

say "verify the apps after the restore"
for h in "immich.$ZONE" "gitea.$ZONE" "nextcloud.$ZONE"; do
  c=$(curl -sk -m 15 -o /dev/null -w '%{http_code}' --resolve "$h:443:${k3s_control_plane_vip:-$VHOST}" "https://$h/" 2>/dev/null || echo 000)
  case "$c" in 200|301|302|303|307|308|401|403) ok "$h ($c)" ;; *) bad "$h ($c)" ;; esac
done

# Leave the environment in its normal mode for the next run.
cli backup schedule --interval off --data snapshot >/dev/null 2>&1 || true


if [ "$fails" -ne 0 ]; then
  echo "backup/restore check found $fails problem(s)" >&2
  exit 1
fi
echo "backup/restore check passed"
