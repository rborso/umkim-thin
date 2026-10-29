#!/usr/bin/env bash
# Test an installation that already exists. This intentionally does NOT reset,
# provision, or install anything. Use ci/e2e.sh when you want a fresh lab
# environment; use this when you want to verify a running single/HA target.
#
#   ./ci/test-existing.sh single
#   ./ci/test-existing.sh ha --env deploy/provision-ha.env
#   ./ci/test-existing.sh single --journeys
#   ./ci/test-existing.sh ha --env deploy/provision-ha.env --pin
#
# Options: --env FILE, --report-dir DIR, --journeys, --no-platform,
#          --with-restart, --pin, --apps "group ...", --no-cleanup.
# Reports land under tests/reports/<target>/<run-id>/ by default. The gate
# creates a temporary ci-portal-* tenant and cleans up ci-* accounts on exit;
# use --no-cleanup only when you want to inspect them.
set -euo pipefail
cd "$(dirname "$0")/.."
repo_root=$(pwd)

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

TARGET="${1:?usage: ci/test-existing.sh single|ha [options]}"
shift

PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
REPORT_DIR=""
JOURNEYS=0
PLATFORM=1
CLEANUP=1
WITH_RESTART=0
PIN=0
APPS_OVERRIDE=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env) PROVISION_ENV="${2:?--env needs a file}"; shift 2 ;;
    --env=*) PROVISION_ENV="${1#*=}"; shift ;;
    --report-dir) REPORT_DIR="${2:?--report-dir needs a directory}"; shift 2 ;;
    --report-dir=*) REPORT_DIR="${1#*=}"; shift ;;
    --journeys) JOURNEYS=1; shift ;;
    --no-platform) PLATFORM=0; shift ;;
    --no-cleanup) CLEANUP=0; shift ;;
    --with-restart) WITH_RESTART=1; shift ;;
    --pin) PIN=1; shift ;;
    --apps) APPS_OVERRIDE="${2:?--apps needs a space-separated list}"; shift 2 ;;
    --apps=*) APPS_OVERRIDE="${1#*=}"; shift ;;
    -h|--help)
      sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

export TARGET PROVISION_ENV
# shellcheck source=ci/lib.sh
source ci/lib.sh
# The runner helper uses a repo-relative ssh key. Resolve it before the run
# changes directory for Playwright, otherwise post-gate node checks fail.
if [ -n "${KEY:-}" ] && [ "${KEY#/}" = "$KEY" ]; then
  KEY="$(cd "$(dirname "$KEY")" && pwd)/$(basename "$KEY")"
  export KEY
fi

[ -f "$PROVISION_ENV" ] || { echo "missing $PROVISION_ENV" >&2; exit 2; }
run_id="${UMKIM_THIN_CI_RUN:-$(date -u +%Y%m%dt%H%M%Sz | tr '[:upper:]' '[:lower:]')-$RANDOM}"
export UMKIM_THIN_CI_RUN="$run_id"
REPORT_DIR="${REPORT_DIR:-tests/reports/${TARGET}/${run_id}}"
mkdir -p "$REPORT_DIR"
REPORT_DIR="$(cd "$REPORT_DIR" && pwd)"
export UMKIM_THIN_REPORT_DIR="$REPORT_DIR"
export UMKIM_THIN_STRICT_TLS="${UMKIM_THIN_STRICT_TLS:-0}"

echo "==== existing installation test: $TARGET ===="
echo "target: $VHOST  zone: ${ZONE:-}  report: $REPORT_DIR"

# Discover releases rather than assuming the e2e app set is installed.
echo "== installed applications =="
release_json=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/helm list -A -o json 2>/dev/null
EOS
)
APP_GROUPS="${APPS_OVERRIDE:-}"
if [ -z "$APP_GROUPS" ]; then
  APP_GROUPS=$(RELEASES="$release_json" python3 - <<'PY'
import json, os
try:
    releases = {r["name"] for r in json.loads(os.environ["RELEASES"])}
except Exception:
    releases = set()
manifest = json.load(open("ci/apps.json"))
print(" ".join(g for g in manifest if g in releases))
PY
)
fi
export APP_GROUPS
if [ -z "$APP_GROUPS" ]; then
  echo "   no catalog releases found on the target" >&2
  exit 2
fi
echo "   $APP_GROUPS"

