#!/usr/bin/env bash
# Install the minimal self-hosted CI on this host (the jumphost).
#
#   sudo ./ci/runner/install.sh
#
# It creates /srv/ci, mirrors GitHub there (read-only), installs the worker and
# three systemd timers (poll/static, nightly extended, weekly extended with
# certificate regeneration), and serves the results on :8080.
#
# The environment files are gitignored and must exist first:
#   /etc/umkim-thin-ci/provision-single.env
#   /etc/umkim-thin-ci/provision-ha.env
#   /etc/umkim-thin-ci/lab.env
set -euo pipefail
cd "$(dirname "$0")/../.."

CI_ROOT="${CI_ROOT:-/srv/ci}"
CERT_CACHE="${CI_CERT_CACHE:-/mnt/data/mergerfs/share/ci/certs}"
SECRETS="${CI_SECRETS:-/etc/umkim-thin-ci}"
REPO_URL="${REPO_URL:-https://github.com/rborso/umkim-thin.git}"
USER_NAME="${CI_USER:-linux}"

echo ">> directories"
sudo mkdir -p "$CI_ROOT"/{work,results,state} "$CERT_CACHE" "$SECRETS"
sudo chown -R "$USER_NAME:$USER_NAME" "$CI_ROOT" "$CERT_CACHE" "$SECRETS"

echo ">> mirror"
if [ ! -d "$CI_ROOT/repo.git" ]; then
  sudo -u "$USER_NAME" git clone --mirror "$REPO_URL" "$CI_ROOT/repo.git"
else
  sudo -u "$USER_NAME" git -C "$CI_ROOT/repo.git" remote set-url origin "$REPO_URL"
fi

echo ">> worker"
sudo install -m 0755 ci/runner/worker.sh "$CI_ROOT/worker.sh"
sudo -u "$USER_NAME" "$CI_ROOT/worker.sh" static || true

for f in provision-single.env provision-ha.env lab.env; do
  if [ ! -f "$SECRETS/$f" ]; then
    echo "   WARN: $SECRETS/$f is missing; runs that need it will fail" >&2
  fi
done

echo ">> systemd units"
sudo tee /etc/systemd/system/ci-poll.service >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: poll GitHub and run the static checks
After=network-online.target

[Service]
Type=oneshot
User=linux
Group=linux
WorkingDirectory=/srv/ci
ExecStart=/srv/ci/worker.sh poll
EOF

sudo tee /etc/systemd/system/ci-poll.timer >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: poll every five minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=5min
Unit=ci-poll.service

[Install]
WantedBy=timers.target
EOF

sudo tee /etc/systemd/system/ci-nightly.service >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: nightly extended run (single and HA in parallel)
After=network-online.target

[Service]
Type=oneshot
User=linux
Group=linux
WorkingDirectory=/srv/ci
ExecStart=/srv/ci/worker.sh extended both
TimeoutStartSec=8h
EOF

sudo tee /etc/systemd/system/ci-nightly.timer >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: nightly extended run

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true
Unit=ci-nightly.service

[Install]
WantedBy=timers.target
EOF

sudo tee /etc/systemd/system/ci-weekly.service >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: weekly full run with certificate regeneration
After=network-online.target

[Service]
Type=oneshot
User=linux
Group=linux
WorkingDirectory=/srv/ci
ExecStart=/srv/ci/worker.sh extended both --regen-certs
TimeoutStartSec=10h
EOF

sudo tee /etc/systemd/system/ci-weekly.timer >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: weekly full run (Sunday)

[Timer]
OnCalendar=Sun *-*-* 06:00:00
Persistent=true
Unit=ci-weekly.service

[Install]
WantedBy=timers.target
EOF

sudo tee /etc/systemd/system/ci-results.service >/dev/null <<'EOF'
[Unit]
Description=umkim-thin CI: results page on :8080
After=network-online.target

[Service]
User=linux
Group=linux
ExecStart=/usr/bin/python3 -m http.server 8080 --bind 0.0.0.0 --directory /srv/ci/results
Restart=always

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now ci-poll.timer ci-nightly.timer ci-weekly.timer ci-results.service
echo ">> installed; results: http://$(hostname -f 2>/dev/null || hostname):8080/"
