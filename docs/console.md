# The console

The console is the platform's UI and API. It installs catalog apps, publishes
their routes, shows live status, and can build and run the media stack's
one-shot configuration Job. It is one Go binary (Helm and kubectl included)
served behind Authelia and its own login.

## Layout

The console is split into tabs so unrelated operations are not stacked on one
page:

- **Overview** — the getting-started checklist and the live **Applications**
  view. The checklist marks the first app, invite and WireGuard client; it is a
  nudge, not a gate.
- **Catalog** — installable apps with hostname, exposure, certificate and
  storage-class pickers, followed by the **Installed apps** management table
  (Helm releases from the catalog only; uninstall/purge and status).
- **Access** — **Invitations** and **WireGuard clients**, the two person/device
  onboarding surfaces.
- **Storage** — local-host disks/RAID/NFS exports and external NFS
  StorageClasses; see [`storage.md`](storage.md).
- **Certificates** — manual TLS certificates and the default wildcard.

The header keeps only the portal and console links; the tabs own everything
else. Messages and job progress appear in a fixed status panel, so a long
install does not disappear when the page is scrolled.

**Applications** polls `/api/status` every two minutes (the server caches for
45 s). Its Version column shows the installed chart and, when the catalog has a
different stable version, the new chart/app version plus an **Upgrade to …**
button. **Versions…** shows the full catalogued list and the upgrade path runs
`helm upgrade --atomic` after a backup confirmation.

## Status

`GET /api/status` is one aggregate poll. Per app:

| field | meaning |
|---|---|
| `helmStatus` | Helm's release status (`deployed`, `pending-upgrade`, ...) |
| `ready` | Helm is deployed **and** every Deployment/StatefulSet/Job is ready/complete |
| `routes` | hosts of the Ingresses in the app's namespace |
| `components` | per-workload `kind/name/ready/status` |
| `bootstrap` | media stack only: `not-configured`, `pending`, `running`, `succeeded`, `failed` |
A release in `pending-*` is shown as **configuring** (amber), not as down: that
is what a normal upgrade or bootstrap looks like. Readiness is cluster
readiness, not an HTTP probe.

## Storage classes

`GET /api/config` returns the classes, the default marker, and access-mode
hints. Kubernetes does **not** declare access modes on a StorageClass (they are
a PVC property), so the API uses the first source that knows:

1. access modes observed on bound PVs of that class (`observed`);
2. `UMKIM_THIN_STORAGE_ACCESS_MODES` (`class=RWX+RWO,...`, `configured`);
3. the platform's conventions (`umkim-thin-nfs`/`nfs`/`nfs-client` → RWX,
   `*-rwx` → RWX, `local-path`/Longhorn → RWO, `known`);
4. otherwise `unknown`, and the install form warns rather than guessing.

The media picker warns when the chosen class does not advertise RWX. It never
hides a class.

## External NFS

The Storage tab has two separate paths:

- **Local host** (single node only): blank disks/RAID or an existing directory,
  exported by the host NFS server through the storage agent.
- **External NFS** (single and HA): an existing `server` and `export` path. The
  console probes TCP/2049 and validates the export with `showmount -e` when it
  is available, then installs `nfs-subdir-external-provisioner` as Helm release
  `nfs-<name>` and creates a ReadWriteMany, Retain StorageClass. It does not
  format, mount or manage the remote export. Apply can take up to five minutes;
  the form and plan output show that it is running.

External NFS state is kept on the console PVC and is merged into
`GET /api/storage` with the host exports. The name must be unique across both
paths because each maps to one Helm release.

## Media stack: setup

The stack has no tracker dependency. `POST /api/apps/servarr/setup` runs the
bootstrap as a background job:

1. ensure the Secret (app passwords generated **once**, chart API keys copied);
2. restart qBittorrent so its WebUI password matches the Secret;
3. probe the pinned node for the image (`imagePullPolicy: Never`);
4. if missing, build the fixed context in the `umkim-thin-build` namespace and
   import the archive into that node's containerd;
5. `helm upgrade --reuse-values` with `bootstrap.enabled=true`,
   `imagePullPolicy=Never` and a `nodeSelector` for the pinned node;
6. Helm waits for the post-upgrade Job; its log is reported on failure.

### Default language and quality

