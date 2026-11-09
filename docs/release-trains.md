# Release trains: platform, charts and catalog

The platform, the app charts and the app catalog now have separate cadences.
A platform security fix does not need a chart bump, and a new chart version does
not need a new console image.

| Train | Artifact | Version source | How it is published |
|---|---|---|---|
| **Platform** | console image, installer, `VERSION` | `VERSION` / exact git tag | install/upgrade (`install.sh`, `deploy/provision.sh`) |
| **Charts** | a chart version in the `umkim-charts` source tree | `charts/apps/<app>/Chart.yaml` → `version` | commit + submodule update; the installer packages the local mirror |
| **Catalog** | `.tar.gz` of `catalog/*.yaml` + `VERSION` | `catalog/VERSION` | `umkim-thin catalog package`, then upload in the console |

## Platform

Unchanged: `VERSION` is the platform release. A platform release contains the
console/API/installer behavior and the **bundled catalog fallback**. It does
not need to change when an app chart changes.

The console's update check remains platform-only. The catalog and chart version
shown in Applications come from the live catalog, not from the console image.

## Catalog release

1. Bump `catalog/VERSION` when the entry set changes.
2. Validate and package the catalog from the platform checkout:

   ```bash
   go run ./cmd/umkim-thin catalog package
   # → catalog-0.1.0.tgz (the output is deterministic apart from gzip framing)
   ```

3. On **Catalog → Upload catalog…**, choose the tarball. The console validates
   every entry, replaces `/data/catalog` on its PVC, and refreshes the live
   snapshot. **Refresh** re-reads the directory after an external copy/change.
4. To roll back, upload the previous tarball. The bundled catalog remains the
   fallback if the external directory is empty or invalid.

The upload is admin-only, same-origin protected, size-limited, and extracts only
top-level `*.yaml` files and `VERSION` (no links, no nested paths). A malformed
bundle is refused before the running snapshot is replaced. A failed refresh
keeps the last good catalog and shows the error in the Catalog header.

### Compatibility gate

Catalog entries can carry:

```yaml
chart:
  versions:
    - version: 0.1.2
      minimumUmkimThinVersion: "0.3.4"
```

`chart.minimumUmkimThinVersion` is the fallback for versions that do not set
their own. A release build older than the minimum is refused **before** Helm
runs. A `dev+<sha>` build is allowed, because it cannot prove a release; use a
release build for production. The field is validated as a version by
`catalog.Load` and by the CI catalog contract.

## Chart release

Charts live in `umkim-charts`; the platform vendors them as the `charts/apps`
submodule and packages them into the local mirror. There is no chart registry
and nothing to push: a chart release is a version bump and a commit in
`umkim-charts`, then a catalog entry that names that version.

In `umkim-charts`:

```bash
helm lint servarr
helm package servarr --destination dist   # optional local check
```

The catalog entry records the source and pins the version:

```yaml
chart:
  ref: https://github.com/rborso/umkim-charts/tree/main/servarr
  version: 0.3.3
```

Then bump the `charts/apps` submodule in the platform checkout (so the
bundled/offline mirror carries the chart) and publish a catalog release that
adds the version (and its `minimumUmkimThinVersion`, if it needs one). The
console resolves the chart in this order:

1. `chart-mirror/<name>-<version>.tgz` from the installer/cache — this is the
   only delivery path; the catalog's `ref` is provenance, not a fetch URL;
2. if the exact package is absent, the install fails rather than reaching for a
   registry. Install/update the platform or upload a catalog whose version is
   present in the mirror.

The installer packages the current chart into the mirror; keep the previous
stable tarball there too so an offline upgrade/rollback can still resolve it.

## Upgrade and rollback

- **Platform:** run the installer for the new release (or restore the platform
  backup). The bundled catalog is a fallback; the external catalog is untouched
  by a console image update.
- **Catalog:** upload the new/previous tarball; no pod restart.
- **Chart:** `Applications → Upgrade` (or `app upgrade <name> --to <version>`).
  The pre-upgrade backup confirmation is unchanged. Rolling a chart back is
  another explicit `--to` upgrade, not an implicit downgrade.

A backup records the chart version it saw. If a later catalog drops that version
and the mirror no longer has it, restore the platform/console image that
contains the fallback before restoring the app. Keep chart versions immutable so
this window is rare.