if [ "$PIN" = "1" ]; then
  vip="${k3s_control_plane_vip:-}"
  [ -n "$vip" ] || { echo "--pin needs k3s_control_plane_vip in the environment" >&2; exit 2; }
  # Pin every installed app we discovered, not just the e2e ci_apps list.
  # Otherwise an app outside that list resolves to the public address and is
  # rejected by the router's RFC1918 hairpin protection.
  apps_for_pin="$APP_GROUPS"
  hosts="$PORTAL_HOST $CONSOLE_HOST home.$ZONE"
  for a in $apps_for_pin; do
    hosts="$hosts $(ZONE="$ZONE" APPS="$a" python3 - <<'PY'
import json, os
m = json.load(open("ci/apps.json"))
z = os.environ["ZONE"]
for e in m.get(os.environ["APPS"], []):
    print(e["host"].replace("{zone}", z), end=" ")
PY
)"
  done
  # Also pin every Ingress host (Collabora is part of the Nextcloud stack but
  # is not a separate catalog app, so it is absent from ci/apps.json).
  ingress_json=$(node_root <<'EOS'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl get ingress -A -o json 2>/dev/null || true
EOS
)
  ingress_hosts=$(ZONE="$ZONE" INGRESS="$ingress_json" python3 - <<'PY'
import json, os
zone = os.environ["ZONE"]
hosts = set()
try:
    data = json.loads(os.environ.get("INGRESS", "{}"))
except Exception:
    data = {}
for item in data.get("items", []):
    for rule in item.get("spec", {}).get("rules", []):
        h = rule.get("host", "")
        if h and (h == zone or h.endswith("." + zone)):
            hosts.add(h)
print(" ".join(sorted(hosts)))
PY
)
  hosts="$hosts $ingress_hosts"
  # Nextcloud publishes Collabora on a second route that is not a separate
  # catalog entry; pin it even if the chart uses an IngressRoute rather than an
  # Ingress.
  case " $APP_GROUPS " in
    *" nextcloud "*) hosts="$hosts collabora.$ZONE" ;;
  esac
  # shellcheck disable=SC2086
  pin_hosts "$vip" $hosts
fi

echo "== target reachability =="
for h in "$PORTAL_HOST" "$CONSOLE_HOST"; do
  c=$(http "https://$h/")
  for _ in 1 2 3 4 5; do
    case "$c" in 200|302) break ;; esac
    sleep 3
    c=$(http "https://$h/")
  done
  case "$c" in
    200|302) ok "$h -> $c" ;;
    *) bad "$h -> $c" ;;
  esac
done
[ "$fails" -eq 0 ] || { echo "target is not reachable; aborting before tests" >&2; exit 1; }

# Build the app metadata consumed by Playwright from the already-installed
# groups. The app entries themselves are the catalog's tested definitions.
export APPS="$APP_GROUPS"
APPS_JSON=$(ZONE="$ZONE" APPS="$APP_GROUPS" python3 - <<'PY'
import json, os
zone = os.environ["ZONE"]
manifest = json.load(open("ci/apps.json"))
out = []
for group in os.environ["APPS"].split():
    for e in manifest.get(group, []):
        e = dict(e)
        e["host"] = e["host"].replace("{zone}", zone)
        out.append(e)
print(json.dumps(out))
PY
)
printf '%s\n' "$APPS_JSON" > "$REPORT_DIR/apps.json"
export UMKIM_THIN_APPS="$APPS_JSON"
echo "   $APPS_JSON" | head -c 500; echo

pw="$(admin_password)"; mask "$pw"
[ -n "$pw" ] || { echo "no directory admin password available" >&2; exit 2; }
export UMKIM_THIN_USER=admin UMKIM_THIN_PASSWORD="$pw"
export UMKIM_THIN_ZONE="$ZONE" UMKIM_THIN_HOME_HOST="home.$ZONE"
export UMKIM_THIN_AUTH_HOST="$PORTAL_HOST" UMKIM_THIN_CONSOLE_HOST="$CONSOLE_HOST"
export UMKIM_THIN_GITEA_SSH_PORT="${UMKIM_THIN_GITEA_SSH_PORT:-2222}"
portal_user="ci-portal-$run_id"
portal_pw="$(python3 -c 'import secrets,string; print("".join(secrets.choice(string.ascii_letters+string.digits) for _ in range(24)))')"
export UMKIM_THIN_PORTAL_USER="$portal_user" UMKIM_THIN_PORTAL_PASSWORD="$portal_pw"
mask "$portal_pw"

