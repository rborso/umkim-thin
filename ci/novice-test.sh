#!/usr/bin/env bash
# Tier 1 checks for the one-command installer. No root and no cluster.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f charts/apps/gitea/Chart.yaml ] || {
  echo "charts/apps submodule is missing; run: git submodule update --init --recursive" >&2
  exit 1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok    $*"; }

echo "== installer syntax =="
bash -n install.sh
ok "bash -n install.sh"
if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck -S warning install.sh; then
    ok "shellcheck"
  else
    echo "  warn  shellcheck reported issues (not fatal for this test)" >&2
  fi
fi

echo "== help =="
./install.sh --help >/dev/null
ok "--help"

if [ -x teardown.sh ]; then
  ./teardown.sh --help >/dev/null
  ok "teardown --help"
  ./teardown.sh --dry-run >/dev/null 2>&1
  ok "teardown dry-run"
else
  fail "teardown.sh is missing or not executable"
fi
if [ -x uninstall-ha.sh ]; then
  ./uninstall-ha.sh --help >/dev/null
  ok "uninstall-ha --help"
else
  fail "uninstall-ha.sh is missing or not executable"
fi
if [ -x install-single.sh ]; then
  bash -n install-single.sh
  ok "bash -n install-single.sh"
  ./install-single.sh --help >/dev/null
  ok "install-single --help"
  # Exercise the prompts and env rendering without touching a host.
  printf '%s\n' \
    example.com \
    192.0.2.10 \
    '' \
    '' \
    admin@example.com \
    '' \
    '' \
    1 \
    '' '' '' '' '' '' '' |
    ./install-single.sh --dry-run >/dev/null
  ok "install-single dry-run"
else
  fail "install-single.sh is missing or not executable"
fi

echo "== render-only =="
./install.sh --render-only --zone example.com --email admin@example.com --out "$tmp/render" >/dev/null
for f in identity-values.yml console-values.yml config.env traefik-config.yaml; do
  [ -s "$tmp/render/$f" ] || fail "render-only did not write $f"
done
ok "rendered all four files"

# The rendered values must be usable by the charts, not just syntactically YAML.
helm template identity charts/identity -f "$tmp/render/identity-values.yml" >/dev/null
ok "identity values render"
helm template console charts/umkim-thin -f "$tmp/render/console-values.yml" >/dev/null
ok "console values render"
python3 - "$tmp/render/traefik-config.yaml" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    d = yaml.safe_load(f)
assert d["kind"] == "HelmChartConfig", d
values = d["spec"]["valuesContent"]
assert "certificatesresolvers.letsencrypt.acme.email" in values
assert "identity-traefik" in values
assert "acme.httpchallenge.entrypoint=web" in values, "ACME must use HTTP-01"
assert "allowACMEByPass: true" in values, "the web entrypoint must let ACME bypass the redirect"
assert "nodePort: 30080" in values, "Traefik web must pin 30080 for the router"
assert "nodePort: 30443" in values, "Traefik websecure must pin 30443 for the router"
assert "nodePort: 30444" in values, "Traefik vpn must pin the internal 30444 port"
print("  ok    traefik config parses")
PY

grep -q '^UMKIM_THIN_ZONE=example.com$' "$tmp/render/config.env" || fail "zone missing from config.env"
grep -q '^UMKIM_THIN_IDENTITY_CHART=/data/chart-mirror/identity-' "$tmp/render/config.env" || fail "identity chart path missing"
grep -q '^consoleHost: "console.example.com"$' "$tmp/render/console-values.yml" || fail "console host missing"
grep -q '^  portalHost: "auth.example.com"$' "$tmp/render/identity-values.yml" || fail "portal host missing"
ok "generated config has the expected values"

mode=$(stat -c '%a' "$tmp/render/identity-values.yml")
[ "$mode" = "600" ] || fail "identity-values.yml mode is $mode, want 600"
ok "identity values are 0600"

echo "== chart mirror names =="
mirror="$tmp/mirror"
mkdir -p "$mirror"
for d in charts/identity charts/umkim-thin charts/wireguard-gateway charts/apps/*; do
  [ -d "$d" ] || continue
  helm package "$d" --destination "$mirror" >/dev/null
done
for f in catalog/*.yaml; do
  ref=$(awk '/^chart:/{c=1} c && /^  ref:/{print $2; exit}' "$f")
  ver=$(awk '/^chart:/{c=1} c && /^  version:/{print $2; exit}' "$f")
  name=${ref##*/}
  [ -f "$mirror/$name-$ver.tgz" ] || { echo "missing chart package: $name-$ver.tgz" >&2; exit 1; }
done
ok "every catalog entry has a packaged chart"

echo "== input validation =="
if ./install.sh --render-only --zone invalid --email a@b.com --out "$tmp/bad" >/dev/null 2>&1; then
  fail "an invalid zone was accepted"
fi
if ./install.sh --render-only --zone example.com --out "$tmp/bad" >/dev/null 2>&1; then
  fail "a missing email was accepted"
fi
ok "invalid input is rejected"

echo "== dry run =="
out=$(./install.sh --dry-run --zone example.com --email admin@example.com --skip-preflight)
printf '%s\n' "$out" | grep "dry run" >/dev/null || fail "dry run did not print the plan"
ok "dry run prints a plan"

echo "== doctor without a cluster =="
# It must explain the failure, not crash.
if ./install.sh --doctor >"$tmp/doctor" 2>&1; then
  # A developer machine may have a live cluster; both outcomes are valid as
  # long as the command did not crash.
  :
fi
grep -qE "FAIL|ok" "$tmp/doctor" || fail "doctor produced no checks"
ok "doctor runs without a cluster"
