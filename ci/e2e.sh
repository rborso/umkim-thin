#!/usr/bin/env bash
# End-to-end lab run for one target: reset to the clean base, provision it, assert
# the platform, install the catalog apps, and run the Playwright verification.
# Run on the lab runner (it reaches the jumphost and the hypervisors).
set -euo pipefail
cd "$(dirname "$0")/.."
repo_root="$(pwd)"

TARGET="${1:?usage: ci/e2e.sh single|ha [--full]}"
shift || true
if [ "${1:-}" = "--full" ]; then
  CI_PROFILE=full
fi
export CI_PROFILE="${CI_PROFILE:-default}"
export TARGET
export PROVISION_ENV="deploy/provision-${TARGET}.env"
[ -f "$PROVISION_ENV" ] || { echo "missing $PROVISION_ENV (copy the .example and edit)" >&2; exit 2; }
# shellcheck source=ci/lib.sh
source ci/lib.sh

# Every run gets a unique tenant and password. The password is passed to the
# Playwright process through the environment and never written to disk; the
# cleanup trap removes the accounts, keys and repositories even on failure.
run_id="${UMKIM_THIN_CI_RUN:-$(date +%s)-$RANDOM}"
export UMKIM_THIN_CI_RUN="$run_id"
portal_user="ci-portal-$run_id"
portal_pw="$(python3 -c 'import secrets,string; print("".join(secrets.choice(string.ascii_letters+string.digits) for _ in range(24)))')"
export UMKIM_THIN_PORTAL_USER="$portal_user" UMKIM_THIN_PORTAL_PASSWORD="$portal_pw"
mask "$portal_pw"

apps="${ci_apps:-gitea}"
home_host="${UMKIM_THIN_HOME_HOST:-home.$ZONE}"

# "full" is an on-demand profile: it exercises ACME with staging certificates
# for real (no production Let's Encrypt), verifies TLS strictly, and builds the
# optional Servarr bootstrap image. The default profile keeps the lab CA as the
# fallback and tolerates HTTPS errors.
if [ "$CI_PROFILE" = "full" ]; then
  echo "==== $TARGET: full profile ===="
  # A matching default certificate makes Traefik skip ACME entirely.
  export lab_ca_default=false
  export UMKIM_THIN_STRICT_TLS=1
  trust_staging_roots
  # Fail early if the zone does not point at this cluster (the ACME challenge
  # depends on it). The external vantage is a separate run: see ci/preflight.sh.
  ./ci/preflight.sh "$TARGET" || { echo "preflight failed; refusing to run ACME" >&2; exit 1; }
fi

# The HA zone resolves to the public DDNS, which does not hairpin from the lab;
# resolve the target's names straight to its VIP on the runner.
if [ -n "${k3s_control_plane_vip:-}" ]; then
  app_hosts=$(ZONE="$ZONE" APPS="$apps" python3 - <<'PY'
import json, os
zone = os.environ["ZONE"]
m = json.load(open("ci/apps.json"))
out = []
for a in os.environ["APPS"].split():
    for e in m.get(a, []):
        out.append(e["host"].replace("{zone}", zone))
print(" ".join(out))
PY
)
  # shellcheck disable=SC2086
  # Apps may publish extra routes beyond their primary host (Nextcloud routes
  # Collabora separately); those need pinning too, or the browser/CI resolves
  # them through the public DDNS and Collabora refuses the request.
  extra_hosts=""
  case " $apps " in
    *" nextcloud "*) extra_hosts="collabora.$ZONE" ;;
  esac
  # shellcheck disable=SC2086
  pin_hosts "$k3s_control_plane_vip" "$PORTAL_HOST" "$CONSOLE_HOST" "$home_host" $app_hosts $extra_hosts
fi

# The private chart repos are cloned with this key (provision.sh inherits it).
export GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh -i $HOME/.ssh/chart -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new}"

echo "==== $TARGET: reset ===="
if [ "${SKIP_RESET:-0}" = "1" ]; then
  echo ">> SKIP_RESET=1: leaving the existing environment in place"
else
  ./ci/reset.sh "$TARGET"
fi

# A snapshot reset boots with a stale clock. The Ansible common role installs
# packages (dnf verifies GPG signatures against the clock) before its own chrony
# step, so step the clock here first, retrying until the nodes are reachable.
echo "==== $TARGET: clock ===="
for _ in $(seq 1 30); do
  if sync_clock >/dev/null 2>&1; then sleep 3; sync_clock >/dev/null 2>&1 || true; break; fi
  sleep 5
done

echo "==== $TARGET: provision ===="
./deploy/provision.sh

