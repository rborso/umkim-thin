# Testing taxonomy

The test names describe **purpose** first and **technology** second. "E2E" is the
tier that runs against a real deployment; it is not a synonym for every test in
that tier.

| Layer | Name | What it proves | Entry point |
|---|---|---|---|
| Tier 1 | **Static checks** | The tree builds, is formatted/vetted, unit-tested, and its charts/manifests render. Includes Helm lint/template, the catalog contract render, gitleaks (when installed) and Ansible syntax. Run manually before a PR or merge; there is no hosted CI workflow. | `./ci/static.sh` |
| Tier 1 | **Unit tests** | Go packages behave correctly, including `TestCatalogChartsRender`, which renders each catalog entry with the console's values against its pinned chart. They are part of `ci/static.sh`, not a separate lab stage. | `go test ./...` |
| Tier 2 | **Platform conformance** | The provisioned cluster is healthy: nodes, storage classes/PVCs, identity secrets, network policy, TLS, the storage agent and WireGuard. | `ci/assert.sh` / `tests/e2e/run.sh platform` |
| Tier 2 | **Acceptance gate** | The deployment meets its required contract: route/auth challenge, TLS, console, and every installed app accepts a directory login. Gating. | `tests/e2e/run.sh acceptance` |
| Tier 2 | **Functional user journeys** | A user or client can complete a real workflow. Informational, reported in full. | `tests/e2e/run.sh journeys` |
| Tier 2 | **Backup and restore** | The console creates an archive, verifies it, lists it, restores it (state unchanged) and the console stays healthy. | `ci/backup-check.sh single|ha` |
| Tier 2 | **Upgrade** | Re-runs the documented platform upgrade, then asserts the console rolled, the state survived, the new config landed and every app is ready. The app-chart N-1 path runs when the mirror carries the older tarball. | `ci/upgrade-check.sh single|ha` |
| Tier 2 | **Existing-installation run** | The same platform, acceptance and optional journey checks run against an installation that is already deployed; no reset, provisioning or app install. | `ci/test-existing.sh single|ha` |
| Tier 2 | **Migration / upgrade tests** | Existing single-node installer state moved to the console PVC intact. | `tests/e2e/run.sh migration` |
| Tier 2 | **Security checks** | No secrets in the tree, logs or ConfigMaps; secret/security invariants. Split between static checks and platform conformance. | `ci/static.sh`, `ci/assert.sh` |

## Why not "functionality testing"?

Use **functional testing** (not "functionality testing") when talking about
*what* is tested, and **E2E** when talking about *where* it runs. A test can be
both. In this repo the deep workflows are called **functional user journeys**
because they simulate a user/client across several components.

Do not label every Tier 2 test "E2E" — that hides the difference between a
gating contract check, a platform health check, and an informational workflow.

## Acceptance gate

`tests/playwright`:

| Spec | Category | Covers |
|---|---|---|
| `apps.spec.ts` | acceptance | route reachability, auth challenge, public-path exemptions, authenticated reachability |
| `auth.spec.ts` | acceptance | regular tenant login: Gitea/Jellyfin/Nextcloud LDAP, Immich/Paperless/SparkyFitness/Actual Budget/HomeBox OIDC, Gitea SSH |
| `tls.spec.ts` | acceptance | the edge serves a real certificate for every route |
| `console.spec.ts` | acceptance | browser login through Authelia + console, Applications page, `/api/status`, CSP/HSTS, anonymous API refusal |
| `home.spec.ts` | acceptance | authenticated read-only home page and `/api/home`; no admin fields |

The gate is run by `ci/e2e.sh` with `npx playwright test apps.spec.ts auth.spec.ts
tls.spec.ts console.spec.ts home.spec.ts`.

## Functional user journeys

`tests/playwright/journeys/` (informational, but reported with pass/fail).
All checks are run manually. The suite includes one platform journey and the
per-app journeys; optional apps add their own when installed. Cross-app checks
such as Mealie → SparkyFitness are opt-in with `--integrations`. Journeys run
serially because OCR/indexing is sensitive to concurrent load.

### Platform

| Journey | What the user/client does |
|---|---|
| **Invitation acceptance** (`platform.spec.ts`) | An operator creates an invitation in the console; the invitee opens the public invitation link, chooses a password, and signs in through Authelia. |

### Gitea

| Journey | What the user/client does |
|---|---|
| **Repository workflow** | Creates a repository, branches, commits, opens a pull request, merges it, downloads the archive, and deletes the repository. |
| **Release workflow** | Creates a release and uploads an asset through the multipart API, then reads it back. |

### Nextcloud

