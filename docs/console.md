# The console

The console is the platform's UI and API. It installs catalog apps, publishes
their routes, shows live status, and can build and run the media stack's
one-shot configuration Job. It is one Go binary (Helm and kubectl included)
served behind Authelia and its own login.

## Home page

The platform also publishes a separate read-only `home.<zone>` page for ordinary
directory users. It is gated only by Authelia forward-auth and never asks for the
console login. It lists installed catalog apps with their display name,
description, launch URL and a coarse `Available` / `Starting` / `Unavailable`
status. The home listener refuses requests without Authelia's `Remote-User`
header, so the ClusterIP port is not a second unauthenticated surface.

The home page is deliberately not the console: it has no namespaces, Helm
status, version details, installs, upgrades, uninstall, invite or storage
controls. Its `GET /api/home` response is cached for 45 seconds because the
status calculation shells out to Helm and kubectl.

The console links to it so an operator can find the household address without
reading the installer output: the header carries a **home** link next to
*portal* and *console*, and Overview has a **Household home page** card with the
full URL to share. Both come from `homeUrl` in `GET /api/config`, which the
chart fills from `homeHost` (derived from `consoleHost` when unset). An install
that has no home host hides both.

## Layout

The console is split into tabs so unrelated operations are not stacked on one
page:

- **Overview** — the getting-started checklist and the live **Applications**
  view. The checklist marks the first app, invite and WireGuard client; it is a
  nudge, not a gate.
- **Catalog** — installable apps with hostname, exposure, certificate and
  storage-class pickers, followed by the **Installed apps** management table
  (Helm releases from the catalog only; uninstall/purge and status). Both
  tables show the deployed application's licence, the chart licence and links
  to the upstream website/source; the install panel repeats those links and
  shows the full licence notice and any non-commercial warning.
- **Access** — **Invitations** and **WireGuard clients**, the two person/device
  onboarding surfaces.
- **Storage** — local-host disks/RAID/NFS exports and external NFS
  StorageClasses; see [`storage.md`](storage.md).
- **Certificates** — manual TLS certificates and the default wildcard.

The header keeps only the platform links (portal, home, console), the running
version and the update chip; the tabs own everything else. Messages and job
progress appear in a fixed status panel, so a long install does not disappear
when the page is scrolled.

**Applications** polls `/api/status` every two minutes (the server caches for
45 s). Its Version column shows the installed chart and, when the catalog has a
different stable version, the new chart/app version plus an **Upgrade to …**
button. **Versions…** shows the full catalogued list and the upgrade path runs
`helm upgrade --atomic` after a backup confirmation. **Install options** shows
the release's Helm metadata and the user-supplied values it was installed with
(secrets redacted), including the storage classes and install choices. The CLI
equivalent is `umkim-thin app options <name>`.

## Update check

The catalog and the version are baked into the console image, so a running
console cannot know on its own that a newer release exists. `GET /api/version`
answers that: the running version, the newest published release, whether it is
newer, and the release page. When it is, the header shows an **update X.Y.Z**
chip linking to the release notes.

- The check reads the project's public release metadata
  (`https://api.github.com/repos/rborso/umkim-thin/releases/latest` by default)
  with `UMKIM_THIN_UPDATE_CHECK_URL` as the override for a mirror.
- It is read-only. It never downloads, installs or changes anything; the
  upgrade stays `sudo ./install.sh ... --reuse-k3s` (or the documented HA run).
- It runs in the background and is cached for six hours, including across pod
  restarts (`/data/update-check.json`), so a page load never waits on the
  network and a restart loop cannot hammer the endpoint.
- A failure (no egress, rate limit, an unparsable tag) is not an error the
  operator has to clear: the last answer stays and the chip is simply absent.
- A locally built image reports `dev`; the check stays off and
  `GET /api/version` carries the reason. `UMKIM_THIN_UPDATE_CHECK=false` turns
  it off on purpose (air-gapped installs); the Ansible default is
  `update_check: true`.

Application updates are separate: **Applications** compares each installed
chart with the catalog in this image and shows **Upgrade to X** when the
catalogued version is newer. The comparison is by version, so an install ahead
of the catalog (a candidate) is not offered a downgrade. Discovering *newer
upstream app releases* is maintainer work (a new tested catalog pin), not a
console feature.

## Status

