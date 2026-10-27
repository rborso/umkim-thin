# Console: applications, status, settings, and on-demand bootstrap

Status: **implemented** (phases 0–3; phase 4's hardening, docs and tests are
in). The design below is kept as the record; see `docs/console.md` for the
behaviour and "Implementation notes" at the end for the points that changed
while building it. Two follow-ups are explicit rather than silently skipped:
the console's ClusterRole is still broad (Helm installs arbitrary charts), and
the Applications section is checked by `ci/assert.sh` rather than a dedicated
Playwright suite.

## Decisions

1. **Applications section** — a separate section (not the installed table) with
   the app's links and live status.
2. **Status** — one poll for all installed apps, refreshed every **120 s**
   (server-side cache 45 s). Readiness = Helm status + workloads, not an HTTP
   probe (phase 1); an HTTP probe can be added later.
3. **Storage classes** — read live from the cluster, default to the class marked
   `is-default-class`, show access modes, and **warn** (do not hide) when the
   media class is not RWX.
4. **Bootstrap policy** — default: **build the image if it is missing, then run
   the bootstrap**. The stack has no tracker dependency; the app is installed and
   setup is offered. A build/bootstrap failure never fails the app install.
5. **Images** — upstream images are pulled as before. The console and servarr
   bootstrap are internal images built locally by the installer/console; they
   are not prebuilt or published.
6. **Where the build runs** — on the host: on HA, the **first/bootstrap node**,
   pinned. The image is imported into that node's containerd and served from
   there (`imagePullPolicy: Never`).
7. **Indexers** — optional. They can be added manually in Prowlarr, or through
   the console's generic Prowlarr passthrough (see `docs/console.md` and
   `docs/servarr-indexers.md`). The bootstrap creates the download client and
   the FlareSolverr proxy, not a tracker integration; no tracker is required
   for installation.
8. **Credentials helper** — `ci/credentials.sh` is committed but **masked by
   default**; `--reveal` prints values; it never writes a file.

## Security requirements (apply to every phase)

- **Secrets**: values are accepted over the console's authenticated session,
  written to the Secret via stdin (`kubectl create secret --from-env-file=-`),
  never put on a command line, never logged, never returned by the API (only
  `configured` and the username), masked in the UI.
- **Privileged build**: the only privileged workload is the builder Job.
  - Dedicated namespace `umkim-thin-build`, labelled
    `pod-security.kubernetes.io/enforce=privileged` (the only namespace that is);
    app namespaces stay baseline/restricted.
  - Dedicated ServiceAccount, `automountServiceAccountToken: false`.
  - No `hostPID`, no `hostNetwork`; `seccompProfile: RuntimeDefault`;
    `readOnlyRootFilesystem`; drop all capabilities; `activeDeadlineSeconds`
    (30 m) and resource limits.
  - Mounts only the build context (read-only) and the containerd socket (for the
    import); no host paths for data.
  - NetworkPolicy: egress only to the base-image registry and cluster DNS.
  - The build context is **fixed by the platform** (vendored in the console
    image); the API never accepts a Dockerfile or context.
  - Base image **pinned by digest**.
- **Audit**: every privileged action (settings change, build, bootstrap) writes
  one console log line (actor, action, app, node; no values) and a Kubernetes
  Event / Job annotation.
- **Known residual**: the console's ServiceAccount can create pods cluster-wide,
  so it is already node-root-equivalent. The builder does not widen that, but
  phase 4 narrows the RBAC (namespace-scoped Roles instead of one ClusterRole).

## Phase 0 — groundwork (no UX change)

- `internal/app`: `List()` currently returns the Helm status in `Routes`; return
  the app's **Ingress hosts** and put the Helm status in a new `HelmStatus`
  field. `Result` gains `helmStatus`.
- `StorageClasses()` returns `{name, default, accessModes}`; the default comes
  from the `is-default-class` annotation when the env default is unset.
- `/api/config` exposes the richer storage classes; the UI shows
  `name (RWO,RWX)` and warns when media is not RWX.