echo "==== $TARGET: install apps ($apps) ===="
for a in $apps; do
  echo ">> install $a"
  if ! cli app install "$a" 2>&1 | tail -3; then
    echo ">> retry $a"
    cli app install "$a" 2>&1 | tail -3
  fi
done

echo "==== $TARGET: wait for the app workloads ===="
for a in $apps; do
  node_root <<EOS 2>&1 | tail -2
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $a rollout status deployment --timeout=420s || true
EOS
done

# Now that the app namespaces exist, apply the default-deny ingress policies
# (the netpol role only protects namespaces that exist when it runs).
echo "==== $TARGET: apply app network policies ===="
run_tags netpol >/dev/null 2>&1 || echo ">> WARN: netpol re-run failed"

# A full run also exercises the console's on-demand bootstrap: store the tracker
# credentials through the API, let the console build the image on the pinned node
# and run the chart's Job. The direct script is a fallback, not the path under
# test; a bootstrap failure is reported but does not fail the pipeline.
if [ "$CI_PROFILE" = "full" ] && printf ' %s ' "$apps" | grep -q ' servarr '; then
  echo "==== $TARGET: servarr bootstrap through the console (full) ===="
  if ! ./ci/console-servarr.sh "$TARGET"; then
    echo ">> WARN: the console bootstrap failed; falling back to the direct path"
    ./ci/enable-servarr-bootstrap.sh "$TARGET" || true
  fi
fi

echo "==== $TARGET: platform conformance ===="
# Belt and braces: a snapshot restore can leave a node's clock hours behind and
# the session cookies depend on it (see ci/lib.sh:sync_clock).
sync_clock
./ci/assert.sh "$TARGET"
if [ "$TARGET" = "single" ]; then
  # The single target starts from a host state directory; after this change the
  # console state lives on the PVC. Verify the seed was copied intact.
  ./tests/e2e/migration/migrate-single.sh
fi

echo "==== $TARGET: backups ===="
# Create, verify and restore through the console's own CLI (the same code the
# schedule and the Backups tab use). Apps are installed by now, so app values
# and PVC snapshots are part of the archive.
./ci/backup-check.sh "$TARGET"

echo "==== $TARGET: regular portal user ===="
cleanup() { (cd "$repo_root" && ./ci/cleanup-ci.sh "$TARGET" "$run_id" >/dev/null 2>&1) || true; }
trap cleanup EXIT
./ci/portal-user.sh "$TARGET"

echo "==== $TARGET: build the app list ===="
ZONE="$ZONE" APPS="$apps" python3 - <<'PY' > tests/playwright/apps.json
import json, os
zone = os.environ["ZONE"]
manifest = json.load(open("ci/apps.json"))
entries = []
for a in os.environ["APPS"].split():
    for e in manifest.get(a, []):
        e = dict(e); e["host"] = e["host"].replace("{zone}", zone); entries.append(e)
print(json.dumps(entries))
PY
cat tests/playwright/apps.json

echo "==== $TARGET: playwright - acceptance gate ===="
# Read the freshly provisioned directory's admin password (not the pre-reset one).
pw="$(admin_password)"
mask "$pw"
# App-admin credentials the functional user journeys need, read from ci/apps.json so a
# new app only has to declare its Secret there (not here). Fetched before cd,
# because node_root uses a repo-relative key.
while IFS=$'\t' read -r name ns secret key user; do
  [ -n "$name" ] || continue
  val="$(node_root <<EOS | base64 -d 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get secret $secret -o jsonpath='{.data.$key}' 2>/dev/null || true
EOS
)"
  pre="UMKIM_THIN_$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
  export "${pre}_ADMIN_USER=$user"
  export "${pre}_ADMIN_PASSWORD=$val"
done < <(APPS="$apps" python3 - <<'PY'
import json, os
manifest = json.load(open("ci/apps.json"))
for app in os.environ["APPS"].split():
    for entry in manifest.get(app, []):
        admin = entry.get("admin")
        if admin:
            # the entry's own name: a manifest group can publish several apps
            # (servarr -> sonarr/radarr/jellyfin), each with its own credentials
            print("\t".join([entry["name"], admin["namespace"], admin["secret"],
                             admin.get("passwordKey", "admin-password"), admin["user"]]))
PY
)

# API keys (the *arr apps keep theirs in a chart Secret, not a config file).
while IFS=$'\t' read -r name ns secret key; do
  [ -n "$name" ] || continue
  val="$(node_root <<EOS | base64 -d 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get secret $secret -o jsonpath='{.data.$key}' 2>/dev/null || true
