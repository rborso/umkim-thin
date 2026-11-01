#!/usr/bin/env bash
# Extended lab run: install, real app data (the Playwright journeys), markers,
# Servarr download, VPN, HA faults/reboots/full stop, then the full backup and
# restore as the last phase.
#
#   ./ci/extended.sh single
#   ./ci/extended.sh ha
#   ./ci/extended.sh ha --regen-certs          # weekly: let ACME re-issue
#   ./ci/extended.sh single --skip-faults --skip-backup
#
# It expects to run on the CI host (the jumphost) with CI_ON_JUMP=1. The fast
# acceptance gate is still ci/e2e.sh; this is the deep run.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/extended.sh single|ha [--regen-certs] [--skip-faults] [--skip-backup] [--skip-servarr]}"
shift || true
REGEN=0; SKIP_FAULTS=0; SKIP_BACKUP_PHASE=0; SKIP_SERVARR=0; FULL=1
for a in "$@"; do
  case "$a" in
    --regen-certs) REGEN=1 ;;
    --skip-faults) SKIP_FAULTS=1 ;;
    --skip-backup) SKIP_BACKUP_PHASE=1 ;;
    --skip-servarr) SKIP_SERVARR=1 ;;
    --no-full) FULL=0 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
export TARGET
export CI_ON_JUMP="${CI_ON_JUMP:-0}"
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
[ -f "$PROVISION_ENV" ] || { echo "missing $PROVISION_ENV" >&2; exit 2; }

run="${UMKIM_THIN_CI_RUN:-$(date +%s)-$RANDOM}"
export UMKIM_THIN_CI_RUN="$run"
export UMKIM_THIN_MARK="ci-marker-$run"

# e2e.sh does reset/provision/install/gate/journeys; we keep the data and run
# the destructive backup last.
export SKIP_BACKUP=1 SKIP_CLEANUP=1 JOURNEYS_GATING=1 CERT_CACHE=1
[ "$REGEN" = 1 ] && export CERT_REGEN=1

fails=0
phase() { printf '\n######## %s ########\n' "$*"; }
ok()    { printf '  ok    %s\n' "$*"; }
bad()   { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }

phase "extended run $TARGET ($run)"
if [ "$FULL" = 1 ]; then
  ./ci/e2e.sh "$TARGET" --full || { bad "the install/gate phase failed"; exit 1; }
else
  ./ci/e2e.sh "$TARGET" || { bad "the install/gate phase failed"; exit 1; }
fi
ok "install, acceptance gate and user journeys"

phase "data markers"
./ci/markers.sh create "$TARGET" --mark "$UMKIM_THIN_MARK" || bad "could not create the markers"

if [ "$SKIP_SERVARR" = 0 ]; then
  phase "servarr download"
  ./ci/servarr-download.sh "$TARGET" || bad "the servarr download check failed"
fi

phase "vpn"
./ci/vpn-check.sh "$TARGET" || bad "the vpn check failed"

if [ "$SKIP_FAULTS" = 0 ]; then
  phase "fault injection"
  ./ci/faults.sh "$TARGET" all || bad "fault injection failed"
fi

if [ "$SKIP_BACKUP_PHASE" = 0 ]; then
  phase "backup and restore (last)"
  ./ci/backup-restore.sh "$TARGET" --mark "$UMKIM_THIN_MARK" || bad "the backup/restore check failed"
fi

phase "certificate cache"
./ci/certs.sh save "$TARGET" || bad "could not save the certificate cache"

phase "cleanup"
./ci/cleanup-ci.sh "$TARGET" "$run" >/dev/null 2>&1 || true

if [ "$fails" -ne 0 ]; then
  echo "EXTENDED RUN FAILED: $fails phase(s)" >&2
  exit 1
fi
echo "EXTENDED RUN PASSED: $TARGET ($run)"