media_fixture_created=0
cleanup_run() {
  cd "$repo_root" 2>/dev/null || return
  if [ "${media_fixture_created:-0}" = "1" ] && [ -n "${media_library:-}" ]; then
    node_root <<EOS >/dev/null 2>&1 || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n servarr exec deploy/sonarr -- \
  sh -c 'rm -rf "$media_library"' 2>/dev/null || true
EOS
  fi
  if [ "$CLEANUP" = "1" ]; then
    cd "$repo_root" && ./ci/cleanup-ci.sh "$TARGET" "$run_id" >/dev/null 2>&1 || true
  fi
}
trap cleanup_run EXIT

# App credentials, read from the existing installation's Secrets, exactly as
# the e2e gate does. Missing secrets are reported by the individual tests.
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
done < <(APPS="$APP_GROUPS" python3 - <<'PY'
import json, os
m = json.load(open("ci/apps.json"))
for app in os.environ["APPS"].split():
    for e in m.get(app, []):
        a = e.get("admin")
        if a:
            print("\t".join([e["name"], a["namespace"], a["secret"],
                             a.get("passwordKey", "admin-password"), a["user"]]))
PY
)
while IFS=$'\t' read -r name ns secret key; do
  [ -n "$name" ] || continue
  val="$(node_root <<EOS | base64 -d 2>/dev/null || true
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get secret $secret -o jsonpath='{.data.$key}' 2>/dev/null || true
EOS
)"
  pre="UMKIM_THIN_$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
  export "${pre}_API_KEY=$val"
done < <(APPS="$APP_GROUPS" python3 - <<'PY'
import json, os
m = json.load(open("ci/apps.json"))
for app in os.environ["APPS"].split():
    for e in m.get(app, []):
        k = e.get("apiKey")
        if k:
            print("\t".join([e["name"], k["namespace"], k["secret"], k["key"]]))
PY
)

# Platform conformance is useful here, but the console restart is an optional
# mutation: existing installations default to skipping it.
if [ "$WITH_RESTART" = "1" ]; then
  ASSERT_SKIP_RESTART=0
else
  ASSERT_SKIP_RESTART=1
fi
export ASSERT_SKIP_RESTART ASSERT_NO_INSTALL=1

platform_rc=0
if [ "$PLATFORM" = "1" ]; then
  echo "== platform conformance =="
  set +e
  ./ci/assert.sh "$TARGET" >"$REPORT_DIR/platform.log" 2>&1
  platform_rc=$?
  set -e
  tail -30 "$REPORT_DIR/platform.log"
  [ "$platform_rc" -eq 0 ] || echo ">> platform conformance failed (see $REPORT_DIR/platform.log)" >&2
fi

echo "== create temporary portal user =="
./ci/portal-user.sh "$TARGET" >"$REPORT_DIR/portal-user.log" 2>&1
tail -5 "$REPORT_DIR/portal-user.log"

cd tests/playwright
[ -d node_modules ] || npm ci --no-audit --no-fund
[ -d "$HOME/.cache/ms-playwright" ] || npx playwright install chromium

echo "== application acceptance gate =="
gate_rc=0
set +e
npx playwright test apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts home.spec.ts \
  --output "$REPORT_DIR/artifacts" >"$REPORT_DIR/gate.log" 2>&1
gate_rc=$?
set -e
tail -35 "$REPORT_DIR/gate.log"
[ "$gate_rc" -eq 0 ] || echo ">> application gate failed (see $REPORT_DIR/gate.log)" >&2

# The e2e suite places a media fixture on the shared volume before journeys.
# Do the same for an existing install under a per-run directory; the Servarr
# journey removes its own Radarr root-folder entry during cleanup.
if [ "$JOURNEYS" = "1" ] && printf ' %s ' "$APP_GROUPS" | grep -q ' servarr '; then
  echo "== media fixture =="
  # Use a per-run path so an existing installation's real libraries and the
  # previous run's root-folder entry are never touched.
  media_library="/media/ci-library-${run_id}"
  export UMKIM_THIN_MEDIA_LIBRARY="$media_library"
  fixture_state=$(node_root <<EOS || true
set -u
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n servarr exec deploy/sonarr -- sh -c \
  'mkdir -p "$media_library" && chmod 0777 "$media_library" && echo "https://example.invalid/ci.mp4" > "$media_library/CI Sample (2020).strm" && chmod 0666 "$media_library/CI Sample (2020).strm"' >/dev/null 2>&1 && echo created || echo unavailable
EOS
)
  case "$fixture_state" in
    created)
      media_fixture_created=1
      export UMKIM_THIN_MEDIA_FIXTURE=1
      echo "   created $media_library"
      ;;
    *)
      export UMKIM_THIN_SKIP_MEDIA_FIXTURE=1
      echo "   media fixture unavailable; the root-folder journey will be skipped" >&2
      ;;
  esac