EOS
)"
  pre="UMKIM_THIN_$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
  export "${pre}_API_KEY=$val"
done < <(APPS="$apps" python3 - <<'PY'
import json, os
manifest = json.load(open("ci/apps.json"))
for app in os.environ["APPS"].split():
    for entry in manifest.get(app, []):
        k = entry.get("apiKey")
        if k:
            print("\t".join([entry["name"], k["namespace"], k["secret"], k["key"]]))
PY
)

# A fixture for the media/import chain: .strm files are indexed by Jellyfin
# without probing real media, and the file lives on the shared /media volume.
if printf ' %s ' "$apps" | grep -q ' servarr '; then
  echo "==== $TARGET: media fixture ===="
  node_root <<'EOS' || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n servarr exec deploy/sonarr -- sh -c \
  'mkdir -p /media/ci-library && chmod 0777 /media/ci-library && echo "https://example.invalid/ci.mp4" > "/media/ci-library/CI Sample (2020).strm" && chmod 0666 "/media/ci-library/CI Sample (2020).strm"' \
  && echo "fixture placed in /media/ci-library"
EOS
fi

export UMKIM_THIN_ZONE="$ZONE" \
  UMKIM_THIN_HOME_HOST="$home_host" \
  UMKIM_THIN_APPS="$(cat tests/playwright/apps.json)" \
  UMKIM_THIN_USER=admin \
  UMKIM_THIN_PASSWORD="$pw" \
  UMKIM_THIN_PORTAL_USER="$portal_user" \
  UMKIM_THIN_PORTAL_PASSWORD="$portal_pw" \
  UMKIM_THIN_GITEA_SSH_PORT=2222 \
  UMKIM_THIN_CONSOLE_HOST="$CONSOLE_HOST" \
  UMKIM_THIN_AUTH_HOST="$PORTAL_HOST"

cd tests/playwright
[ -d node_modules ] || npm ci --no-audit --no-fund
[ -d "$HOME/.cache/ms-playwright" ] || npx playwright install chromium

# Acceptance gate: routes/auth challenge, the directory-login matrix, TLS and
# the console browser flow. These gate the pipeline; the deep functional user
# journeys stay informational.
npx playwright test apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts home.spec.ts

# Functional user journeys (client simulation): INFORMATIONAL. A failure here
# must not fail the pipeline; it is written to a report for review instead.
echo "==== $TARGET: functional user journeys (informational) ===="
report="journey-report.txt"
json="/tmp/umkim-journeys-$TARGET.json"   # per target: both run in parallel
if npx playwright test journeys --workers=1 --reporter=json > "$json" 2>/tmp/umkim-journeys-"$TARGET".err; then
  :
else
  echo ">> functional user journeys reported failures (see $report)"
fi
python3 - "$report" "$json" <<'PY'
import json, sys, pathlib
report = pathlib.Path(sys.argv[1])
try:
    data = json.load(open(sys.argv[2]))
except Exception as e:  # noqa: BLE001
    report.write_text(f"could not parse the journey report: {e}\n")
    print("journeys: report unavailable")
    sys.exit(0)
rows = []
def walk(suites):
    for s in suites:
        for sp in s.get("specs", []):
            statuses = [r.get("status") for t in sp.get("tests", []) for r in t.get("results", [])]
            err = ""
            for t in sp.get("tests", []):
                for r in t.get("results", []):
                    msg = (r.get("error") or {}).get("message")
                    if msg:
                        err = msg.strip().splitlines()[0]
            if statuses and all(x == "skipped" for x in statuses):
                mark = "skip"        # not installed on this target
            elif sp.get("ok"):
                mark = "ok"
            else:
                mark = "FAILED"
            rows.append((mark, sp.get("title", ""), err))
        walk(s.get("suites", []))
walk(data.get("suites", []))
counts = {k: sum(1 for r in rows if r[0] == k) for k in ("ok", "FAILED", "skip")}
lines = [
    f"functional user journey report: {len(rows)} tests, {counts['ok']} ok, {counts['FAILED']} failed, {counts['skip']} skipped",
    "(informational - a failure here does not fail the pipeline)",
    "",
]
for mark, title, err in rows:
    lines.append(f"  {mark:6} {title}")
    # only a failing test needs its error: a pass on retry still carries the
    # first attempt's error, which would read as a failure next to "ok"
    if err and mark == "FAILED":
        lines.append(f"         {err[:300]}")
report.write_text("\n".join(lines) + "\n")
print("\n".join(lines))
PY

echo "==== $TARGET: PASSED ===="
