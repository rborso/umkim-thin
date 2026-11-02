# Upstream version audit — 2026-11-02

Read-only audit of the catalog, the `charts/apps` submodule and the live
single-node dev cluster. The upgrade described below was applied for the
v0.3.0 release; see **Applied in v0.3.0** first, and treat the rest of this
file as the pre-upgrade record.

Sources: GitHub Releases API for each upstream project, the catalog entries in
`catalog/`, `charts/apps/*/Chart.yaml` and `values.yaml`, `helm list`, and
`kubectl` on the single-node dev cluster.

## Applied in v0.3.0 (2026-11-02)

The stable upgrades from this audit are in `charts/apps` commit `b36f0b0` and
the catalog entries that point at them. The old chart versions stay listed in
each catalog `versions` block for rollback.

| App | Chart | App version |
|---|---|---|
| Actual Budget | 0.1.2 | 26.11.0 |
| Gitea | 0.1.21 | 28.3.0 |
| HomeBox | 0.1.1 | 0.27.1 (already current) |
| Immich | 0.1.11 | 3.3.5 (server/ML now exact `v3.3.5`, not `release`) |
| Mealie | 0.1.4 | 3.30.0 (chart passes `extraEnv`/`HTTP_ALLOW_LIST`) |
| Nextcloud | 0.1.20 | 35.0.2 (Collabora 26.04.5.2.1) |
| Paperless-ngx | 0.1.13 | 3.4.1 |
| Servarr | 0.3.2 | Jellyfin 12.4, Radarr 6.4.4, Sonarr 4.0.20.3014, Prowlarr 2.7.0.5723, qBittorrent 5.3.0 |
| SparkyFitness | 0.1.1 | 1.11.0 |
| WireGuard gateway | 0.1.8 | Alpine 3.24.3 |
| Identity | 0.1.10 | Valkey 9.1.3; LLDAP stays pinned at v0.6.3 |

Jellyfin 12 was verified against the v12.4 source: `Startup/Configuration`,
`Startup/User`, `Startup/RemoteAccess`, `Startup/Complete`,
`Users/AuthenticateByName`, `Auth/Keys` and `Library/VirtualFolders` are all
still present (the startup routes are marked obsolete but functional). The
LDAP plugin moved to v24 (target ABI 12.0). Jellyfin 12 defaults the legacy
authorization flag to false and then ignores `X-Emby-Token` /
`X-Emby-Authorization`; the bootstrap and the CI marker/auth helpers send the
standard `Authorization: MediaBrowser ... Token="..."` header as well, so one
image works against 10.x and 12.x. The upgrade is still a one-way database
migration for an existing 10.x configuration, so a data backup is mandatory
before the first start.

## Summary (pre-upgrade audit)

- Every catalogued app is installed, every Helm release is `deployed`, and every
  app Deployment is `1/1`.
- The catalog and the chart submodule are in sync. No chart version bump is
  pending.
- Most apps are one patch or one minor behind. The only upgrade that needs real
  design and test work is **Jellyfin 10.10.7 → 12.x**.
- Two more upgrades need targeted tests and possibly a chart change:
  **Radarr 5.28 → 6.x** and **Mealie 3.22 → 3.30**.
- One policy deviation: the Immich chart uses the floating
  `ghcr.io/immich-app/immich-server:release` tag instead of an exact pin.

## Repository state

| Item | State |
|---|---|
| Main checkout | `f6949ca`, 6 commits behind `origin/main` |
| Pending upstream commits | CI/install/docs only; no app or chart changes |
| Local working tree | Modified `ci/preflight.sh`, untracked `tests/playwright/apps.json` (pre-existing; untouched) |
| `charts/apps` submodule | Clean at `2a42234`, up to date with `origin/main` |
| Catalog/chart consistency | All catalog `chart.version` values match the submodule `Chart.yaml` |
| Catalog channels | All entries are `channel: stable`; no candidate entries |

Any app image bump still requires a new immutable chart version, a catalog
`versions` entry, and a submodule bump — per `docs/versioning-plan.md`.

## Live cluster state

Target: single-node k3s `v1.36.5+k3s1` dev cluster (not the HA lab).