fi

journey_rc=0
if [ "$JOURNEYS" = "1" ]; then
  echo "== functional user journeys (informational) =="
  set +e
  npx playwright test journeys --workers=1 --reporter=json \
    >"$REPORT_DIR/journeys.json" 2>"$REPORT_DIR/journeys.err"
  journey_rc=$?
  set -e
  [ "$journey_rc" -eq 0 ] || echo ">> journey failures are informational" >&2
fi

# Machine-readable summary plus a human-readable index for the report folder.
REPORT_DIR="$REPORT_DIR" TARGET="$TARGET" ZONE="${ZONE:-}" APPS="$APP_GROUPS" \
PLATFORM_RC="$platform_rc" GATE_RC="$gate_rc" JOURNEY_RC="$journey_rc" \
JOURNEYS="$JOURNEYS" python3 - <<'PY'
import json, os, pathlib, datetime
report = pathlib.Path(os.environ["REPORT_DIR"])
def load(name):
    try:
        return json.loads((report / name).read_text())
    except Exception:
        return None
def failed_titles(data):
    out = []
    def walk(suites):
        for s in suites or []:
            for sp in s.get("specs", []):
                if sp.get("ok") is False:
                    out.append(sp.get("title", ""))
            walk(s.get("suites", []))
    walk((data or {}).get("suites", []))
    return out
gate = load("gate.json")
journeys = load("journeys.json") if os.environ.get("JOURNEYS") == "1" else None
def stats(d):
    return (d or {}).get("stats", {})
summary = {
    "target": os.environ["TARGET"],
    "zone": os.environ["ZONE"],
    "generated": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "apps": os.environ["APPS"].split(),
    "platform_exit_code": int(os.environ["PLATFORM_RC"]),
    "gate_exit_code": int(os.environ["GATE_RC"]),
    "journeys_exit_code": int(os.environ["JOURNEY_RC"]),
    "gate_stats": stats(gate),
    "journeys_stats": stats(journeys),
    "gate_failed": failed_titles(gate),
    "journeys_failed": failed_titles(journeys),
    "report": str(report),
}
(report / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
lines = [
    f"# Existing installation test — {summary['target']}",
    "",
    f"- Zone: `{summary['zone']}`",
    f"- Generated: `{summary['generated']}`",
    f"- Apps: {', '.join(summary['apps'])}",
    f"- Platform conformance: {'PASS' if summary['platform_exit_code'] == 0 else 'FAIL'}",
    f"- Application gate: {'PASS' if summary['gate_exit_code'] == 0 else 'FAIL'}",
]
if journeys:
    lines.append("- Functional journeys: informational "
                 f"({'PASS' if summary['journeys_exit_code'] == 0 else 'FAIL'})")
    lines.append(f"- Journey stats: `{summary['journeys_stats']}`")
lines += ["", "## Gate", "", f"- Stats: `{summary['gate_stats']}`"]
for title in summary["gate_failed"]:
    lines.append(f"- FAILED: {title}")
for title in summary["journeys_failed"]:
    lines.append(f"- JOURNEY FAILED (informational): {title}")
lines += ["", "## Artifacts", "", f"- [HTML report](html/index.html)",
          "- [JUnit](gate.xml)", "- [JSON](gate.json)", "- [Platform log](platform.log)",
          "- [Gate log](gate.log)"]
if journeys:
    lines += ["- [Journey JSON](journeys.json)"]
(report / "summary.md").write_text("\n".join(lines) + "\n")
print("\n".join(lines))
PY

echo "==== existing installation test result ===="
echo "report: $REPORT_DIR"
[ -f "$REPORT_DIR/summary.md" ] && cat "$REPORT_DIR/summary.md"
if [ "$platform_rc" -ne 0 ] || [ "$gate_rc" -ne 0 ]; then
  exit 1
fi
exit 0
