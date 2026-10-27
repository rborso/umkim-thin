# Tier 2: E2E tests

These tests run against a real deployment. They are split by purpose, not by
technology:

| Category | What it proves | Command |
|---|---|---|
| **Acceptance gate** | The deployed system meets its required contract: routes challenge anonymous users, every app accepts a directory login, TLS is served by the edge, and the console works in a browser. | `tests/e2e/run.sh acceptance` |
| **Functional user journeys** | A user or client can complete a real workflow: accept an invitation, edit a document, merge a PR, upload photos, OCR a PDF, run the media stack. These are informational and run serially (OCR/indexing is sensitive to concurrent load). | `tests/e2e/run.sh journeys` |
| **Platform conformance** | The provisioned platform is healthy: nodes, storage classes/PVCs, identity secrets, network policy, WireGuard and TLS. | `tests/e2e/run.sh platform` |
| **Migration / upgrade** | Single-node host state moved to the console PVC without changing installer-owned files. | `tests/e2e/run.sh migration` |

`gate` runs acceptance + platform (+ migration on `single`); that is the manual
gating path. `all` additionally runs the user journeys and reports their
failures without failing the run, matching `ci/e2e.sh`.

```sh
TARGET=ha tests/e2e/run.sh acceptance
TARGET=ha tests/e2e/run.sh journeys
TARGET=ha tests/e2e/run.sh platform
TARGET=single tests/e2e/run.sh migration
TARGET=ha tests/e2e/run.sh gate
TARGET=ha tests/e2e/run.sh all
```

The target environment is selected with `TARGET=single|ha`; credentials and
hostnames come from `deploy/provision-<target>.env` and `ci/lab.env`. For the
acceptance specs that log in as a regular tenant, set
`UMKIM_THIN_PORTAL_USER` / `UMKIM_THIN_PORTAL_PASSWORD` (the e2e script does
this automatically).

The Playwright specs live in `tests/playwright/`:

| Path | Category |
|---|---|
| `apps.spec.ts` | acceptance: route and auth-challenge contract |
| `auth.spec.ts` | acceptance: directory/LDAP/OIDC login matrix |
| `tls.spec.ts` | acceptance: edge certificate and route |
| `console.spec.ts` | acceptance: console browser flow |
| `journeys/*.spec.ts` | functional user journeys |

The `tests/e2e/migration/` check is not a Playwright suite; it reads the host
seed and the console PVC through `ci/lib.sh`.
