# Catalog sources (proposed)

**Status:** proposal for discussion; the current code still loads one flat
catalog directory. See [`catalog.md`](catalog.md) for the entry format that is
already implemented.

## Goal

Have one **official catalog** that is curated and testable, while allowing an
operator to add a private or community app through the same mechanism. An
official app and a custom app should not be two different code paths, and a
custom app must not require forking the official catalog.

## Recommendation: a catalog is a source, an entry is still an entry

Keep the existing `CatalogEntry` (one YAML file per app). Add one small source
manifest around it:

```yaml
# catalog/official/catalog.yaml
apiVersion: umkim-thin/v1
kind: Catalog
metadata:
  name: official
  labels:
    trust: official
spec:
  # official | community | private
  trust: official
  # Sources are loaded in priority order. A collision is an error unless the
  # *later* source explicitly allows overriding an earlier entry.
  priority: 0
  allowOverride: false
  entries:
    - gitea.yaml
    - immich.yaml
    - nextcloud.yaml
    - paperless.yaml
    - servarr.yaml
    - wireguard.yaml
```

A private source uses exactly the same type:

```yaml
apiVersion: umkim-thin/v1
kind: Catalog
metadata:
  name: home
spec:
  trust: private
  priority: 10
  allowOverride: false
  entries:
    - dashboard.yaml
```

The entry file format does not change. The source manifest is only an index and
a trust/policy boundary.

## Source kinds

One source type, several transports:

| Kind | Example | Use |
|---|---|---|
| `builtin` | baked into the console image at `/opt/umkim-thin/catalog/official` | offline default |
| `dir` | `/data/catalogs/home` on the console PVC, or a read-only mount | private apps, air-gapped installs |
| `oci` | `oci://ghcr.io/rborso/catalogs/official:0.1.0` | published/cached official or community catalogs |

Do not add a Kubernetes CRD just for this. A directory or an OCI artifact is
portable, works with Git, and does not require the console to have cluster-scoped
CRD permissions. A CRD can be added later as another transport if it earns its
keep.

## Merge and override rules

1. Load **official** first, then sources by ascending `priority`.
2. Entries are keyed by `metadata.name`.
3. A duplicate name is an error by default, so a private source cannot silently
   shadow an official app.
4. An operator who really wants to replace an official app sets
   `allowOverride: true` on the later source, or passes an explicit
   `--allow-catalog-override` flag. The audit log and UI record the winning
   source.
5. Never merge two entries' fields. A winner is selected as a whole entry, so a
   custom source cannot partially mutate an official security policy.

This keeps the official catalog curated without locking the operator out.

## Official vs custom validation

The current `Validate` requires `verify.playwright` and `verify.checks`. That is
right for the official catalog and wrong for a private app that has no test
suite yet.

Make validation trust-aware:

| Rule | official | community | private |
|---|---|---|---|
| pinned `chart.version` | yes | yes | yes (or explicit local build) |
| route/auth/exposure valid | yes | yes | yes |
| `verify` present | yes | recommended | optional |
| chart in the official mirror/OCI | yes | optional | optional |
| upstream license/source recorded | yes | yes | recommended |

A custom entry without verification installs with a visible **unverified**
badge. It is not "official" and is not used by the official CI gate.

## Chart transport without lock-in

`chart.ref` should support:

- `oci://...` — the official and published path;
- `file:///data/catalogs/home/charts/dashboard` — a chart directory on a
  mounted catalog source;
- a packaged `.tgz` inside the same source root.

The resolver should confine `file://` paths to the source root (no `..`, no
absolute paths outside a configured root). Do not allow arbitrary URLs by
default; a remote HTTP chart is a supply-chain decision, not a convenience.

Add an optional `chart.digest` (sha256 of the package). Official entries must
set it; custom entries may omit it. Signatures are optional and can be layered
on later without changing the entry shape.

## License and provenance fields

Add an optional block to the entry, used by both official and custom catalogs:

```yaml
upstream:
  project: Gitea
  license: MIT
  source: https://github.com/go-gitea/gitea
```

Official entries must set it. The console can show it next to Install, and
[`licenses.md`](licenses.md) remains the repository-level attribution index.
A chart's `Apache-2.0` license covers the chart, not the image it deploys.

## Console/API/CLI shape

- `umkim-thin serve --catalog <dir>` stays the default and becomes repeatable:
  `--catalog /opt/umkim-thin/catalog/official --catalog /data/catalogs/home`.
- `UMKIM_THIN_CATALOGS` can carry the same list for the installer/container.
- `GET /api/catalog` includes `source` and `trust` for every entry.
- The console's Applications/Catalog page shows an **Official / Community /
  Private** badge and an **Unverified** badge where applicable.
- A private source is mounted on the console PVC or a read-only volume; the API
  does not accept a path from an unauthenticated caller.

## Backward compatibility

The current flat directory can be read as an implicit source:

```
builtin + trust=official, entries = every *.yaml in the directory
```

Migration can therefore be incremental:

1. Add the source manifest and loader; keep the flat directory working.
2. Add repeatable sources and the collision/override rules.
3. Publish the official catalog as an OCI artifact (optional, for caching and
   version pinning).
4. Add the source/trust badges and the optional `upstream` fields to the UI.
5. Only then move `catalog/*.yaml` into `catalog/official/`.

## What not to do

- Do not make the official catalog a Git submodule or a separate binary-only
  file format; that makes private apps second-class and hard to audit.
- Do not require a signature for every custom chart; it would lock out the
  simple case.
- Do not let a custom source override an official entry silently.
- Do not put all entries in one giant YAML file. One entry per file keeps diffs,
  review and verification per app.