| App | Chart | App version | Helm status | Workload |
|---|---|---|---|---|
| Actual Budget | 0.1.1 | 26.10.0 | deployed | 1/1 |
| Gitea | 0.1.20 | 28.1.0 | deployed | 1/1 |
| HomeBox | 0.1.1 | 0.27.1 | deployed | 1/1 |
| Immich | 0.1.10 | 3.3.5 (floating `:release`) | deployed | 1/1 |
| Mealie | 0.1.3 | 3.22.0 | deployed | 1/1 |
| Nextcloud | 0.1.19 | 35.0.1 | deployed | 1/1 |
| Paperless-ngx | 0.1.12 | 3.3.0 | deployed | 1/1 |
| Servarr | 0.2.27 | bundle 1.0.0 | deployed | 1/1 each |
| SparkyFitness | 0.1.0 | 1.9.0 | deployed | 1/1 |
| WireGuard gateway | 0.1.7 | 1.0 | deployed | 1/1 |
| Identity | 0.1.9 | 1.0 | deployed | 1/1 |
| Console | 0.1.5 | 1.0 | deployed | 1/1 |

Startup-only restarts were observed (not ongoing failures):

- Mealie: one `OOMKilled` at startup (1Gi memory limit).
- Immich server: three `Error` exits during initial startup.
- Gitea: one clean restart.
- WireGuard gateway: pod recreated during the audit window (hostNetwork);
  readiness warnings were transient.
- `nfs-provisioner`: seven restarts, platform component, not an app.

## Upstream delta

Latest stable upstream releases as of 2026-11-02.

| App | Pinned | Upstream latest | Gap | Risk |
|---|---|---|---|---|
| Actual Budget | 26.10.0 | 26.11.0 | minor | low |
| Gitea | 28.1.0 | 28.3.0 | patch | low |
| HomeBox | 0.27.1 | 0.27.1 | current | none |
| Immich | 3.3.5 (floating) | 3.3.5 | current | pinning issue |
| Mealie | 3.22.0 | 3.30.0 | 8 minors | **breaking change in 3.26** |
| Nextcloud | 35.0.1 | 35.0.2 | patch | low |
| Paperless-ngx | 3.3.0 | 3.4.1 | minor + patch | low/medium |
| SparkyFitness | 1.9.0 | 1.11.0 | 2 minors | low |
| Sonarr | 4.0.17 | 4.0.20.3014 | patch | low |
| Radarr | 5.28.0 | 6.4.4.10685 | **major** | **medium** |
| Prowlarr | 2.6.5.5623 | 2.7.0.5723 | minor | low |
| qBittorrent | 5.1.4 | 5.3.0 | 2 minors | medium (needs retest) |
| Jellyfin | 10.10.7 | 12.4 | **2 majors** | **high** |
| Seerr | 3.5.1 | 3.5.1 | current | none |
| FlareSolverr | 3.5.2 | 3.5.2 | current | none |
| Postgres | 18.6 | 18.6 | current | none |
| Valkey | 9.1.2 | 9.1.3 | patch | low |
| Collabora | 26.04.5.1.1 | 26.04.5.2.1 | patch | low |
| Alpine (bootstrap/VPN images) | 3.20.10 | 3.24.3 | 3.20 is EOL | maintenance |

## Breaking changes and upgrade notes

### 1. Jellyfin 10.10.7 → 12.4 (resolved in v0.3.0)

- Database changes are **not reversible without a full data restore**. A full
  backup of the Jellyfin data directory is mandatory before the upgrade.
- Direct upgrade from 10.10.7 to 12.0 is supported; intermediate versions are
  not required.
- Installed third-party plugins should be removed before migrating. Plugins
  need to be compatible with the new database/API. The chart now pins LDAP
  plugin **v24** (target ABI 12.0) for the 12.4 image.
- Jellyfin 12 removes legacy routes and APIs (`/emby/*`, `/mediabrowser/*`,
  EasyPassword, several item/network endpoints) and disables legacy
  authorization by default. In practice it still serves the startup routes,
  but ignores `X-Emby-Token`; the bootstrap now sends the standard
  `Authorization: MediaBrowser ... Token="..."` header as well.