`GET /api/status` is one aggregate poll. Per app:

| field | meaning |
|---|---|
| `helmStatus` | Helm's release status (`deployed`, `pending-upgrade`, ...) |
| `ready` | Helm is deployed **and** every Deployment/StatefulSet/Job is ready/complete |
| `routes` | hosts of the Ingresses in the app's namespace |
| `license` | catalog licence block: chart and application licence, text URL and note |
| `components` | per-workload `kind/name/ready/status` |
| `bootstrap` | media stack only: `not-configured`, `pending`, `running`, `succeeded`, `failed` |
A release in `pending-*` is shown as **configuring** (amber), not as down: that
is what a normal upgrade or bootstrap looks like. Readiness is cluster
readiness, not an HTTP probe.

## Trusted access

`GET /api/config` also reports `trustedAccessEnabled`,
`trustedManagementEnabled`, `trustedManagementCIDRs`, and
`trustedManagementExcludeCIDRs`. The values come from the generated `config.env`
(`UMKIM_THIN_TRUSTED_ACCESS_ENABLED`,
`UMKIM_THIN_TRUSTED_MANAGEMENT_ENABLED`,
`UMKIM_THIN_TRUSTED_MANAGEMENT_CIDRS`,
`UMKIM_THIN_TRUSTED_MANAGEMENT_EXCLUDE_CIDRS`); the console does not accept a CIDR from
a browser request. Catalog routes opt in with `trusted.management` /
`trusted.vpn` and must declare `appAuth`. The console applies the public route,
the high-priority management `IngressRoute` (`ClientIP`, no Authelia), and the
same-host `vpn` Ingress (no Authelia, `vpn-only` allowlist) together.

## Storage classes

`GET /api/config` returns the classes, the default marker, and access-mode
hints. Kubernetes does **not** declare access modes on a StorageClass (they are
a PVC property), so the API uses the first source that knows:

1. `UMKIM_THIN_STORAGE_ACCESS_MODES` (`class=RWX+RWO,...`, `configured`);
2. the platform's conventions (`nfs`/`nfs-client` provisioners and `*-rwx`
   classes → RWX, `local-path`/Longhorn → RWO, `known`);
3. for classes with no convention, access modes observed on bound PVs of that
   class (`observed`). A single PVC can ask for RWO on a class that is also
   capable of RWX, so observation is not authoritative for known classes;
4. otherwise `unknown`, and the install form warns rather than guessing.

The media picker warns when the chosen class does not advertise RWX. It never
hides a class.

The install form has a **Media access mode** select next to the media class. It
defaults to the site's mode and follows the selected class: a class that
advertises RWX selects `ReadWriteMany`, an RWO class selects `ReadWriteOnce`.
The install request carries the value explicitly, so choosing an NFS class on a
single-node install switches the media PVC to RWX even though the site default
is RWO.

The defaults come from `UMKIM_THIN_STORAGE_CONFIG_CLASS`,
`UMKIM_THIN_STORAGE_MEDIA_CLASS` and `UMKIM_THIN_STORAGE_MEDIA_ACCESS_MODE`.
The on-host `install.sh` path sets media to `local-path`/`ReadWriteOnce`; the
Ansible single path sets `umkim-thin-nfs`/`ReadWriteMany`, and HA sets
`longhorn-rwx`/`ReadWriteMany`. The warning on `local-path` is expected: a
single node can run media on RWO, but an HA cluster needs a shared class.

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

The External NFS form's settings:

| Setting | Default | Validation |
|---|---|---|
| Storage name | — | 2–31 lowercase letters, digits or hyphens; unique across local and external exports |
| NFS server | — | Hostname or IP address; must accept TCP/2049 from the console pod |
| Export path | — | Absolute path, not `/`, no `..`, spaces or commas |
| StorageClass | `nfs-<name>` | 2–31 characters; lowercase letters, digits, hyphens and dots |
| Mount options | `nfsvers=4.2,rw,nolock` | Comma-separated; `rw`/`ro`/`sync`/`async`/`hard`/`soft`/`lock`/`nolock`/`noatime`/`relatime`, plus numeric `nfsvers=`/`vers=`/`port=`/`mountport=`/`timeo=`/`retrans=`/`rsize=`/`wsize=`/`actimeo=` |

External NFS state is kept on the console PVC and is merged into
`GET /api/storage` with the host exports. The name must be unique across both
paths because each maps to one Helm release.