The **Servarr install form** exposes the media stack's defaults:

- **Default language** (Hungarian, English, and the other Radarr languages):
  sets the Seerr original language and the Radarr quality-profile language.
  Jellyfin receives it during its first-run wizard; an existing Jellyfin UI
  language is not rewritten.
- **Target resolution** (`480p`, `720p`, `1080p`, `2160p`): creates or updates
  the matching Sonarr/Radarr quality profile, blocks qualities above it, and
  selects it as the default profile on the Seerr Sonarr/Radarr servers.

These fields are install-time defaults, not an Overview card. The install form
also has an optional **Additional languages** selector; it creates
language-specific variants of the chosen target quality, for example
`HD-720p · English`, while leaving the default profile unchanged. The
installed stack's Profiles manager can add other resolutions later.

After install, each Servarr row has a **Profiles** action. The app-scoped
manager lists the default profile and the additional profiles, and can add any
resolution/language combination (`480p`, `720p`, `1080p`, `2160p`). It never
changes the default. New profiles are cloned from the closest base quality
rules, so they keep the resolution/cutoff and custom-format constraints.

Additional profiles can be deleted. The default and built-in profiles are
immutable in the console, and deletion is refused while a Radarr/Sonarr library
item or a pending/approved Seerr request still uses the profile. A successful
delete is followed by a Seerr profile-cache flush.

The bootstrap grants Seerr's **Request Advanced** permission to non-admin users
as well, so the request dialog exposes the quality-profile picker and starts it
at the configured default. Choosing a `… · English` profile is how a user
chooses English for a movie; Seerr has no separate per-request language field
for Radarr. Jellyfin playback language remains a per-user Jellyfin setting.


Indexers are an optional user choice. They can be added directly in Prowlarr:

Indexers → Add Indexer → search by name → credentials → optionally assign the
FlareSolverr proxy → Save, then Settings → Apps → Sync App Indexers. The
bootstrap creates the FlareSolverr proxy and the qBittorrent download client,
but no indexer.

Each installed Servarr row has an **Indexers** action. It opens the manager
for that specific app instance (release + namespace), so two Servarr installs
do not share an indexer surface. The manager is a thin Prowlarr client: it
lists Prowlarr's own definitions, renders the fields for one selected
definition and sends the result to that app's Prowlarr. Tracker credentials are
never stored in the console, Helm values or logs; Prowlarr remains their only
owner. Add one indexer at a time (repeat for more). Definitions that require
interactive login, cookies or unusual challenge handling should still be
configured in Prowlarr's UI. The scoped endpoints are
`GET /api/apps/{name}/indexers?namespace=…`,
`GET /api/apps/{name}/indexer-definitions?namespace=…`,
`GET /api/apps/{name}/indexer-definition?namespace=…&definition=…` and
`POST /api/apps/{name}/indexers?namespace=…`.

The profiles manager uses
`GET /api/apps/{name}/profiles?namespace=…`,
`POST /api/apps/{name}/profiles?namespace=…` and
`DELETE /api/apps/{name}/profiles?namespace=…&profile=…`.

**Images.** Upstream app images are pulled as before. The console and Servarr
bootstrap images are **internal build artifacts**: the installer/console builds
them locally and imports them into containerd. They are not prebuilt or
published. The bootstrap build context is vendored in
`images/servarr-bootstrap`, based on a digest-pinned Playwright image, and the
console tags a build as `VERSION-<commit>`, so a source or commit change
produces a new tag and a stale build cannot be reused.

