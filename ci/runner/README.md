# Self-hosted CI (minimal)

GitHub is the source; this host (the jumphost) mirrors it and runs the lab
scripts. There is no GitHub Actions workflow and no GitHub runner.

## Install

```bash
# environment files (gitignored):
sudo mkdir -p /etc/umkim-thin-ci
sudo install -m 600 deploy/provision-single.env /etc/umkim-thin-ci/provision-single.env
sudo install -m 600 deploy/provision-ha.env     /etc/umkim-thin-ci/provision-ha.env
sudo install -m 600 ci/lab.env                  /etc/umkim-thin-ci/lab.env

sudo ./ci/runner/install.sh
```

`install.sh` creates `/srv/ci`, mirrors `github.com/rborso/umkim-thin`, installs
`worker.sh`, and enables:

| Unit | When | What |
|---|---|---|
| `ci-poll.timer` | every 5 min | fetch `main`; run `ci/static.sh` on a new commit |
| `ci-nightly.timer` | 02:00 | `extended ha` |
| `ci-weekly.timer` | Sunday 06:00 | `extended ha --regen-certs` |
| `ci-results.service` | always | results on `http://<host>:8080/` |

The timers run the HA target only: the jumphost can reach the lab HA nodes but
not the dev-single VPS, and the lab single VM belongs to its owner. Run
`extended single` from the agent host that can reach that VPS.

## On demand

```bash
/srv/ci/worker.sh static
/srv/ci/worker.sh extended single
/srv/ci/worker.sh extended ha --skip-faults
/srv/ci/worker.sh extended both --regen-certs
```

## Results

`/srv/ci/results/<timestamp>-<target>/` contains `run.log`, `summary.txt` and
the Playwright report. `index.html` is regenerated after every run.

## Certificates

`CI_CERT_CACHE` (default `/mnt/data/mergerfs/share/ci/certs`) stores Traefik's
`acme.json` per zone. Normal runs restore it before the apps are installed; the
weekly run uses `--regen-certs`, lets ACME issue fresh certificates and saves
them again.

## Environment

The extended run expects `CI_ON_JUMP=1` (the worker sets it): the scripts run on
the jumphost, use its `~/.ssh/id_ed25519` for the targets and hypervisors, and
`deploy/provision.sh` runs the playbook locally instead of over SSH.

`ci/extended.sh` order: install/gate/journeys → markers → Servarr download →
VPN → faults (VIP power-off, graceful/hard reboots, full stop/start) → backup,
delete, restore, verify → certificate save → cleanup.