- Vendored bootstrap source: `images/servarr-bootstrap/` (from the chart repo at
  a pinned commit; license/NOTICE; Dockerfile pinned by digest). The console
  image copies it to `/opt/umkim-thin/bootstrap`.
- Chart (`servarr`): add `bootstrap.imagePullPolicy` (default `IfNotPresent`)
  and `bootstrap.nodeSelector`; the bootstrap Job gets
  `automountServiceAccountToken: false` and resource limits.
- Acceptance: `/api/apps` returns real hosts; storage pickers show access modes;
  existing e2e stays green.

## Phase 1 — Applications section and status

- `GET /api/status` (cached 45 s): per installed app
  `{name, namespace, helmStatus, ready, routes, configured, bootstrap, components:{ready,total,notReady}, checkedAt}`.
  `bootstrap` is `not-configured | pending | running | succeeded | failed`.
- UI: a separate **Applications** section with rows/tiles: name, status chip,
  primary link + all routes, last checked, and a Refresh button; polls every
  120 s. The existing per-app Status button remains for detail.
- Acceptance: five apps green when healthy; a scaled-down deployment turns its
  chip red within one poll; Links open in a new tab.

## Phase 2 — Media-stack settings (ncore) — removed

This phase was implemented and then removed. Shipping a private-tracker
integration made tracker credentials a required part of the install, added a
write-only credential surface to the console, and tied the release to one
community. The stack now configures its own services. Indexers are optional:
they can be added in Prowlarr, or through the per-Servarr-instance Prowlarr
passthrough, which has no tracker-specific dependency or credential storage.
See `docs/console.md` and `docs/servarr-indexers.md`.

## Phase 3 — Build and bootstrap orchestration

- `POST /api/apps/servarr/setup` (async job, same job store as installs):
  1. **probe** the image on the pinned node (a Job with
     `imagePullPolicy: Never`, `command: ["true"]`): Completed → present,
     `ErrImageNeverPull` → missing. Unprivileged.
  2. **build** if missing: builder Job (see security) on the pinned node; the
     context from the console image; `buildah` (vfs/chroot, no extra caps) →
     OCI tar → `ctr -n k8s.io images import` via the mounted socket. Tag =
     chart version + commit, so a rebuild is skipped when unchanged.
  3. **enable** the bootstrap: `helm upgrade servarr --reuse-values --set
     bootstrap.enabled=true --set bootstrap.image=<tag> --set
     bootstrap.imagePullPolicy=Never --set bootstrap.nodeSelector.kubernetes\.io/hostname=<node>`.
  4. **run** the Job, poll it, capture the log (redacted) and update the app's
     `bootstrap` state.
- Install flow: after the Servarr install job, if credentials are configured →
  enqueue setup automatically; otherwise set `not-configured` and show
  Configure. A failed build/setup leaves the app installed and retryable.
- Pinned node: `UMKIM_THIN_BOOTSTRAP_NODE` (set by provisioning to the primary);
  fallback = the node the console runs on, else the only node.
- Acceptance: fresh HA install builds on the first node and bootstraps; deleting
  the image and re-running setup rebuilds it; a retried setup is idempotent.

## Phase 4 — Hardening, tooling, docs

- Dedicated build namespace + PodSecurity labels; builder NetworkPolicy.
- RBAC tightening (follow-up): namespace-scoped Roles for the console instead of
  one broad ClusterRole; keep the ability to create the build namespace only.
- `ci/credentials.sh`: masked by default, `--reveal`, never writes a file.
- `docs/console.md` (applications, status, settings, setup) and updates to
  `docs/full-build.md` and `AGENTS.md`.
- Tests: Go unit tests for the status/route parsing, storage-class parsing and
  Secret merge; e2e: after install, `/api/status` reports configured; a console
  Playwright check for the Applications section.

## Files touched

- `internal/app/app.go` — list routes, storage-class info, status cache,
  settings/Secret merge, setup orchestration.
- `cmd/umkim-thin/api.go` — `/api/status`, settings endpoints, setup job.
- `cmd/umkim-thin/ui/index.html` — Applications section, storage pickers, setup
  panel.
