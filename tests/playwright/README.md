# Playwright suites

The Playwright project has two categories; see `docs/testing.md` for the full
test taxonomy.

| Path | Category | Purpose |
|---|---|---|
| `apps.spec.ts` | acceptance | route reachability, auth challenge, public-path exemptions, authenticated reachability |
| `auth.spec.ts` | acceptance | regular-tenant logins: Gitea/Jellyfin/Nextcloud LDAP, Immich/Paperless OIDC, Gitea SSH |
| `tls.spec.ts` | acceptance | the edge serves a real certificate for every route |
| `console.spec.ts` | acceptance | browser login through Authelia + console, Applications page, API/headers |
| `journeys/` | functional user journeys | complete client workflows (invitation, Gitea, Nextcloud, Immich, Paperless, Servarr); informational |

The acceptance specs gate `ci/e2e.sh`. Functional user journeys are reported but
do not fail the pipeline; they run serially because OCR/indexing is sensitive to
concurrent load.

## Running

Use the Tier 2 runner so credentials, hosts and app metadata are set up the same
way as CI:

```sh
TARGET=ha tests/e2e/run.sh acceptance
TARGET=ha tests/e2e/run.sh journeys
TARGET=ha tests/e2e/run.sh console   # one acceptance spec
```

For direct Playwright work:

```bash
cd tests/playwright
npm ci
npx playwright install --with-deps chromium
npx playwright test apps.spec.ts
```

The specs use the public URLs, because that is what a user hits — including the
NAT/loopback behaviour that makes in-cluster route resolution worth checking.
