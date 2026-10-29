# umkim-thin

Self-host an app stack on a Kubernetes cluster **you already have**: identity
(LLDAP + Authelia), a pinned app catalog, automatic Traefik routes with auth
attached, invite-based onboarding, and optional self-service WireGuard access.

umkim-thin is the single-cluster product. [umkim](https://github.com/rborso/umkim)
is the fleet manager: many clusters, many sites, OCM, Harbor, cluster lifecycle.
umkim-thin
takes the part of umkim that is useful to anyone with one cluster and no fleet —
and nothing else.

## Status

Pre-1.0, but working end to end on single-node and three-node HA k3s test clusters.

Implemented today:

- the catalog format, its validation, and the install/uninstall/status flow
  (`helm upgrade --install` against a pinned chart);
- the identity stack: LLDAP + Authelia, OIDC clients, forward-auth middleware,
  per-app public paths, invitations, and the invite sign-up page;
- WireGuard: an in-cluster gateway, per-client configs, QR codes, and revocation;
- the console/API/CLI, including the Applications status view, storage-class
  discovery, and audit lines;
- local console and bootstrap images built by the installer/console; no
  prebuilt internal image is required;
- app charts from the `umkim-charts` submodule, packaged into the local chart
  mirror;
- Ansible provisioning for a single node and a three-node HA control plane.

Known gaps, deliberately not hidden:

- Nextcloud uses LDAP + forward-auth; `user_oidc` is not enabled yet
  (see [docs/auth.md](docs/auth.md)).
- Gitea's OIDC source is configured, but new-user auto-registration still needs
  the admin auth-source API; the gated test uses LDAP/SSH.
- General directory user management is not a console feature; invitations are.
- The app charts have bootstrap Jobs; only the media stack has the console-driven
  image build/setup flow (the others run their chart Job directly).

## What it gives you

| Piece | What it does |
|---|---|
| **Identity** | LLDAP directory + Authelia. SSO (OIDC) for apps that speak it, forward-auth for apps that do not |
| **Routes** | Every install gets a Traefik route automatically, with auth attached and public paths exempted where the app needs them (share links, native clients) |
| **Catalog** | Curated apps with pinned charts, values, auth mode, and a verification suite that has to pass before an entry is accepted. Today: Gitea, Immich, Nextcloud, Paperless, Mealie, the Servarr media stack, WireGuard, Actual Budget, and HomeBox. SparkyFitness is available as an optional source-available/non-commercial entry |
| **Invitations** | Self-service onboarding: an operator issues a link, the invitee sets their own password |
| **Directory UI** | `lldap.<zone>` behind the portal, for the occasional password reset or removal |
| **WireGuard** | Optional private access: per-client configs created self-service, so apps can stay off the public internet entirely |
| **Home** | A read-only app launcher for household users: installed apps, live availability and links, without console access |
| **Console + CLI** | The same operations from a browser or a terminal |

## Non-goals

No managed fleet/cluster lifecycle, no multi-cluster fleet, no hub/spoke
topology, no OCM, no Harbor, no per-tenant directories. Optional single/HA
Ansible provisioning is supported; one cluster, one directory, one tenant.

## Quick start (one server)

For a first install on a fresh Rocky Linux 9/10 host, use the beginner path —
the one-page version is [`QUICKSTART.md`](QUICKSTART.md):

```bash
git clone --recurse-submodules https://github.com/rborso/umkim-thin.git
cd umkim-thin
sudo ./install.sh --zone example.com --email you@example.com
```

It installs k3s, Traefik, the identity stack, and the console, prints the first
login, and keeps the generated secrets on the host. The installer builds and
imports the console image locally by default; `--image` is only for an
already-available image. The
walkthrough — DNS, first login, invitations, the first app, WireGuard, backups,
troubleshooting and uninstall — is in
[`docs/quickstart/`](docs/quickstart/README.md). `--dry-run` checks the host and
prints the plan without changing anything. To remove the install, including
k3s, run `sudo ./teardown.sh`; for HA use
`./uninstall-ha.sh --env deploy/provision-ha.env` (see
[`docs/quickstart/09-uninstall.md`](docs/quickstart/09-uninstall.md)).

The Ansible path below is the advanced/HA path; it assumes a cluster and a
management/jump host, and supports the three-node HA topology.

## Install

### Option 1: provision a host (single node or HA)

The supported path takes a minimal RHEL-family host to a running install:
prerequisites, firewall, k3s, Traefik, the identity stack, the console, and
storage.

```bash
# single node
cp deploy/provision-single.env.example deploy/provision-single.env
PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh

# HA: prompts, writes deploy/provision-ha.env, verifies it, then runs
./install-ha.sh
```

For the three-node HA path, `./install-ha.sh` prompts for the domain, server
range, VIP, optional NFS export and TLS mode, writes
`deploy/provision-ha.env`, and can start the same provisioner. To remove that site, use
`./uninstall-ha.sh --env deploy/provision-ha.env`; it runs the teardown on every
server with the primary last.

The target must be reachable from the machine named `JUMP` in the selected
environment file. For the full walkthrough — prerequisites, DNS, ports, first
login, invitations, app installs and the HA differences — see
[docs/install.md](docs/install.md). The console is then at
`https://console.<zone>`.

### Option 2: build the binary and run it against an existing cluster

The binary needs `helm` and `kubectl` on `PATH`, a kubeconfig, and a Traefik
ingress. The identity chart must already be installed (the provisioned path does
that for you); app installs add the OIDC clients and route policies to it.

```bash
go build ./...

# The catalog's OCI refs are metadata, not a published registry. Package the
# vendored charts so the installer uses the local mirror.
mkdir -p "$HOME/.umkim-thin/chart-mirror"
for chart in charts/apps/*; do
  helm package "$chart" --destination "$HOME/.umkim-thin/chart-mirror"
done

./umkim-thin catalog list
./umkim-thin catalog show nextcloud

export UMKIM_THIN_ZONE=example.com
export UMKIM_THIN_KUBECONFIG=$HOME/.kube/config
export UMKIM_THIN_STATE_DIR=$HOME/.umkim-thin
export UMKIM_THIN_CHART_MIRROR=$HOME/.umkim-thin/chart-mirror
./umkim-thin serve --addr :8080
```

Configuration is read from the environment and, when present, from
`/etc/umkim-thin/config.env` (the provisioned install writes it). The CLI and
the API read the same keys.

### About app charts

The platform charts (`identity`, `umkim-thin`, `wireguard-gateway`) are in this
repository. The app charts are maintained separately in
[`umkim-charts`](https://github.com/rborso/umkim-charts) and pinned here as the
`charts/apps/` git submodule. Each `catalog/*.yaml` entry pins a canonical OCI
reference and an exact chart version; the installer resolves the local package
from the submodule by name and version, so a fresh checkout can install the
catalog without a registry. A fork can point `chart.ref` at its own charts or
replace the submodule.

An entry is not accepted unless its `verify` suite passes. See
[docs/catalog.md](docs/catalog.md).

## Layout

```
charts/identity/            LLDAP + Authelia + Traefik middleware + invite page
charts/umkim-thin/          the console/API chart (Deployment, Service, Ingress)
charts/wireguard-gateway/   in-cluster WireGuard gateway (peers from a Secret)
charts/apps/                app-chart submodule (umkim-charts)
catalog/                    one YAML per app: chart, values, route, auth, verify
cmd/umkim-thin/             the single binary: API + console + CLI
internal/                   catalog loading, route/auth rendering, identity, WireGuard
deploy/                     Ansible provisioning: prereqs, firewall, k3s, ingress, app
install.sh                  one-command single-node installer (Rocky Linux 9/10)
docs/                       design, catalog format, auth model, console, security plan
docs/quickstart/            the beginner path: install, login, invite, app, backup
tests/playwright/           acceptance specs and functional user journeys
tests/e2e/                  the Tier 2 runner and migration check
local/                      environment-specific notes and keys (gitignored)
```

## Build and test

```bash
go build ./...
go test ./...
./ci/static.sh          # Tier 1: fmt, build, vet, test, helm, gitleaks, ansible
```

`ci/static.sh` is the manual Tier 1 gate. Run it before opening a pull request or
merging. It needs `helm`; `ansible-playbook` and `gitleaks` are used when
installed and skipped otherwise. The catalog contract test renders every entry
with the console's values against the chart it pins, and the chart checks assert
that the identity and console charts **refuse** to render without their required
values.

Tier 2 is the manual reference-lab E2E run: it resets a target, provisions it,
installs
the catalog apps, and runs the acceptance gate as a regular invited tenant
(including a directory login for every app and Gitea SSH clone/push).

```bash
./ci/e2e.sh single       # maintainer lab; needs deploy/provision-single.env
./ci/e2e.sh ha
```

The acceptance gate is `apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts`;
the functional user journeys under `tests/playwright/journeys/` are
informational. See [docs/testing.md](docs/testing.md) for the test taxonomy.

To verify an installation that already exists without resetting or provisioning
it, use the manual existing-installation runner (it discovers installed apps and
writes HTML/JSON/JUnit reports under `tests/reports/`):

```bash
./ci/test-existing.sh single
./ci/test-existing.sh ha --env deploy/provision-ha.env --pin
./ci/test-existing.sh single --journeys   # optional user journeys
```
The reference lab uses Let's Encrypt **staging** and a local CA fallback; never
enable production ACME in test runs. See [docs/full-build.md](docs/full-build.md).

## Security

The console is a cluster-admin-adjacent component by design: Helm has to install
arbitrary charts, and the optional on-demand builder needs the node's containerd
socket. The threat model, the current state, and the hardening work are in
[docs/security-plan.md](docs/security-plan.md). Read it before exposing the
console to the internet. The console's RBAC is intentionally broad today; the
builder is the only privileged workload.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow and catalog
rules, and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community expectations.
Report a vulnerability privately (do not open a public issue); see
[SECURITY.md](SECURITY.md). The current hardening status is in
[docs/security-plan.md](docs/security-plan.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE), and
[docs/licenses.md](docs/licenses.md) for the full platform/catalog license
matrix.

The project deploys third-party software (LLDAP, Authelia, Traefik, the catalog
apps) under their own licenses. It does not link or redistribute their source;
each is pulled from its upstream image or chart. The charts and the bootstrap
image in this repository are Apache-2.0.