| Journey | What the user/client does |
|---|---|
| **Client status + capabilities** | Reads `status.php` and the desktop/mobile client capabilities. |
| **Chunked upload** | Uploads a file larger than the chunk size, assembles it, and verifies the bytes. |
| **Edit conflict** | Proves a stale `If-Match` is refused and a retry with the current ETag succeeds. |
| **Trashbin / restore** | Deletes a file, finds it in the trashbin, restores it. |
| **Public link** | Creates a password-protected share, unlocks it as an anonymous visitor, downloads the content. |
| **Calendar** | Creates a CalDAV calendar and event, reads it back. |
| **Document editor** | Checks the Collabora WOPI discovery the browser editor depends on. |

### Immich

| Journey | What the user/client does |
|---|---|
| **Photo pipeline** | Uploads photos, waits for thumbnails, downloads the originals and compares bytes. |
| **Album + public link** | Creates an album, adds a photo, creates a public link, and opens it without a session. |
| **API key** | Creates an API key and uses it to authenticate and upload as an automation client. |

### Paperless

| Journey | What the user/client does |
|---|---|
| **Consume + OCR** | Uploads a real PDF, waits for the consumer, verifies the text layer, preview/thumbnail/original. |
| **Metadata** | Tags a document, assigns a correspondent, filters by the tag, and checks the associations. |

### Servarr

| Journey | What the user/client does |
|---|---|
| **API keys** | Queries Sonarr, Radarr and Prowlarr status with each app's own API key. |
| **Shared media root** | Creates an accessible root folder on the shared media volume. |
| **Download client** | Reaches qBittorrent's API through the portal session. |

### Actual Budget

| Journey | What the user/client does |
|---|---|
| **Budget sync** | Logs in through OIDC, creates a budget, account and transaction with the official API client, syncs it, then downloads the same budget into a second client and verifies the transaction. |

### HomeBox

| Journey | What the user/client does |
|---|---|
| **Inventory CRUD** | Creates a location, item, tag and photo; reads them back; finds the item by search and in the CSV export. |

### Cross-app (opt-in)

| Journey | What the user/client does |
|---|---|
| **Mealie → SparkyFitness** | Creates a Mealie recipe, runs the console's one-click Mealie integration, and finds the recipe through SparkyFitness's Mealie provider. Requires `--integrations`. |

## Tier 2 runner

`tests/e2e/run.sh` accepts one category:

```sh
TARGET=ha tests/e2e/run.sh acceptance
TARGET=ha tests/e2e/run.sh journeys
TARGET=ha tests/e2e/run.sh platform
TARGET=single tests/e2e/run.sh migration
TARGET=ha tests/e2e/run.sh gate      # acceptance + platform (+ migration on single)
TARGET=ha tests/e2e/run.sh all       # gate + journeys (journeys informational)
```

`ci/e2e.sh` runs the same categories: provision, install apps, platform
conformance, acceptance gate, then the informational user journeys.

## Testing an existing installation (manual)

Use this when the cluster already exists (single or HA) and you do **not** want a
runner to reset it, provision it, or install/upgrade apps:

```sh
# default env: deploy/provision-single.env / deploy/provision-ha.env
./ci/test-existing.sh single
./ci/test-existing.sh ha --env deploy/provision-ha.env --pin

# also run the informational journeys and their media fixture
./ci/test-existing.sh single --journeys
./ci/test-existing.sh ha --env deploy/provision-ha.env --pin --journeys
# cross-app checks are opt-in
./ci/test-existing.sh ha --env deploy/provision-ha.env --pin --integrations
```

What it does:

1. Reads the target env and discovers **installed catalog releases from Helm**;
   apps that are not installed are omitted instead of being treated as failures.
   It also discovers the existing Ingress hosts; it does not call `reset.sh`,
   `provision.sh`, or the app installer.
2. Runs `ci/assert.sh` platform conformance. For an existing installation the
   console restart is disabled by default (`--with-restart` re-enables it) and
   the WireGuard app-install fallback is disabled. The remaining checks are
   read-only or create/remove only `ci-*` test resources.
3. Creates one temporary `ci-portal-<run-id>` tenant, runs the acceptance gate
   (`apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts home.spec.ts`), and
   runs the optional journeys serially. `--integrations` also enables the
   cross-app journey (Mealie → SparkyFitness). Temporary directory test accounts,
   Gitea test users, SSH keys and repositories are removed by
   `ci/cleanup-ci.sh`; the per-run media fixture and its Radarr root folder are
   removed as well. `--no-cleanup` leaves the accounts for inspection.
4. Writes reports under `tests/reports/<target>/<run-id>/` (gitignored):
   `summary.md`, `summary.json`, `gate.json`, `gate.xml`, `gate.log`,
   `platform.log`, `html/index.html`, `artifacts/`, and `journeys.json` when
   journeys were requested.

HA DNS often points at a public address that does not hairpin from the lab
runner; `--pin` resolves the discovered app hosts (including Collabora and any
Ingress hosts) to the target VIP. No hosted CI workflow invokes any of this:
the lab tiers remain a deliberate manual run because the runner holds the
credentials.
