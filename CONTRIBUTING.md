# Contributing

Read [`AGENTS.md`](AGENTS.md) first: it explains the repo layout, the `local/`
convention (never commit environment details), and the two test tiers. The design
and scope are in [`docs/design.md`](docs/design.md); the auth model is in
[`docs/auth.md`](docs/auth.md); the test taxonomy is in
[`docs/testing.md`](docs/testing.md).

By participating you agree to the [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
There is no CLA: contributions are accepted under the repository's
[`LICENSE`](LICENSE) (Apache-2.0). Third-party licenses and provenance are
recorded in [`docs/licenses.md`](docs/licenses.md) and [`NOTICE`](NOTICE).

## Development

Contributors with an SSH key can use the SSH checkout:

```bash
git clone --recurse-submodules git@github.com:rborso/umkim-thin.git
```

The public installer keeps the HTTPS clone URL because it must work without a
GitHub account or SSH key. The `charts/apps` submodule URL stays HTTPS so public
checkouts and the installer work either way.

Requirements: Go 1.24+, `helm`, `kubectl`, `bash`, Node.js/Playwright for the
Tier 2 specs, and (optional) `ansible-core`, `gitleaks`, `shellcheck`.

```bash
go build ./...
go test ./...
./ci/static.sh          # Tier 1: the full no-cluster gate
./ci/novice-test.sh     # installer render/dry-run checks (also in static.sh)
```

The live e2e (`ci/e2e.sh single|ha`) uses the lab and is run manually. See
[`ci/README.md`](ci/README.md) and [`tests/e2e/README.md`](tests/e2e/README.md). Tier 2 categories are:

- **acceptance** — `apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts`;
- **functional user journeys** — `tests/playwright/journeys/` (informational);
- **platform conformance** — `ci/assert.sh`;
- **migration** — `tests/e2e/migration/migrate-single.sh`.

## Changing an app chart

App chart source lives in
[`umkim-charts`](https://github.com/rborso/umkim-charts); this repository pins it
as the `charts/apps/` submodule.

1. Change the chart and bump its `Chart.yaml` version in `umkim-charts`.
2. Run its chart lint/package checks, then update the submodule pointer here:
   `git -C charts/apps fetch && git -C charts/apps switch --detach <ref>`,
   followed by `git add charts/apps`.
3. Bump `chart.version` in `catalog/<name>.yaml` to the same version.
4. Keep the entry's `auth` and `route` honest; every public path must be
   anchored and token-authenticated by the app.
5. Add or update the Playwright acceptance check and, for a real workflow, the
   journey under `tests/playwright/journeys/`; update `verify.checks`.
6. Record the upstream project and license in the chart README,
   [`docs/licenses.md`](docs/licenses.md) and the chart repo's `LICENSES.md`.
7. Run `./ci/static.sh`; the catalog/chart version check and the
   `TestCatalogChartsRender` contract test must pass.

A chart PR and its consumer PR are one change: the chart repo PR contains the
chart, and the umkim-thin PR updates `charts/apps` plus the catalog version. Do
not merge or release one side alone.

A new app proposal should use the **New app** issue template. The official
catalog is curated; a private or community catalog can reuse the same entry
format without being presented as official. The proposed source model is in
[`docs/catalog-sources.md`](docs/catalog-sources.md).

If you clone an existing checkout, initialize the submodule with
`git submodule update --init --recursive`.

## Changing the novice path

`install.sh` and `docs/quickstart/` are a product surface, not an afterthought.
Keep them in sync with the chart values and the console UI:

- `install.sh --render-only --zone example.com --email you@example.com --out /tmp/x`
  must produce values that `helm template` accepts;
- `./ci/novice-test.sh` must pass;
- a new prerequisite or port must be in the preflight and in
  `docs/quickstart/01-install.md`;
- a new destructive action needs a documented undo.

## Security

- Never commit a secret, a real hostname, or a `local/` file.
- `ci/static.sh` runs `gitleaks` when it is installed; run it before a PR.
- Error messages and audit lines must never contain a password, token, invite
  token, or WireGuard private key.
- New mutating API routes go through `mutating()` and write an audit line.

## Commits

Small, focused commits with a scope prefix (`app:`, `deploy:`, `ci:`,
`install:`, `docs:` …). Keep the tree green.
