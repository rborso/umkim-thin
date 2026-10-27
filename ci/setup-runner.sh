#!/usr/bin/env bash
# One-time (idempotent) setup of the lab runner: the tooling the lab
# scripts need, plus the Playwright browsers. Run as the runner's login user.
set -euo pipefail
cd "$(dirname "$0")/.."

echo ">> packages"
for p in git jq curl tar podman golang ansible-core wireguard-tools nodejs npm nss-tools; do
  rpm -q "$p" >/dev/null 2>&1 || sudo dnf -y install "$p" >/dev/null 2>&1 || echo "  MISSING $p"
done

echo ">> helm / kubectl"
helm_version=3.16.3
kubectl_version=1.32.0
command -v helm >/dev/null 2>&1 || {
  helm_tar="helm-v${helm_version}-linux-amd64.tar.gz"
  curl -fsSL "https://get.helm.sh/${helm_tar}" -o "/tmp/${helm_tar}"
  curl -fsSL "https://get.helm.sh/${helm_tar}.sha256sum" -o "/tmp/${helm_tar}.sha256"
  (cd /tmp && sha256sum -c "${helm_tar}.sha256")
  tar xzf "/tmp/${helm_tar}" -C /tmp
  sudo install -m0755 /tmp/linux-amd64/helm /usr/local/bin/helm
  rm -rf "/tmp/${helm_tar}" "/tmp/${helm_tar}.sha256" /tmp/linux-amd64
}
command -v kubectl >/dev/null 2>&1 || {
  curl -fsSL "https://dl.k8s.io/release/v${kubectl_version}/bin/linux/amd64/kubectl" -o /tmp/kubectl
  curl -fsSL "https://dl.k8s.io/release/v${kubectl_version}/bin/linux/amd64/kubectl.sha256" -o /tmp/kubectl.sha256
  printf '%s  %s\n' "$(cat /tmp/kubectl.sha256)" /tmp/kubectl | sha256sum -c -
  sudo install -m0755 /tmp/kubectl /usr/local/bin/kubectl
  rm -f /tmp/kubectl /tmp/kubectl.sha256
}

echo ">> playwright"
# Playwright's --with-deps assumes a Debian-based distro; on Rocky install the
# Chromium runtime libraries ourselves.
sudo dnf -y install nss atk at-spi2-atk at-spi2-core cups-libs libdrm \
  libXcomposite libXdamage libXfixes libXrandr mesa-libgbm pango alsa-lib \
  libxkbcommon gtk3 liberation-fonts >/dev/null 2>&1 || true
cd tests/playwright
[ -d node_modules ] || npm ci --no-audit --no-fund
npx playwright install chromium

echo ">> done"