The server-side setup — exports, firewall, client CIDRs and verification — is in
[Use an NFS server on another host](quickstart/12-external-nfs.md).

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

### Advanced install options

Every install panel has an **Advanced: namespace, storage and resources**
section. Opening it renders the chart once and lists each
`PersistentVolumeClaim` with its current storage class, size and access mode,
plus every workload container with its CPU/memory requests and limits. The
operator can override the class and size per claim and the resources per
container before installing. The install uses a Helm post-renderer to change
those manifests, so this works for charts whose values use different paths for
each volume or container.

The **Validate resources & storage** button renders the same plan and checks the
live cluster before Helm runs. It reports blocking errors for invalid
quantities, missing classes, impossible Longhorn nominal capacity, and resource
requests that cannot fit after existing requests; it reports warnings for
things Kubernetes can only estimate, such as NFS export capacity, missing
requests, and overcommitted limits. Installations run the same preflight on the
server and refuse to create a release with blocking errors.

The preflight resource fit is an aggregate estimate across nodes. It catches
obviously impossible requests, but node affinity, taints, topology and
per-node fragmentation can still affect placement. Keep a namespace
`ResourceQuota` for a hard cluster-level ceiling.

The namespace field is available for normal applications. It is fixed for
`servarr` and `wireguard`, whose bootstrap/gateway wiring has namespace-specific
names. A new app can be installed into a custom DNS-label namespace; an existing
release cannot be moved, and the platform's system namespaces are rejected.
The chosen namespace is recorded with the app state, so status, routes and
uninstall continue to use it.

The advanced storage form is for new installs. Existing PVCs cannot change
storage class, and most cannot be shrunk. The catalog defaults below are
deliberately sized for small metadata volumes; use the advanced form when a
particular app needs a larger volume or a move to NFS.

### Default language and quality

The **Nextcloud install form** has a **Default app language** selector; English
is the default. It sets Nextcloud's `default_language` for new users and the
local admin. Existing users keep their own language. **Immich** has no
server-side install language: the Immich UI follows the user/browser setting and
can be changed in Immich's own user settings.

The **Gitea install form** exposes a default/fallback UI locale. Gitea still
prefers a matching browser locale; the selected locale is the fallback and the
first entry in Gitea's language list. Users keep their own per-user choice.

The **Paperless install form** exposes the Tesseract **OCR language** (English
by default). It does not control Paperless's UI language, which follows the
browser/user locale; Paperless has no server-side UI-language default.

The **Servarr install form** exposes the media stack's defaults:

- **Default language** (English by default; Hungarian and the other Radarr languages are selectable):
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

## Optional Mealie → SparkyFitness integration

When both Mealie and SparkyFitness are installed, the SparkyFitness row in
**Applications** has a one-click **Connect Mealie** action. It is optional and
does not run during either app's install.

SparkyFitness must have at least one user before the provider can be written.
Open SparkyFitness and sign in once (the OIDC login creates the admin account)
before clicking **Connect Mealie**; the action reports that prerequisite and
removes the temporary Mealie token if no user exists.

The action:

1. creates a dedicated `sparkyfitness` API token in Mealie with the Mealie
   admin account from the release Secret;
2. encrypts that token with SparkyFitness's own
   `SPARKY_FITNESS_API_ENCRYPTION_KEY`;
3. creates (or updates) one **global** Mealie food provider in the
   SparkyFitness database;
4. runs a short `psql` Job in the SparkyFitness namespace to upsert the
   provider. The Job reads the database password from the release Secret and
   the SQL script from a temporary Secret; no `pods/exec` grant is needed.

There is no manual key or dialog: SparkyFitness has no supported
service-account/API-key handoff, so the console writes the single provider row
with the same AES-256-GCM key and schema the application uses. The token never
appears in argv, the console log, or a ConfigMap. A second click is a no-op
when the provider already points at this Mealie Service; a failed database
write removes the temporary Mealie token. New installations get the required
console and SparkyFitness → Mealie NetworkPolicies from the Ansible netpol
template; an older installation with default-deny policies must re-run that
template.