- `catalog/servarr.yaml` — bootstrap defaults the console overrides at install.
- `charts/apps/servarr` — `bootstrap.imagePullPolicy`,
  `bootstrap.nodeSelector`, hardened Job.
- `images/servarr-bootstrap/` — vendored source (FOSS, licensed) + pinned
  Dockerfile; `Dockerfile` (console image) copies it.
- `ci/enable-servarr-bootstrap.sh` — the manual/lab fallback; share the same
  tag scheme.
- `ci/credentials.sh`, `docs/console.md`, `docs/full-build.md`, `AGENTS.md`.

## Risks

- The builder needs network to pull the base image (~1.5 GB) and disk; document
  it, and skip the build when the tag already exists on the node.
- The pinned node is a single point for the bootstrap image; if it is down the
  setup cannot run (the app stays installed).
- Upstream app images remain pulled; only our images are built (no published
  artifacts from this project).

## Implementation notes (what changed while building it)

- **Storage access modes** are not a StorageClass field. The API reports the
  modes observed on bound PVs, an optional configured map, then known platform
  conventions; anything else is `unknown` and the UI warns. It never invents an
  RWX guarantee.
- **The Secret is created at install**, not by the bootstrap: the chart's
  qBittorrent initContainer needs the WebUI password before the pod starts. App
  passwords are generated once; the chart API keys are copied in after install.
- **The builder is kaniko**, not buildah: buildah needs a user namespace and
  capability juggling that the hardened pod denied. Kaniko needs no namespaces
  or extra capabilities; only the import uses root and the containerd socket.
  The builder image is pinned by digest. The container rootfs is writable
  because kaniko unpacks the base there; nothing on the host is writable.
- **nCore has no passkey field** in Prowlarr's definition (username, password,
  `2facode`) — historical note: the tracker integration was removed from the
  public tree.
- **Idempotency**: a successful run remembers the image tag; setup skips the
  Job when the image is unchanged, because the chart's Jellyfin wizard step
  cannot be re-run on an already-configured instance.
- **Catalog apps only**: `/api/apps` (and the Applications view) hides the
  platform's own Helm releases, and uninstall refuses anything not in the
  catalog.
- **Jobs** moved to `/api/jobs`: Go's ServeMux rejects
  `/api/apps/jobs/{id}` next to `/api/apps/{name}/settings`.
- The image tag is `VERSION-<commit>` (baked into the console image), with a
  content hash as the fallback for a local build without a commit.

## Validation

- Full profile on **single**: the console stored the credentials, built
  `localhost/umkim-thin/servarr-bootstrap:0.2.23-<commit>` on the single-node
  target, imported it, ran the Job to success, and the platform assertions plus the
  21 journeys of the time passed. After the login tests moved into the gate,
  the live single cluster ran the 39-test gating suite
  (`apps.spec.ts auth.spec.ts tls.spec.ts`) successfully.
- Full profile on **HA** (with Nextcloud now in `ci_apps`, so both targets
  install the same apps): the console built on the pinned first node and
  bootstrapped the stack; `ci/portal-user.sh` created the
  regular tenant `ci-portal`; the 39-test gate passed, including Gitea,
  Jellyfin and Nextcloud LDAP logins and the real OIDC logins for Immich and
  Paperless; 17/17 journeys passed.
- That fresh HA install also caught a regression in the LDAP fix: removing
  `userSearchBase` while adding Nextcloud's `baseDn` broke new Gitea/Jellyfin
  installs (the single cluster's apps predated the change). The installer now
  sets every spelling from the site base, and the HA run above validates it.
- A second setup with the same image and credentials returns
  `skipped: true`; saving settings with a blank password keeps the stored one.
- Residual: the console's ClusterRole remains broad because Helm installs
  arbitrary charts; the builder has its own token-less ServiceAccount and the
  only privileged namespace. Narrowing the console itself needs a split between
  the chart-installer identity and the API identity, which is a separate
  change.
