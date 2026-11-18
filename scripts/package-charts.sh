#!/bin/bash
# package-charts.sh <dest-dir>
#
# Package the app and platform charts into <dest-dir>, including the N-1 version
# of every app chart so an upgrade can be rolled back offline
# (docs/versioning-plan.md: "Keep the previous stable tarball in the mirror ... so
# an upgrade and a rollback work offline").
#
# The checked-out chart is the current version. The previous version is built from
# git history: find the most recent commit of <chart>/Chart.yaml whose `version`
# differs from the current one, export that tree with `git archive`, and package
# it. That is the version an operator could still have installed, so it is the one
# a rollback needs.
#
# Size: the mirror is seeded into a ConfigMap (1 MiB object cap). A full set is
# ~105 KiB and an N-1 set ~210 KiB, so both fit; the caller still prunes against
# the catalog (prune-chart-mirror.py) and ci/static.sh asserts the budget.
#
# Requires: helm, git, tar. The chart sources must be a git checkout; in a source
# tarball there is no history, so N-1 packaging is skipped with a warning rather
# than silently producing a mirror that claims a rollback it cannot serve.
set -uo pipefail

dest="${1:?usage: package-charts.sh <dest-dir>}"
mkdir -p "$dest"

repo_root=$(cd "$(dirname "$0")/.." 2>/dev/null && pwd) || repo_root=""
[ -n "$repo_root" ] || repo_root=$(pwd)
apps="$repo_root/charts/apps"
helm_bin="${HELM:-helm}"

warn() { printf '  WARN: %s\n' "$*" >&2; }
info() { printf '  %s\n' "$*"; }

chart_version() { # chart_version <chart-dir>
  sed -n 's/^version:[[:space:]]*//p' "$1/Chart.yaml" 2>/dev/null | head -1
}

# previous_version <chart-name> : echo the version before the current one, from
# the submodule's history. Empty when the chart has no older version.
previous_version() {
  local name="$1" cur commit v seen=""
  cur=$(chart_version "$apps/$name")
  [ -n "$cur" ] || return 0
  while read -r commit; do
    [ -n "$commit" ] || continue
    v=$(git -C "$apps" show "$commit:$name/Chart.yaml" 2>/dev/null |
      sed -n 's/^version:[[:space:]]*//p' | head -1)
    [ -n "$v" ] || continue
    [ "$v" = "$cur" ] && continue
    printf '%s' "$v"
    return 0
  done < <(git -C "$apps" log --format=%H -- "$name/Chart.yaml" 2>/dev/null)
}

# package_previous <chart-name> : export the older chart from git and package it.
package_previous() {
  local name="$1" want commit v tmp
  want=$(previous_version "$name")
  [ -n "$want" ] || return 0
  if [ -f "$dest/$name-$want.tgz" ]; then
    return 0
  fi
  # Find the commit that carried this exact version.
  commit=""
  while read -r c; do
    [ -n "$c" ] || continue
    v=$(git -C "$apps" show "$c:$name/Chart.yaml" 2>/dev/null |
      sed -n 's/^version:[[:space:]]*//p' | head -1)
    if [ "$v" = "$want" ]; then commit="$c"; break; fi
  done < <(git -C "$apps" log --format=%H -- "$name/Chart.yaml" 2>/dev/null)
  [ -n "$commit" ] || return 0

  tmp=$(mktemp -d)
  if git -C "$apps" archive "$commit" "$name" 2>/dev/null | tar x -C "$tmp" 2>/dev/null &&
     [ -f "$tmp/$name/Chart.yaml" ]; then
    # The chart's LICENSE/NOTICE live outside the chart dir in this repo; copy the
    # current ones so the packaged N-1 has the same license text.
    for f in LICENSE NOTICE; do
      [ -f "$apps/$f" ] && cp "$apps/$f" "$tmp/$name/" 2>/dev/null
    done
    if "$helm_bin" package "$tmp/$name" --destination "$dest" >/dev/null 2>&1; then
      info "previous $name $want (from ${commit:0:8})"
    else
      warn "could not package previous $name $want"
    fi
  else
    warn "could not export previous $name $want from git"
  fi
  rm -rf "$tmp"
}

if [ ! -d "$apps" ]; then
  warn "charts/apps is missing; nothing to package"
  exit 1
fi

# Platform charts (installed by the playbook, resolved from this mirror).
for d in "$repo_root"/charts/identity "$repo_root"/charts/umkim-thin "$repo_root"/charts/wireguard-gateway; do
  [ -d "$d" ] || continue
  "$helm_bin" package "$d" --destination "$dest" >/dev/null 2>&1 ||
    warn "could not package $(basename "$d")"
done

# App charts: current version, then the previous one from history.
have_history=0
git -C "$apps" rev-parse --git-dir >/dev/null 2>&1 && have_history=1

for d in "$apps"/*; do
  [ -d "$d" ] || continue
  name=$(basename "$d")
  [ -f "$d/Chart.yaml" ] || continue
  # LICENSE/NOTICE live at the submodule root; helm package only wants them inside.
  for f in LICENSE NOTICE; do
    [ -f "$apps/$f" ] && cp "$apps/$f" "$d/" 2>/dev/null
  done
  if ! "$helm_bin" package "$d" --destination "$dest" >/dev/null 2>&1; then
    warn "could not package $name"
  fi
  for f in LICENSE NOTICE; do
    [ -f "$apps/$f" ] && rm -f "$d/$f" 2>/dev/null
  done
  if [ "$have_history" -eq 1 ]; then
    package_previous "$name"
  fi
done

if [ "$have_history" -eq 0 ]; then
  warn "charts/apps is not a git checkout; no N-1 tarballs were packaged, so an"
  warn "offline rollback to the previous version will not be available"
fi

count=$(find "$dest" -name '*.tgz' | wc -l)
info "$count chart packages in $dest"
