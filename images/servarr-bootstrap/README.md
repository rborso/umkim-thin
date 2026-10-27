# servarr-bootstrap

In-cluster automation for the Servarr stack. It configures qBittorrent,
Prowlarr, Sonarr, Radarr, Jellyfin and Seerr through their APIs. Indexers are a
user choice and are added later in Prowlarr; the bootstrap creates the download
client and FlareSolverr proxy but no tracker.

## Build

The fixed build context is this directory:

```bash
podman build -t localhost/umkim-thin/servarr-bootstrap:local .
```

The console normally does this for you: **Applications → media stack → Run
setup** probes the pinned node, builds or reuses the image, imports it into that
node's containerd and runs the chart's bootstrap Job. The image is an internal
build artifact and is not prebuilt or published. For a registry-backed
environment, set the chart values instead:

```yaml
bootstrap:
  image: localhost/umkim-thin/servarr-bootstrap:0.2.23
  imagePullPolicy: IfNotPresent
```

The image is based on the digest-pinned Microsoft Playwright image because the
Jellyfin/Seerr setup uses a browser. Expect a large first pull.

## Chart integration

The image is used by `charts/apps/servarr` when `bootstrap.enabled=true`. The
chart runs an `init-media` hook (weight 2) and the main bootstrap Job (weight
10); qBittorrent password, API-key and LDAP init containers are part of the app
Deployments. The console sets `bootstrap.imagePullPolicy: Never` and a
`nodeSelector` for the pinned node after importing the image.

The Job is idempotent for its normal reconciliation path. A completed Jellyfin
first-run wizard cannot be replayed; the console records the credential hash and
skips an identical rerun.

## Local development

```bash
npm ci
npm run build
node dist/main.js   # needs the cluster services and env vars
```

See [`SOURCE.md`](SOURCE.md) for the vendored source and license provenance.
