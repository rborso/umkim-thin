# ci — testing

This directory is for maintainers; normal users do not need it. Lab-only
environment details are in [`docs/maintainer-lab.md`](../docs/maintainer-lab.md).

**Tier 1 is manual**; there is no hosted CI workflow. Run it before a
pull request or merge. The lab tiers are manual too. Two tiers:

- **Tier 1 (no cluster):** `ci/static.sh` — format, build,
  vet, race-enabled unit tests, `helm lint` + `helm template` (both console
  modes), app-chart lint, the catalog/chart version check, the real catalog
  contract render (`TestCatalogChartsRender` renders every entry with the
  console's storage/auth/hostAliases values against its pinned chart), chart
  packaging, a check that the charts fail closed on missing values, a "no
  secrets in a ConfigMap" check, the console pod baseline, an optional gitleaks
  scan, and an Ansible syntax check. It also runs `ci/novice-test.sh`, which
  renders the single-server values and checks the dry-run/help paths. No
  cluster.
- **Tier 2 (lab runner, serialised, manual):** `ci/e2e.sh single|ha` — reset a
  target to its clean base, provision it with `deploy/provision.sh`, install the
  catalog apps, and run `ci/assert.sh` plus the Playwright gate. The run also
  creates, verifies and restores a console backup (`ci/backup-check.sh`) before
  the gate. Run `verify-ha.sh` manually before a production HA provision;
  `ci/assert.sh` now also checks that `sudo kubectl` works without the full
  path.
- **Tier 2 extras (lab runner, manual):** `ci/test-existing.sh single|ha` against
  a live install, `ci/backup-check.sh single|ha` on its own, and
  `ci/upgrade-check.sh single|ha`, which re-runs the documented platform
  upgrade and asserts the console rolled, the state survived, the new config
  landed and every app is still ready.

## Two targets

The lab targets are a single-node and a three-node k3s cluster. Their addresses,
VM names and hypervisors are environment-specific and live only on the runner
(`deploy/provision-{single,ha}.env`, gitignored) and in the reset scripts'
`RESET_VMS` input. Copy the `.example` files and replace the placeholders. For
the full operator walkthrough, see [`docs/install.md`](../docs/install.md).

| Target | Shape | Ingress |
|---|---|---|
| `single` | one node | that network's NAT to the published NodePorts |
| `ha` | three servers + kube-vip | the control-plane VIP |

Both set `acme_staging=true`, so runs use the Let's Encrypt staging directory and
never touch the production 50-certs/7-days limit (the certificates are untrusted;
expected in manual lab runs).

## Reset, not uninstall

`ci/reset.sh` reverts each target's VMs to a **`base` snapshot** taken with
`ci/snapshot-base.sh` while they were at the clean base. That is faster and more
deterministic than uninstalling k3s/Calico/Longhorn in place.

For a normal (non-snapshot) host, use `teardown.sh` (single) or
`uninstall-ha.sh` (HA); both handle Helm releases, the host storage agent, k3s,
CNI/WireGuard/firewall state and optional data purge.

The VM -> hypervisor mapping is environment-specific and is **not** in the
repository. Copy `ci/lab.env.example` to `ci/lab.env` (gitignored) and fill in
`CI_SINGLE_VMS` / `CI_HA_VMS`, or set the same names for the run. The e2e
manual runner reads its mapping from `ci/lab.env` or the environment variables
`CI_SINGLE_VMS` / `CI_HA_VMS`. `reset.sh` also accepts
`RESET_VMS="vm@hypervisor ..."` for a one-off run.

## What the runner needs

- Network access to the jumphost and the hypervisors.
- Tooling: `ansible`, `helm`, `kubectl`, `git`, `podman`, `go`, and a
  `wg`/`wg-quick` if you want the tunnel assertion to run.
- SSH keys:
  - a key for the runner → jumphost, e.g. `$HOME/.ssh/id_ed25519`
  - a chart-repo SSH key at `~/.ssh/chart`, when a manual run packages an
    out-of-tree chart
  - the **runner's own** `~/.ssh/id_ed25519`, pre-authorised on the hypervisors
    (used by `ci/reset.sh` to revert the target VMs)
- The VM -> hypervisor mapping in `ci/lab.env` or `CI_SINGLE_VMS` / `CI_HA_VMS`.
- A base snapshot per target (`ci/snapshot-base.sh single|ha`).

## Manual prep (one-time)

```sh
# 1. provision the target VMs in your hypervisor / lab
# 2. reserve the control-plane VIP outside the DHCP pool (HA only)
# 3. confirm the router port-forwards (or the VIP) reach the published ports
# 4. take the clean-base snapshots
./ci/snapshot-base.sh single
./ci/snapshot-base.sh ha

# 5. from then on, a run is:
./ci/e2e.sh single
./ci/e2e.sh ha
```


## ACME check (read-only)

`ci/acme-check.sh <zone> [host ...]` validates public DNS and the certificate
issuer for the ACME hosts. It makes no ACME requests, so it is safe to run
before/after switching an install to production; run with `EXPECTED_IP=` to
also assert where the names point. `install.sh` separately pins and validates
Traefik 80:30080 / 443:30443, and the identity render test ensures the LLDAP
HTTP ingress cannot shadow `/.well-known/acme-challenge`.