**Node.** `UMKIM_THIN_BOOTSTRAP_NODE` (normally the first server's hostname) is
validated against the cluster; otherwise the only node, the console pod's node,
or the first node is used. The image is imported on that node and the Job is
pinned there; nothing is pulled from a registry.

**Failure policy.** A build or bootstrap failure never fails the install: the
app stays installed and the Applications section shows `failed` with a retry. A
successful run records the image tag, so clicking setup again with the same
image is a no-op (the chart's Jellyfin wizard step cannot be re-run).

## Security

- The console's own login (LDAP admin or breakglass `localadmin`) and Authelia
  both guard it; the session cookie is `HttpOnly`, `Secure`, `SameSite=Lax`.
- Mutating requests are same-origin; the JSON APIs are not CORS-enabled.
- Values enter the cluster through `kubectl apply -f -` on stdin, never argv.
- Privileged actions (install, uninstall, settings, setup) write one audit line
  (user, action, app; no values).
- The builder is the only privileged workload:
  - dedicated namespace `umkim-thin-build`, the only one labelled
    `pod-security.kubernetes.io/enforce=privileged`;
  - no ServiceAccount token, no hostPID/hostNetwork;
  - fixed build context from the console image (the API accepts no Dockerfile
    or context), base image pinned by digest;
  - kaniko (no user namespaces, no extra capabilities); the only mounts are the
    context, an output emptyDir, and the read-only k3s binary/data plus the
    containerd socket (needed for the import);
  - egress limited to DNS and 80/443; 30-minute deadline, no retries.
- Known residual: the console's ServiceAccount can create pods cluster-wide, so
  it is already node-root-equivalent. The builder does not widen that; a
  namespace-scoped RBAC would have to replace the broad ClusterRole that
  arbitrary charts require.

## API

| method | path | purpose |
|---|---|---|
| GET | `/api/catalog` | catalog entries |
| GET | `/api/config` | zone, portal, default + classes |
| GET | `/api/apps` | installed catalog releases (chart, app version, helm status, routes) |
| POST | `/api/apps` | install with `version`, per-route `exposure`/`certificate` (async job) |
| GET | `/api/apps/{name}` | one app's health, chart/app version and routes |
| GET | `/api/apps/{name}/versions` | installed + available chart/app versions |
| POST | `/api/apps/{name}/upgrade` | `{version, confirmBackup}`; atomic upgrade job |
| DELETE | `/api/apps/{name}?purge=` | uninstall (purge deletes data) |
| GET | `/api/status` | Applications aggregate (45 s cache) |
| GET | `/api/storage` | host exports/arrays plus external NFS exports |
| GET | `/api/storage/disks` | local blank/used disk inventory |
| POST | `/api/storage/plan` | local or external plan (`kind` selects the path) |
| POST | `/api/storage/apply` | local format/RAID/export, or external NFS StorageClass |
| DELETE | `/api/storage/{name}` | remove the export/StorageClass (data kept) |
| POST | `/api/apps/servarr/setup` | build (if needed) and run the bootstrap |
| GET | `/api/jobs`, `/api/jobs/{id}` | background job status/log |

## Placement and state

The console is a single cluster-mode Deployment in both single and HA:

- state, invites, WireGuard peers, the chart mirror and session state live on
  the `umkim-thin-state` PVC at `/data`;
- the config is the `umkim-thin-config` ConfigMap and the secrets are the
  `umkim-thin-secrets` Secret;
- the pod uses its ServiceAccount kubeconfig, not the node kubeconfig; it does
  not use `hostNetwork` and does not mount a hostPath state directory;
- the PVC class is `local-path` on single and `longhorn-rwo` on HA;
- the single-node host storage agent is reached through the optional
  `/run/umkim-thin` socket hostPath; without it the console hides the local
  disk form but still offers **External NFS**;
- external NFS exports are recorded on the console PVC as
  `/data/storage-external.yml` and managed through Helm, so they work on both
  single and HA.

## Configuration

The installer renders `/etc/umkim-thin/config.env`, then seeds it into the
`umkim-thin-config` ConfigMap. The same config is used by both targets; the
console-specific keys:

```
UMKIM_THIN_CONSOLE_IMAGE=localhost/umkim-thin:dev
UMKIM_THIN_BOOTSTRAP_IMAGE=localhost/umkim-thin/servarr-bootstrap
UMKIM_THIN_BOOTSTRAP_CONTEXT=/opt/umkim-thin/bootstrap
UMKIM_THIN_BUILDER_IMAGE=gcr.io/kaniko-project/executor:v1.23.2-debug@sha256:...
UMKIM_THIN_BUILD_NAMESPACE=umkim-thin-build
UMKIM_THIN_BOOTSTRAP_NODE=<first server's hostname>
UMKIM_THIN_STORAGE_ACCESS_MODES=class=RWX+RWO,...   # optional override
```

## Credentials

`./ci/credentials.sh single|ha` prints the console/directory secrets and the
media stack's usernames/passwords, **masked**; `--reveal` prints the values. It
reads them from the cluster and never writes a file.
