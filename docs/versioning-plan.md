# Versioning and upgrade plan

This is the implementation plan for **pinned, catalogued versions** and
**deliberate upgrades**. The target is one active version per app, several
tested versions available, and no floating image tags.

> **Status: core implemented.** Phases 0–2 and the exposure/certificate
> routing are in the tree: exact pins, catalog versions, `app versions`,
> `app upgrade --to` with an atomic Helm upgrade, manual per-route/default
> certificates, the `public`/`authelia`/`vpn` exposure ladder, the internal VPN
> entrypoint and CoreDNS zone. Remaining: a certificate API/console section,
> the automated N-1 upgrade matrix, and the platform-upgrade track.
>
> **Delivery note:** this plan predates the local-artifact policy. Internal
> console/bootstrap images are built locally, and charts are packaged from the
> `charts/apps` submodule into the local mirror. The OCI/publish references
> below are historical options, not a required release pipeline.

## Why

The current install is mostly reproducible, but not completely:

- The catalog pins a **chart** version and `app install` runs
  `helm upgrade --install`, so an install is already an upgrade.
- Five images float: `immich-server:release`, `prowlarr:latest`,
  `seerr:latest`, `flaresolverr:latest`, `lldap:stable`.
- Several sidecars use major-only tags: `postgres:18`, `valkey:9`,
  `nginx:1.27-alpine`, `alpine:3.20`, `busybox:1.36`.
- The catalog has **one** version per app. There is no list of available
  versions, no `--version`, no N-1 retention, and no rollback contract.
- Helm runs with `--wait`, not `--atomic`; a failed upgrade can leave a
  `pending-upgrade` release.
- Chart `appVersion` and the image tag are not always in sync (Traefik is the
  clearest example: chart appVersion `v3.7.1`, image `3.7.13`).

## Goal

1. Every install is reproducible from exact image tags/digests and a chart
   version.
2. Upgrading is explicit: pick a tested chart/app version; never a floating
   tag and never an implicit "latest".
3. The console and CLI show current and available versions.
4. An upgrade has a defined safety contract: backup confirmation, atomic Helm,
   health checks, rollback, and a data-restore path for stateful apps.
5. Promotion is test-gated: a candidate becomes stable only after fresh
   install, N-1 upgrade, app journey and backup/restore checks pass.
6. Keep the **previous stable** version available for rollback.

## Non-goals

- Multiple live versions of the same app side by side. This is one tenant and
  one namespace per app; side-by-side stateful apps are not worth the
  complexity.
- Auto-updating to the newest upstream release. Discovery may be automated;
  deployment is not.
- Automatic data migrations across major versions. The app's migration runs,
  but the pre-upgrade snapshot is the recovery path.
- Bundling platform upgrades (k3s, Helm, Traefik) into app upgrades. They get
  their own track and gate.

## Version model

Use four distinct things and never conflate them:

| Thing | Example | Source of truth |
|---|---|---|
| Platform version | k3s `v1.36.5+k3s1`, Helm `3.16.3` | installer / `install.sh` |
| Chart version | `nextcloud-0.1.16` | `Chart.yaml` `version` |
| App version | Nextcloud `35.0.1` | `Chart.yaml` `appVersion` |
| Image | `nextcloud:35.0.1-apache` | chart values, exact tag/digest |

Rules:

- **A chart version is an immutable bundle.** Any image, sidecar, default,
  template or wiring change produces a new chart version, including a patch
  image bump. Never reuse the same version for different contents.
- **`appVersion` matches the primary app.** For bundles (`servarr`) the chart
  version is the bundle; list the component versions in the chart README and
  in the catalog `appVersion` note.
- **Exact pins only.** No `latest`, `release`, `stable`, and no major-only
  tags where a patch/digest exists. For an upstream that has no semver, pin
  the digest and record the upstream release in a comment.
- **`imagePullPolicy: IfNotPresent`** with exact tags/digests. The current
  charts already mostly do this.
- **One stable pointer.** The catalog's `version` is the default stable chart;
  the `versions` list is the set of tested versions. `candidate` is visible but
  not offered as the default.

## Catalog schema

Extend `catalog.ChartRef` (or add a sibling `Versions` list) without breaking
existing entries:

```yaml
chart:
  ref: oci://ghcr.io/rborso/charts/nextcloud
  version: 0.1.17          # stable default, must be in versions
  versions:
    - version: 0.1.16
      appVersion: 35.0.1
      channel: stable
    - version: 0.1.17
      appVersion: 35.0.2
      channel: stable
    - version: 0.1.18
      appVersion: 35.1.0
      channel: candidate
      notes: "requires PHP 8.4; run the app upgrade"
```

