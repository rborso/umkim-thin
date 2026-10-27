# AGENTS — working in umkim-thin

This file is guidance for AI agents (and humans) working in this repo.

## Read the environment first

Environment-specific details — lab hosts, the test runner, keys, snapshots — live
in **`local/`**, which is gitignored and exists only on a machine that has been
set up. **Check it before assuming anything about hosts or wiring:**

- `local/lab-access.md` — the lab (jumphost, VMs, how to reach them)
- `local/ci-env.md` — the CI environment (runner, targets, hypervisors,
  snapshots, keys, zones)

If `local/` is absent, this is a fresh checkout with no lab access — ask rather
than guessing addresses.

## Never commit secrets or environment specifics

Hosts, keys, tokens and the per-environment files (`deploy/provision.env`,
`deploy/provision-{single,ha}.env`, `local/`) are gitignored. Keep them that
way; put new environment info under `local/`.

## New-user path

`install.sh` is the supported single-node Rocky Linux 9/10 path; `docs/quickstart/`
is its documentation. Keep them in sync with the chart values and the console
UI. `ci/novice-test.sh` runs the render/dry-run checks in Tier 1; the lab e2e is
still the authority for a real install.

## Tests

- **Tier 1 (no cluster):** `ci/static.sh` — format, build, vet, unit tests,
  `helm lint`/`template` (including a fail-closed check on required values and
  the catalog contract render), chart packaging, gitleaks, ansible syntax check.
  Run this manually; there is no hosted CI workflow. The lab tiers are also
  manual.
- **Tier 2 (lab):** `ci/e2e.sh single|ha` — reset the target's VMs to their
  `base` snapshot, `deploy/provision.sh`, `ci/assert.sh` (platform), then install
  the apps in `ci_apps` and run the acceptance gate in `tests/playwright`
  (driven by `ci/apps.json`). The gate is
  `apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts`: it uses a regular
  invited tenant (`ci/portal-user.sh`), and `auth.spec.ts` requires every
  installed app to accept a directory login (Gitea/Jellyfin/Nextcloud LDAP,
  Immich/Paperless OIDC) and Gitea's SSH clone/push over port 2222. The
  functional user journeys under `tests/playwright/journeys/` are
  informational. See `docs/testing.md` for the taxonomy and `docs/auth.md` for
  the OIDC limits (Gitea's OAuth2 auto-registration and Nextcloud's
  `user_oidc` are not in the gate).
- **Reset is by snapshot** (`ci/snapshot-base.sh` once, `ci/reset.sh` per run) —
  never by uninstalling k3s/Calico/Longhorn in place.
- The targets use the Let's Encrypt **staging** directory
  (`acme_staging=true`); never enable production ACME in CI (50 certs/7 days).
- Two profiles: the default one (lab CA fallback, tolerant TLS) and an on-demand
  **full** one (`CI_PROFILE=full` / `./ci/e2e.sh <target> --full`): ACME staging
  for real, strict TLS, and the Servarr bootstrap built locally. It refuses to
  start without a passing `ci/preflight.sh`; see `docs/full-build.md`.

## Security

- The hardening plan is `docs/security-plan.md`; the auth matrix (portal, OIDC,
  LDAP, Gitea SSH) is `docs/auth.md`. Read both before changing the console RBAC,
  the builder, the identity stack or the public ports.

## Conventions

- `charts/apps/` is a git submodule pointing at `umkim-charts`; app chart
  changes belong there, followed by a submodule/catalog bump here.

- Small, focused commits with a scope prefix (`app:`, `deploy:`, `ci:`,
  `wireguard:` …).
- Keep the tree green: `go build ./...`, `go vet ./...`, `helm lint charts/*`,
  `./deploy/provision.sh --syntax-check`.
- One tenant, one directory, one cluster — see `docs/design.md` for scope.

## Console and the on-demand bootstrap

- The console (`cmd/umkim-thin`, UI in `cmd/umkim-thin/ui`) is a client of the
  same API; it owns catalog installs, the Applications status view, and the
  media stack's settings/setup. See `docs/console.md` for the API and semantics.
- **The console and servarr bootstrap images are built locally** by the
  installer/console and imported into containerd; no prebuilt internal image is
  required. The bootstrap source is vendored in `images/servarr-bootstrap` and
  baked into the console image: that fixed context is the only thing the builder
  accepts.
- The console creates `servarr-bootstrap` **before** installing the stack (the
  qBittorrent WebUI password comes from it) and generates the app passwords
  once; never regenerate them on reinstall.
- The builder is the only privileged workload: `umkim-thin-build` (the only
  namespace with the privileged PodSecurity label), no ServiceAccount token, a
  digest-pinned kaniko image, no hostPID/hostNetwork, and only the read-only k3s
  paths plus the containerd socket. It imports into the **pinned node's**
  containerd; the Job then runs there with `imagePullPolicy: Never`.
- Secrets are write-only through the API and go to the cluster on stdin. The
  console's audit lines carry the actor and action, never values.
- The single-node storage agent is a **root host systemd service**
  (`umkim-thin-storage`), not a privileged pod. It exposes a fixed
  disk/RAID/NFS operation set over a host-only Unix socket; do not expose the
  socket beyond the host/console, and keep the fixed planner/safety checks.
- Full-profile CI drives the console (`ci/console-servarr.sh`) rather than the
  direct script; `ci/enable-servarr-bootstrap.sh` is the fallback.

## CI networking assumptions (don't "fix" these without reading local/ci-env.md)

- Pod CIDR is `10.244.0.0/16` and Calico's pool is pinned to it; that CIDR feeds
  the firewall, the NFS export and the WireGuard allowed IPs.
- Calico uses **native BGP routing**, not IPIP; firewalld is **off on the HA
  (multi-node) target** because it drops the pod traffic Calico forwards in from
  peers. Single node keeps the host firewall.
- The HA Traefik service carries the control-plane VIP as an `externalIP`, and
  the runner pins the HA hostnames to that VIP in `/etc/hosts`.
- When ACME cannot issue (e.g. the HA zone is not reachable for HTTP-01),
  Traefik serves a **lab CA + wildcard** default certificate
  (`{{ state_dir }}/tls/ca.crt` on the primary). For a browser, import that
  `ca.crt` to trust it (the CI chooses `ignoreHTTPSErrors`).
- CI storage: the single target is local-path (config) + NFS (RWX media); the
  **HA target uses Longhorn** (`longhorn-rwo` RWO + `longhorn-rwx` RWX via the
  share-manager, 2 replicas, `WaitForFirstConsumer`). The app charts' bootstrap
  Jobs now affinity to their server pod's node, so a shared RWO claim works on
  Longhorn (its PVs have no node affinity).
- `ci/e2e.sh` leaves the cluster and apps running for inspection; set
  `SKIP_RESET=1` to reuse an existing environment. Single and HA use different
  VMs, so they run in parallel (a separate work dir per target).
- The `identity`/app NetworkPolicies also allow the node network, because the
  hostNetwork WireGuard gateway masquerades tunnel traffic to the node address.
