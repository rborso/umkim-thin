#!/usr/bin/env bash
# Minimal self-hosted CI worker. GitHub is only the source: this host mirrors
# it, checks out a commit and runs the lab scripts. No GitHub Actions, no
# GitHub runner, no GitHub secrets.
#
#   worker.sh poll                         # fetch; static on a new commit
#   worker.sh extended single [flags]      # one target
#   worker.sh extended both [flags]        # single and HA in parallel
#   worker.sh static                       # static checks only
#
# Results live in $CI_ROOT/results/<timestamp>-<target>/ (run.log, summary.txt,
# Playwright reports) and are served by the ci-results service.
set -uo pipefail

CI_ROOT="${CI_ROOT:-/srv/ci}"
REPO="${CI_ROOT}/repo.git"
BRANCH="${BRANCH:-main}"
SECRETS="${CI_SECRETS:-/etc/umkim-thin-ci}"
CERT_CACHE="${CI_CERT_CACHE:-/mnt/data/mergerfs/share/ci/certs}"
export CI_ON_JUMP=1 CI_CERT_CACHE
# The jumphost's /tmp is a small tmpfs; the backup tests build archives under
# TMPDIR and require a 2 GiB free-space margin.
export TMPDIR="${TMPDIR_OVERRIDE:-$CI_ROOT/tmp}"

mkdir -p "$CI_ROOT/work" "$CI_ROOT/results" "$CI_ROOT/state" "$CI_ROOT/tmp" "$CERT_CACHE"
ts() { date -u +%Y%m%dT%H%M%SZ; }
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }

fetch() {
  git -C "$REPO" fetch -q origin "+refs/heads/${BRANCH}:refs/heads/${BRANCH}" || return 1
  git -C "$REPO" rev-parse "refs/heads/${BRANCH}"
}

checkout() { # checkout <sha> <dir>
  local sha="$1" dir="$2"
  rm -rf "$dir"
  git clone -q --shared "$REPO" "$dir" || return 1
  git -C "$dir" checkout -q "$sha" || return 1
  git -C "$dir" submodule update --init --recursive -q || return 1
  # The environment files are gitignored and live on the CI host.
  cp "$SECRETS"/provision-*.env "$dir/deploy/" 2>/dev/null || true
  cp "$SECRETS/lab.env" "$dir/ci/lab.env" 2>/dev/null || true
}

index() {
  {
    echo '<!doctype html><meta charset="utf-8"><title>umkim-thin CI</title>'
    echo '<style>body{font:14px system-ui;margin:2rem}td{padding:.25rem .75rem;border-bottom:1px solid #ddd}</style>'
    echo '<h1>umkim-thin CI</h1><p>Self-hosted runs. Newest first.</p><table>'
    echo '<tr><th>run</th><th>target</th><th>commit</th><th>status</th><th>log</th></tr>'
    for d in $(ls -1dt "$CI_ROOT"/results/*/ 2>/dev/null); do
      [ -f "$d/summary.txt" ] || continue
      local name target sha status
      name=$(basename "$d")
      target=$(sed -n 's/^target=//p' "$d/summary.txt" | head -1)
      sha=$(sed -n 's/^sha=//p' "$d/summary.txt" | head -1)
      status=$(sed -n 's/^status=//p' "$d/summary.txt" | head -1)
      echo "<tr><td>$(basename "$d")</td><td>${target}</td><td><code>${sha:0:12}</code></td><td>${status}</td><td><a href=\"$(basename "$d")/run.log\">log</a></td></tr>"
    done
    echo '</table>'
  } >"$CI_ROOT/results/index.html.tmp"
  mv "$CI_ROOT/results/index.html.tmp" "$CI_ROOT/results/index.html"
}

run_one() { # run_one <target> <kind> <sha> [flags...]
  local target="$1" kind="$2" sha="$3"; shift 3
  local dir="$CI_ROOT/work/${target}-${sha:0:12}"
  local out="$CI_ROOT/results/$(ts)-${target}-${kind}"
  if [ "$kind" != static ]; then
    exec 8>"$CI_ROOT/state/${target}.lock"
    flock -n 8 || { log "$target is already running"; return 0; }
  fi
  mkdir -p "$out"
  checkout "$sha" "$dir" || { echo "checkout failed" >"$out/run.log"; echo "status=CHECKOUT-FAILED" >"$out/summary.txt"; return 1; }
  local started ended rc
  started=$(date -u +%s)
  (
    cd "$dir" || exit 1
    if [ "$kind" = static ]; then
      ./ci/static.sh
    else
      UMKIM_THIN_CI_RUN="$(ts)-${target}" ./ci/extended.sh "$target" "$@"
    fi
  ) 2>&1 | tee "$out/run.log"
  rc=${PIPESTATUS[0]}
  ended=$(date -u +%s)
  {
    echo "target=$target"
    echo "kind=$kind"
    echo "sha=$sha"
    echo "started=$started"
    echo "ended=$ended"
    echo "duration=$((ended - started))"
    echo "status=$([ "$rc" -eq 0 ] && echo PASS || echo FAIL)"
  } >"$out/summary.txt"
  [ -d "$dir/tests/playwright/playwright-report" ] && cp -r "$dir/tests/playwright/playwright-report" "$out/" 2>/dev/null || true
  [ -d "$dir/tests/playwright/test-results" ] && cp -r "$dir/tests/playwright/test-results" "$out/" 2>/dev/null || true
  rm -rf "$dir"
  index
  return "$rc"
}

cmd="${1:-poll}"; shift || true
case "$cmd" in
  poll)
    exec 9>"$CI_ROOT/state/poll.lock"; flock -n 9 || exit 0
    sha=$(fetch) || { log "fetch failed"; exit 1; }
    last=$(cat "$CI_ROOT/state/last-static" 2>/dev/null || true)
    if [ "$sha" != "$last" ]; then
      log "new commit ${sha:0:12}; static"
      run_one all static "$sha" && echo "$sha" >"$CI_ROOT/state/last-static"
    fi
    ;;
  static)
    sha=$(fetch) || exit 1
    run_one all static "$sha"
    ;;
  extended)
    target="${1:?usage: worker.sh extended single|ha|both [flags]}"; shift || true
    sha=$(fetch) || exit 1
    if [ "$target" = both ]; then
      rc=0
      run_one single extended "$sha" "$@" & p1=$!
      run_one ha extended "$sha" "$@" & p2=$!
      wait "$p1" || rc=1
      wait "$p2" || rc=1
      exit "$rc"
    fi
    run_one "$target" extended "$sha" "$@"
    ;;
  *)
    echo "usage: worker.sh poll|static|extended single|ha|both [flags]" >&2
    exit 2
    ;;
esac