Validation:

- `chart.version` is present and appears in `versions`.
- Every `versions[].version` is valid semver; every `appVersion` is non-empty.
- A stable version is not also a candidate; channels are `stable` or
  `candidate`.
- The chart tarball for each advertised version exists in the mirror/OCI and
  its `Chart.yaml` `version`/`appVersion` match the catalog entry. The static
  check enforces this.
- The chart mirror and catalog are generated together; no hand-edited mismatch.

### Where the versions live

- Publish each chart version immutably to the OCI registry
  (`ghcr.io/rborso/charts/<app>:<version>`), tagged by chart version.
- `install.sh` continues to package the **current** chart into
  `chart-mirror/` for an offline/lab install. Keep the **previous stable**
  tarball in the mirror (and in the catalog `versions`) so an upgrade and a
  rollback work offline.
- The console prefers `chart-mirror/<name>-<version>.tgz`; if it is absent,
  use the OCI ref/version. This is already the shape of `chartRef` in
  `internal/app/app.go`; it only needs the selected version.
- **Fix the OCI fallback before relying on it:** the current `chartRef` returns
  the OCI ref but `Installer.Install` does not pass `--version`, so an absent
  mirror tarball would pull the newest chart. Every `helm` invocation must
  carry the selected `--version` (or the tarball must be present).
- The catalog itself is baked into the console image
  (`/opt/umkim-thin/catalog`). A version-index change is a **console image
  rebuild**, not only a chart bump; build the console image locally and keep
  the installer's pinned tag in step.
- Do not delete the N-1 tarball on the 30-day `find -mtime` rule. Retain
  catalogued versions explicitly; prune only versions no longer in any
  catalog.

## CLI and API

### CLI

```text
umkim-thin app install <name> --version <chart-version>
umkim-thin app versions <name>
umkim-thin app upgrade <name> --to <chart-version> [--dry-run] [--yes]
umkim-thin app status <name>       # includes chart and appVersion
```

`app versions` output:

```text
nextcloud
  installed  0.1.16  (35.0.1)
  stable     0.1.17  (35.0.2)
  candidate  0.1.18  (35.1.0)
```

`app upgrade` must require `--to`; there is no implicit stable/default. For a
stateful app it prints the backup command and refuses without `--yes` after
showing the plan.

### API