- The startup/initial-config routes are marked obsolete but remain in v12.4
  (`Startup/Configuration`, `Startup/User`, `Startup/RemoteAccess`,
  `Startup/Complete`), so the bootstrap's API path still works.
- **Impact on this repo:** `images/servarr-bootstrap/src/ui/jellyfin.setup.ts`
  and `ci/markers.sh` now send both the standard and legacy auth headers.
- Jellyfin 12 refuses to start when `/config/data/data` has less than 2 GiB
  free. The chart now requests a **3 GiB** Jellyfin config PVC; an existing
  2 GiB claim must be expanded (Longhorn expands online) before the upgrade.
- A full library scan is required after migration.

### 2. Radarr 5.28 → 6.4 (medium)

- The 6.x line moves to .NET 8 and **removes Basic Auth**. API-key auth, which
  the bootstrap uses, still works.
- Movie-folder token support was removed.
- The default wanted language for quality profiles changed to “Original”.
- Database migration is involved. The chart and bootstrap need a smoke test,
  but no obvious bootstrap breakage was identified.

### 3. Mealie 3.22 → 3.30 (breaking security change)

- 3.26 added SSRF hardening: server-initiated requests to **private networks**
  (webhooks, recipe imports, image downloads, recipe actions) are blocked
  unless `HTTP_ALLOW_LIST` is set.
- The Mealie chart has **no `extraEnv` or `HTTP_ALLOW_LIST` passthrough**, so a
  chart change is required if any private-network target is used.
- `OPENAI_CUSTOM_PROMPT_DIR` was removed; custom prompts are imported into the
  new format automatically.
- Check whether Mealie's OIDC discovery reaches Authelia through a public or an
  internal URL. An internal URL may be affected by the same SSRF policy.

### 4. qBittorrent 5.1.4 → 5.3.0 (needs retest)

- WebUI/API behavior changed, including cross-site request detection via
  `Sec-Fetch-Site`, a new add-torrent flow, and session changes.
- No explicit “breaking” flag, but the bootstrap's qBittorrent WebUI password
  and login step, plus Prowlarr's download-client configuration, must be
  re-tested.

### 5. Paperless-ngx 3.3.0 → 3.4.1 (low/medium)

- No explicit breaking flag.
- Bumps `django-allauth` to 65.20 and `oauthlib` to 4.0; re-test the OIDC
  callback path.
- Drops ImageMagick; PDF thumbnails now use poppler. Re-test thumbnail
  generation and OCR.

### 6. SparkyFitness 1.9.0 → 1.11.0 (low)

- Database migrations run on startup.
- Adaptive calorie budgets no longer double-count activity/BMR credits;
  remaining calories may be lower after upgrade. This is a behavior change,
  not a deployment break.
- Re-test OIDC login, the admin panel and the Mealie food-provider integration.

### 7. Low-risk updates

- **Gitea 28.3.0:** patches only; the 28.0 breaking changes are already applied.
- **Nextcloud 35.0.2:** patch release.
- **Actual Budget 26.11.0:** monthly minor; no breaking notes found.
- **Prowlarr 2.7.0:** minor; database migration fixes.
- **HomeBox, Immich, Seerr, FlareSolverr:** already current.
- **Valkey 9.1.3, Collabora 26.04.5.2.1:** patch updates.
- **Alpine 3.20:** EOL; rebuild the bootstrap/VPN/WireGuard images on a
  supported Alpine (3.24.x) as maintenance.

## What needs more testing

### Priority 1 — Jellyfin 12

- [ ] Fresh-install Jellyfin 12 on a clean PVC, then run the servarr bootstrap.
- [ ] N-1 upgrade from 10.10.7 with a real data directory; verify the
      documented backup/restore path.
- [ ] Confirm whether `/Startup/*` still exists; if not, rework
      `jellyfin.setup.ts` to the new configuration endpoints.
- [ ] LDAP plugin v23/v24: install, bind with LLDAP, verify directory login and
      user creation.