The provider uses the in-cluster URL
`http://mealie.<namespace>.svc.cluster.local:9000`. Recipes flow from Mealie
into SparkyFitness; nothing is copied back. See the SparkyFitness chart README
for the manual UI path and the private-network SSRF trade-off.

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
| GET | `/api/catalog` | catalog entries, including chart/application licences and upstream links |
| GET | `/api/config` | zone, portal, `homeUrl`, running `version`, default classes and `mediaAccessMode` |
| GET | `/api/version` | running version, newest published release, `available`, release URL and the last check time |
| GET | `/api/apps` | installed catalog releases (chart, app version, helm status, routes, licence, upstream links) |
| POST | `/api/apps` | install with `version`, per-route `exposure`/`certificate`, optional `namespace`, `mediaAccessMode`, per-PVC `pvcs`, and per-container `resources` overrides (async job) |
| GET | `/api/apps/{name}` | one app's health, chart/app version and routes |
| GET | `/api/apps/{name}/release` | Helm metadata plus user-supplied values, with secret-looking values redacted |
| GET | `/api/apps/{name}/storage` | render the chart and list its PVCs/classes/sizes |
| GET | `/api/apps/{name}/plan` | render the chart's PVCs and workload containers |
| POST | `/api/apps/{name}/validate` | preflight storage/resource fit for the same install options |
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
| POST | `/api/apps/{name}/integrations/mealie` | one-click optional SparkyFitness → Mealie provider |
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
`umkim-thin-config` ConfigMap. The same config is used by both targets. The
settings the console reads:

```env
# Set by the chart and the image, not by hand
UMKIM_THIN_HOME_HOST=home.example.com   # chart: derived from consoleHost
UMKIM_THIN_VERSION=0.2.0                # image: the release tag it was built from

# Storage defaults shown by the install form
UMKIM_THIN_STORAGE_CONFIG_CLASS=local-path
UMKIM_THIN_STORAGE_MEDIA_CLASS=local-path        # Ansible single: umkim-thin-nfs; HA: longhorn-rwx
UMKIM_THIN_STORAGE_MEDIA_ACCESS_MODE=ReadWriteOnce  # Ansible/HA: ReadWriteMany
UMKIM_THIN_STORAGE_ACCESS_MODES=class=RWX+RWO,...   # optional access-mode override

# On-demand bootstrap
UMKIM_THIN_CONSOLE_IMAGE=localhost/umkim-thin:dev
UMKIM_THIN_BOOTSTRAP_IMAGE=localhost/umkim-thin/servarr-bootstrap
UMKIM_THIN_BOOTSTRAP_CONTEXT=/opt/umkim-thin/bootstrap
UMKIM_THIN_BUILDER_IMAGE=gcr.io/kaniko-project/executor:v1.23.2-debug@sha256:...
UMKIM_THIN_BUILD_NAMESPACE=umkim-thin-build
UMKIM_THIN_BOOTSTRAP_NODE=<first server's hostname>

# Console release check (read-only; false for an air-gapped install)
UMKIM_THIN_UPDATE_CHECK=true
UMKIM_THIN_UPDATE_CHECK_URL=https://api.github.com/repos/rborso/umkim-thin/releases/latest

# Trusted access
UMKIM_THIN_TRUSTED_ACCESS_ENABLED=false
UMKIM_THIN_TRUSTED_MANAGEMENT_ENABLED=true
UMKIM_THIN_TRUSTED_MANAGEMENT_CIDRS=
UMKIM_THIN_TRUSTED_MANAGEMENT_EXCLUDE_CIDRS=

# WireGuard gateway
UMKIM_THIN_WG_NAMESPACE=wireguard
UMKIM_THIN_WG_SUBNET=192.168.168.0/24            # HA default: 192.168.169.0/24
UMKIM_THIN_WG_SERVER_IP=192.168.168.1
UMKIM_THIN_WG_PORT=51820
UMKIM_THIN_WG_ENDPOINT=<public-address>:51820
```

`install.sh` writes the first storage block with `local-path` and
`ReadWriteOnce`; the Ansible path writes `umkim-thin-nfs`/`ReadWriteMany` on
single and `longhorn-rwx`/`ReadWriteMany` on HA. The console does not accept
storage-class or trusted-CIDR values from a browser request; they come from
this ConfigMap.

## Credentials

`./ci/credentials.sh single|ha` prints the console/directory secrets and the
media stack's usernames/passwords, **masked**; `--reveal` prints the values. It
reads them from the cluster and never writes a file.