Add:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/apps/{name}/versions` | installed + available versions |
| `POST` | `/api/apps/{name}/upgrade` | body `{ "version": "0.1.17", "confirmBackup": true }`; async job |
| `GET` | `/api/apps` | include `chart`, `appVersion`, `upgradeAvailable` |

Keep `POST /api/apps` for a fresh install; accept an optional `version`. The
job record carries old/new chart and app versions, the Helm revision, duration
and the log.

### Console UI

- Applications table: **Version** column (chart + app) and an **Upgrade**
  action when a stable/candidate version is newer.
- Upgrade dialog: current → target, release notes/warnings, the backup
  command, `confirmBackup` checkbox, then a job with progress/log.
- No "latest" label anywhere; show explicit versions.

## Upgrade mechanics

The upgrade is a separate operation from install so the safety checks cannot
be skipped by accident.

1. **Resolve** the target from the catalog `versions`; require the chart
   tarball and a matching `appVersion`.
2. **Preflight**: app is `deployed` and ready; no pending Helm operation; the
   target is newer or explicitly allowed; disk/cluster healthy; the chart's
   required values still render.
3. **Backup gate**:
   - Single-node/local-path: a VM snapshot, or `install.sh --backup` plus the
     VM snapshot for PVC data. The CLI prints the exact command; the UI shows
     it; `confirmBackup` records the operator's confirmation.
   - HA/Longhorn: a volume snapshot plus the etcd/state backup; later a
     pre-upgrade snapshot Job can automate the Longhorn part.
   - A backup-confirmation is required for stateful apps; `--skip-backup` is an
     explicit override and is audited.
4. **Upgrade**: `helm upgrade --install --atomic --cleanup-on-fail --wait
   --timeout 15m` with the stored app values, re-rendered for the target
   chart. `--atomic` rolls back the release on failure.
5. **Post-check**: Helm deployed; all Deployments/StatefulSets ready; Jobs
   complete; routes answer; app-specific smoke (auth redirect, OIDC discovery,
   WOPI path, API health). Use the catalog `verify.checks` where possible.
6. **Record**: Helm revision, old/new versions, result, log; audit line with
   the actor/action, never secret values.
7. **On failure**: if `--atomic` rolled back, verify the old version is healthy.
   If a data migration already ran, restore the pre-upgrade snapshot; a Helm
   rollback alone is not a data rollback.

The console already writes the app values to
`/data/apps/<name>-values.yaml`; the upgrade must regenerate them so route,
OIDC and public-path changes in the new chart are applied. Do not use
`--reuse-values` as the primary path; it defeats chart defaults and new
required values.

## Safety and compatibility rules

- **Upgrade path:** N-1 stable → N stable is supported and tested. Skipping a
  major version is not supported without app-specific notes and a restore
  plan.
- **Major app upgrades** (Jellyfin 10 → 12, Radarr 5 → 6, etc.) start on the
  candidate channel with a data backup; promote only after the app journey and
  a restore drill.
- **Downgrades** are only supported with a matching data restore. The UI/CLI
  must say so; do not imply that `--to <old>` reverses a schema migration.
- **Platform versions** (k3s, Helm, Traefik) have their own upgrade runbook and
  are not changed by an app upgrade. In particular, Helm 4 and the Traefik
  chart major are separate changes.
- **One active chart version per app.** The Helm release is the source of
  truth; the catalog is the available set.

## Phase 0: pin what floats (no behavior change)

This is the first commit and can land before the version-list feature.

| Chart | Current | Pin |
|---|---|---|
| immich | `immich-server:release` | `v3.3.5` + digest |
| servarr | `prowlarr:latest` | `2.6.5.5623` |
| servarr | `seerr:latest` | `3.5.1` |
| servarr | `flaresolverr:latest` | resolve the 3.5.2/3.5.5 discrepancy; pin the tested digest |
| identity | `lldap:stable` | `0.6.3` + digest |
| nextcloud/gitea/paperless | `postgres:18` | `18.6` |
| all | `valkey:9` / `9-alpine` | `9.1.2` / `9.1.2-alpine` |
| identity | `nginx:1.27-alpine` | `1.27.5-alpine` |
| bootstrap/init | `alpine:3.20`, `busybox:1.36` | exact patch/digest |
| wireguard | `alpine:3.20` | exact patch/digest |

Also:

- Bump the chart version for every chart whose values change; bump the catalog
  `chart.version` to match.
- Set `appVersion` to the real primary app version (`servarr` stays a bundle).
- Add a Tier 1 check that rejects `latest`, `release`, and `stable` image tags
  in `charts/**` and catalog values, with an allowlist only for the console's
  local `novice` build.
- Re-run `ci/static.sh`, `ci/novice-test.sh`, `helm lint`, and a fresh lab
  install.

**Acceptance:** no floating app image tags; all charts render; the current
lab install is still green; the installed versions are unchanged (this phase
pins, it does not upgrade).

## Phase 1: version index and read-only visibility

- Add `versions` to the catalog schema and validation; per-app files for the
  nextcloud/gitea/immich/paperless/servarr/wireguard/identity entries.
- Generate/validate the index from chart tarballs; add
  `ci/check-versions.sh` (or a section in `ci/static.sh`) that fails on a
  missing tarball, duplicate version, mismatched `appVersion`, or a stable
  pointer not in the list.
- Add `GET /api/apps/{name}/versions`, extend `GET /api/apps` and
  `app status` with `appVersion` and `upgradeAvailable`.
- Add `app versions <name>`.
- Pass the selected chart version to Helm on every path, including the OCI
  fallback (`--version`), and test with the mirror tarball removed.
- Retain the N-1 chart tarball in the mirror and OCI; document the retention.
- No upgrade action yet.

**Acceptance:** `app versions nextcloud` shows installed/stable/candidate;
the API returns the same; a missing/old tarball fails the pre-release check; the
install path still uses the stable pointer.

## Phase 2: upgrade execution

- Refactor `Installer.Install` so the chart version is an argument; add
  `Installer.Upgrade(name, target, confirmBackup)`.
- Use `--atomic --cleanup-on-fail`; re-render values; wait for readiness.
- Add the preflight and backup-confirmation gate (CLI first).
- Add post-upgrade smoke checks from `verify.checks` and the route/auth
  probes.
- Add `POST /api/apps/{name}/upgrade` as an async job; job log and audit.
- Add a rollback path: `app upgrade <name> --to <previous>` plus explicit
  data-restore notes; test an induced failure.
- Keep the previous Helm revision and chart tarball.

**Acceptance:** a successful N-1 → N upgrade; a failed upgrade rolls back and
the app remains healthy; a stateful app upgrade requires the backup
confirmation; no `pending-upgrade` left behind.

## Phase 3: console UI

- Applications: version column, available-version selector, Upgrade button.
- Confirmation dialog with target notes, backup command, `confirmBackup`.
- Job progress/log; current version after success; rollback instructions on
  failure.
- No default/automatic upgrade; no "latest".

**Acceptance:** the upgrade can be driven entirely from the console; the CLI
and UI show the same versions/state; the audit log records the actor and
versions.

## Phase 4: upgrade test gate and promotion

- Add `ci/upgrade.sh` (or a Playwright/CI job):
  - fresh install of the current stable;
  - N-1 stable → N stable upgrade with data present;
  - `ci/assert.sh` + gating Playwright (`apps.spec.ts auth.spec.ts tls.spec.ts`);
  - functional user journeys for the apps in `ci_apps`;
  - backup/restore check;
  - single and HA.
- Add a **previous stable snapshot** fixture (`ci/versions/previous.yaml` or a
  lab snapshot) so the upgrade test starts from a real N-1 install with data.
- Add a broken-upgrade test (bad image/tag) that proves `--atomic` rollback.
- Candidate channel: the manual gate runs the candidate versions; stable is
  promoted only after the matrix passes.
- Add a version-discovery script/Renovate config that opens PRs with exact
  pins + chart bumps; no auto-merge.

**Acceptance:** the gate fails if N-1 → N is broken, if a floating tag returns,
or if a catalog/chart version mismatches; a candidate can be promoted or
demoted by a release of the console image + catalog, not by editing a running
container.

## Phase 5: documentation and release process

- `docs/versioning.md` (policy), `docs/quickstart/07-upgrade.md` (operator
  runbook), rollback/data-restore section in `docs/quickstart/06-backup.md`.
- Document supported N-1, major-upgrade caveats, candidate/stable channels,
  and the exact backup command for single vs HA.
- Release checklist: bump chart version and `appVersion`; pin images; update
  catalog `versions`; run Tier 1/2 upgrade gate; package the chart into the
  local mirror; move the stable pointer; keep the previous version.
- Add a `CHANGELOG.md` per chart or a release note field in the catalog.

## Test matrix

| Test | Fresh install | N-1 → N | Failure/rollback | Backup/restore |
|---|---|---|---|---|
| Tier 1 (render/lint/pins) | yes | yes | yes | — |
| Single node e2e | yes | yes | yes | yes |
| HA e2e | yes | yes | yes | Longhorn snapshot |
| Functional user journeys | yes | yes | — | — |
| Auth/OIDC/LDAP | yes | yes | — | — |
| Stateful data migration | — | per app | restore | yes |
| Platform (k3s/Helm/Traefik) | separate | separate | separate | — |

## Risks and open decisions

- **Stateful migrations** are the hard part. Helm rollback does not restore
  data; the backup gate and restore drill are mandatory for Nextcloud,
  Paperless, Immich, Gitea/Postgres and the Servarr config volumes.
- **Backup automation**: local-path has no snapshots. Start with an
  operator-confirmed VM snapshot; later add a Longhorn snapshot Job for HA and
  a host-side `install.sh --backup` marker.
- **Chart distribution**: the submodule commit plus the local mirror is the
  delivery path; OCI is an optional historical fallback. Keep the N-1 tarballs;
  do not rely on the 30-day prune.
- **Baked catalog vs runtime version index**: the catalog is in the console
  image, so shipping a version currently means rebuilding the console locally. Start
  with the baked catalog for reproducibility; if version updates need to avoid
  a console rebuild, move the version index to a generated ConfigMap or an OCI
  artifact read at startup. Recommendation: keep it baked in Phase 1 and add
  the ConfigMap index in Phase 4 if release cadence demands it.
- **Servarr** is a bundle of seven images. Version it as one chart (the tested
  bundle) and show the component versions in the release notes; per-component
  upgrades are not independent.
- **Floating upstream tags with no semver** (FlareSolverr) use a digest and a
  recorded version; do not treat the `latest` tag's reported version as a
  stable release without a test.
- **Major app jumps** (Jellyfin 12, Radarr 6) need the candidate channel and a
  data-restore plan before promotion; they are not part of Phase 0.
- **Platform upgrades** (Helm 4, Traefik 42, k3s 1.37) stay on a separate
  cadence; do not mix them into an app upgrade PR.
- **Downgrades** are only safe with a matching data restore; document that.

## Done when

- A fresh install uses only exact pins, and the static checks reject a floating tag.
- `app versions` / `GET /api/apps/{name}/versions` show installed, stable and
  candidate versions.
- `app upgrade --to <version>` performs an atomic upgrade with a backup
  confirmation, health checks and a clear rollback story.
- The console offers the same upgrade with a version selector, not "latest".
- The N-1 → N gate passes on single and HA, including a stateful app, and a
  failed upgrade rolls back.
- The stable pointer moves only after the gate; the previous chart stays
  available.