- [ ] Verify Seerr's Jellyfin authentication still works.
- [ ] Verify the first-run wizard, library creation, scan and playback.
- [ ] Verify the `10.10.x` bootstrap compatibility comment in
      `charts/apps/servarr/values.yaml` is removed or updated with the new
      supported range.
- [ ] Test rollback: restore the pre-upgrade data directory and the old image.

### Priority 2 — Radarr 6

- [ ] N-1 upgrade from 5.28.0; confirm the database migration completes.
- [ ] Run the bootstrap: API key, root folder, quality profile, indexer and
      download-client wiring.
- [ ] Verify Basic Auth removal does not affect the route/forward-auth or the
      bootstrap's API calls.
- [ ] Verify the quality-profile language default change does not alter the
      stack's language selection.
- [ ] Run the request → indexer → download → import journey.
- [ ] Test rollback to 5.28.0 with a database snapshot.

### Priority 3 — Mealie 3.30

- [ ] Add an `HTTP_ALLOW_LIST` (or generic `extraEnv`) value to the Mealie
      chart, then test private-network recipe import/webhook/image fetch.
- [ ] Verify OIDC discovery and login against Authelia.
- [ ] Verify SparkyFitness can still read recipes from Mealie.
- [ ] Verify the custom-prompt import path after the
      `OPENAI_CUSTOM_PROMPT_DIR` removal.
- [ ] N-1 upgrade from 3.22.0 with a database backup.

### Priority 4 — qBittorrent 5.3

- [ ] Fresh install and N-1 upgrade from 5.1.4.
- [ ] Bootstrap qBittorrent WebUI password and login.
- [ ] Verify Prowlarr's download-client connection.
- [ ] Verify the full import chain still reaches the Jellyfin library.
- [ ] Check the `Sec-Fetch-Site` change against the bootstrap's API calls.

### Priority 5 — Paperless 3.4, SparkyFitness 1.11, sidecars

- [ ] Paperless: OIDC login, document upload, OCR, thumbnails, consume/export.
- [ ] SparkyFitness: OIDC login, DB migration, admin panel, Mealie provider.
- [ ] Valkey/Collabora patch bump: Nextcloud + Collabora document editing.
- [ ] Rebuild Alpine 3.20-based images on 3.24.x and re-run the bootstrap Jobs.

### Immich pinning

- [ ] Replace `immich-server:release` with an exact version tag (currently
      3.3.5) and set `Chart.yaml` `appVersion` to the same version.
- [ ] Add a static check that rejects floating tags in chart values.
- [ ] Verify an Immich pod restart no longer changes the running version.

### Regression gate for any of the above

Run the normal gates before promoting a candidate:

```sh
ci/static.sh
ci/e2e.sh single
ci/e2e.sh ha
ci/upgrade-check.sh single
```

For each app upgrade, also run the app's Playwright journey and the
`apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts` acceptance gate. The
Jellyfin/Radarr/qBittorrent/Mealie work should be tested on the **full** CI
profile as well, since the servarr bootstrap is built and exercised there.

## Recommended sequencing

1. **Jellyfin 12** — design + bootstrap rework + plugin bump, then lab test.
2. **Radarr 6** — chart bump + N-1 upgrade test.
3. **Mealie 3.30** — chart value for `HTTP_ALLOW_LIST` + upgrade test.
4. **qBittorrent 5.3 / Prowlarr 2.7 / Sonarr 4.0.20** — servarr bundle bump.
5. **Paperless 3.4 / SparkyFitness 1.11 / Gitea 28.3 / Nextcloud 35.0.2 /
   Actual 26.11** — routine bumps.
6. **Sidecars** — Valkey/Collabora patches, Alpine 3.24 rebuild.
7. **Immich** — pin the image before the next chart bump.

## Open questions

- Does the servarr bootstrap need a compatibility shim for both Jellyfin 10.10
  and 12, or is a clean break acceptable?
- Is `HTTP_ALLOW_LIST` needed for the default Mealie install, or only for
  optional private-network features?
- Should Immich be pinned now, even though that requires a new chart version
  and a catalog bump?
- Are the Alpine 3.20 bootstrap images rebuilt as part of the next chart bump,
  or kept until a separate maintenance change?

