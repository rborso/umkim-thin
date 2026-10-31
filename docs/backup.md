# Backups and restore

The console writes two kinds of backup, deliberately separate:

| Kind | Covers | Archive |
|---|---|---|
| **Data backup** | the console state, the app inventory and the selected app volumes (CSI snapshot, or a full file copy when the storage cannot snapshot) | `umkim-thin-<ts>.tar.gz`, plus `<archive>-data-<ns>-<pvc>.tar.gz` copies |
| **Platform backup** | the cluster itself: every Kubernetes object (including Secrets), the console and host state, and the platform volumes (LLDAP, Authelia, Traefik ACME) | `umkim-thin-<ts>-platform.tar.gz`, plus copies for the platform volumes |

A data backup cannot rebuild the cluster, and a platform backup does not contain
application data. Keep both.

## Schedule

Edit it in **Backups → Automatic backups**: enable or disable it, set the
interval, the retention and how volume data is captured (below). The console
saves it to `state/backup-schedule.json` on its PVC (a restart keeps it, and a
restore brings it back) and applies a change immediately — no restart and no
reinstall.

The installer's environment variables are only the initial default, used until
the console saves a schedule:

```env
UMKIM_THIN_BACKUP_DIR=/data/backups
UMKIM_THIN_BACKUP_KEEP=7              # archives kept; 0 keeps everything
UMKIM_THIN_BACKUP_INTERVAL=24h        # "0" or "off" starts disabled
UMKIM_THIN_BACKUP_SNAPSHOTS=true      # create VolumeSnapshots
```

The schedule runs inside the console process: one archive per interval, then a
prune to the configured keep. The next run is measured from the newest archive,
so a restart or a manual backup does not duplicate a run; with no archive at
all, enabling the schedule takes one promptly. There is no cron expression —
hours are the granularity; use the storage's own schedule for anything finer.

The CLI edits the same file; the console notices an external change within a
minute:

```bash
umkim-thin backup schedule                      # show
umkim-thin backup schedule --interval 12h       # enable, every 12h
umkim-thin backup schedule --interval off       # disable
umkim-thin backup schedule --keep 5 --data auto
```

The **Data** setting is one of:

| Mode | What it does |
|---|---|
| `off` | Metadata only: state, seed, app inventory. No volume data. |
| `snapshot` | A CSI `VolumeSnapshot` per volume; a warning when the storage cannot (NFS, local-path, no class). The default. |
| `auto` | Snapshot where the storage supports it, **full file copy** for the rest. |
| `copy` | Always copy the volume contents into a `.tar.gz` (slow, but works on NFS and local-path). |

## Where backups go

The console writes to one **target** at a time; pick it in **Backups → Target**
(the same setting is `backup schedule --target PATH`):

- **console** — the console PVC (`/data/backups`, `UMKIM_THIN_BACKUP_DIR`). The
  default.
- **mounted targets** — the installer mounts an NFS export under
  `/backups/<name>`: `install.sh --backup-nfs server:/absolute/path` adds `nfs`
  at `/backups/nfs`, and a re-run keeps it. The chart also accepts a `pvc` or
  `hostPath` target (`backupTargets` in the console values).

The target must be writable by the console's UID (65532); the screen shows each
target's free space and refuses an unwritable one. Switching is immediate: the
archive list, retention and restores follow the selected target. Archives on
another target stay where they are — switch back to see them.

## Platform backup

**Backups → Platform backup…** (CLI: `umkim-thin backup platform`, API:
`POST /api/backups/platform`). It writes one archive with:

```text
platform/k8s-objects.yaml    every Kubernetes object as a List, sanitized for
                             re-apply (namespaces, CRDs, workloads, services,
                             ingress, configmaps, secrets, PVCs, RBAC,
                             Traefik/Helm CRs)
platform/README.txt          the rebuild checklist
state/                       the console PVC (state, secrets, per-app values)
seed/                        the host seed mounted into the console
<archive>-data-*.tar.gz      identity (LLDAP, Authelia) and Traefik ACME volumes
```

Transient objects (pods, events, endpoints, replicasets, leases) are omitted,
as are PersistentVolumes, Jobs and VolumeSnapshots: their controllers recreate
them. Server-assigned metadata, `status`, controller ownership, Service IPs and
PVC bindings are stripped so the export can be replayed. Helm release Secrets
are too large for client-side apply, so the restore uses server-side apply.

The object export is the API-level equivalent of an etcd dump; for a byte-exact
etcd snapshot use the host-side `install.sh --backup` (which also archives the
k3s datastore).

Restoring a platform archive is deliberately manual: the console restores
`state/` and `seed/` (like a data backup), but never applies the object export
or overwrites a volume. The checklist is:

1. install a fresh platform of the same version;
2. `kubectl apply --server-side --force-conflicts -f platform/k8s-objects.yaml`;
3. stop the identity stack and extract the volume copies into their PVCs;
4. `umkim-thin backup restore <archive> --apps`.

Platform archives use the same target and retention as data backups; the list
shows the kind.

## Full data copy

When the mode is `auto` (and the storage cannot snapshot) or `copy`, the console
copies each selected volume's files:

- It creates a short-lived pod in the app's namespace that mounts the PVC
  **read-only** (as root, because app data is often owned by another UID), runs
  `tar czf -` and is deleted afterwards. The token is not mounted and the
  container root filesystem is read-only.
- The stream is written to `<archive>-data-<namespace>-<pvc>.tar.gz` next to
  the metadata archive, with a SHA-256 and size in the manifest. `backup verify`
  checks them; deleting or pruning the archive removes the copies too.
- The copy is **crash-consistent**, not application-consistent: a running
  database may write while it is copied. Stop the app for a clean copy.
- Before copying, the console measures the volume (`du`) and refuses a volume
  that would leave less than 2 GiB free on the backup filesystem. A full copy is
  a last resort, not a reason to fill the disk.

Extract a copy back into a volume with a helper pod that mounts the target PVC
read-write (stop the app first), copy the `.tar.gz` in, and `tar xzf` it at the
mount point. The console lists the copies in **Backups** (the Data column links
to each file) and never overwrites live data itself.

**The archives live on the console PVC.** Copy them off the host (`scp`, an NFS
mount, or a backup agent) or the backup dies with the disk it protects. On HA
with Longhorn, the PVC itself is replicated; that is redundancy, not a backup.

## Choosing what to back up

**Back up now** takes a full backup by default. Open **Choose apps and
volumes** to narrow it: tick the apps to record and the volumes to snapshot.
Ticking an app also ticks its volumes; a volume can also be selected on its own.

The selection means exactly this:

| Part | What the selection changes |
|---|---|
| App inventory | Only the selected releases are recorded in the manifest, so `restore --apps` reinstalls only those |
| Volume snapshots | Only the selected PVCs get a `VolumeSnapshot` |
| Console state | **Always archived whole.** It is small and global (invites, peers, app state); a filtered copy would restore a console that disagrees with itself |

Consequences worth knowing:

- Selecting apps but no volumes produces a metadata-only archive: fast, and
  useful before an upgrade.
- Selecting a volume the storage cannot snapshot (NFS, local-path) is a
  **warning**, not a failure: the archive is still written and names the
  volumes it does not cover. The console marks those volumes `no snapshot` and
  leaves them out of the default selection; back them up on the NFS server or
  with a VM snapshot.
- A partial archive still restores the **whole** console state. The data step
  only has snapshots for the volumes you selected.
- The nightly schedule always takes a full backup; selection is per run.

From the CLI:

```bash
umkim-thin backup scope                        # list apps and volumes
umkim-thin backup create                       # full
umkim-thin backup create --app immich --volume immich/immich-data
umkim-thin backup list                         # SCOPE: full or partial
```

The API is `GET /api/backups/scope` (the choices) and `POST /api/backups` with
`{"apps":["immich"],"volumes":["immich/immich-data"]}`. An empty body (or
`{"all":true}`) is a full backup; an explicit empty selection is rejected, so a
mistake cannot produce an archive that covers nothing.

## What one archive contains

```text
manifest.json                     schema, created, version, zone, app inventory,
                                  checksums, snapshot list, warnings, selection
state/                            the console PVC (minus backups/ and chart-mirror/)
state/invites.json                pending and redeemed invitations
state/apps-state.json             installed namespaces, route selections
state/wg-clients.json             WireGuard peers
state/identity-apps.yml           OIDC clients the console registered
state/apps/<app>-values.yaml      the exact Helm values each app was installed with
state/secrets.yml                 the generated secrets (copied to the PVC on boot)
state/identity-values.yml         identity stack values
seed/config.env                   the installer's console config (archived, not restored)
```

`chart-mirror/` is excluded: the installer re-packages it from the checkout, and
it is reproducible. App *values* are included — that is what makes a reinstall
possible.

## Restore

Two steps, in this order:

1. **Data.** Create a PVC from the archive's snapshot and let the app's
   StatefulSet/Deployment mount it. The console prints the snapshot names; it
   does not apply them, because replacing a live PVC is a destructive operation.
   On Longhorn that is a `VolumeSnapshot` → `PersistentVolumeClaim` restore; on a
   CSI driver without snapshots, restore the volume from the VM or NFS backup.
2. **State.** Restore the archive:

   ```bash
   # in the console pod, or from the host with the same config
   umkim-thin backup restore umkim-thin-20261102T030000Z.tar.gz --apps
   ```

   `--apps` reinstalls the catalog apps from the archive that are missing, at the
   chart version they had. The restore always writes a **safety archive** of the
   current state first, so a wrong archive can be undone by restoring that one.

   Add **`--data`** to put the recorded volume copies back. Workloads that mount
   a volume are scaled to zero, the copy is extracted into the PVC, and they are
   started again; a workload that cannot be stopped (a DaemonSet, a Job, a bare
   pod) blocks that volume instead of writing under a live app. For a platform
   archive, **`--platform`** applies `platform/k8s-objects.yaml` with server-side
   apply. Both require `--confirm`:

   ```bash
   umkim-thin backup restore <platform> --data --platform --confirm
   umkim-thin backup restore <data>     --data --apps --confirm
   ```

   In the console: **Backups → Restore** offers the same two checkboxes. The full
   rebuild sequence is in [`disaster-recovery.md`](disaster-recovery.md).

The console reads `secrets.yml` and the session key at startup; restart the
console pod after a restore that changes them (`kubectl -n umkim-thin rollout
restart deploy/umkim-thin-console`).

`seed/config.env` is archived for reference but **not** restored: the installer
owns it, and silently reverting a zone or storage class would be worse than
asking the operator to re-run the installer.

The generated secrets and identity files (`secrets.yml`, `identity-values.yml`,
`identity-apps.yml`) *are* restored — without the OIDC clients every OIDC app
fails at the issuer with a 404.

## Verify

`umkim-thin backup verify <name>` recomputes every checksum and drains the gzip
stream, so a truncated or flipped archive fails instead of restoring garbage.
A restore refuses an archive that does not verify. `GET /api/backups` lists
archives with their manifest; `GET /api/backups/<name>/download` returns the
archive.

## Retention and deletion

`KEEP` applies to archives; deleting an archive also deletes the snapshots it
recorded (best effort). Snapshots are named
`ut-<timestamp>-<pvc>-<hash>`, so they are identifiable without the archive.

## Limitations

- **No remote target.** The archive directory is local. For offsite copies, sync
  `/data/backups` (or point `UMKIM_THIN_BACKUP_DIR` at an RWX class) and back up
  the snapshot storage separately.
- **Snapshots need a CSI driver that supports them.** The HA installer provides
  the standard snapshot API (`external-snapshotter` v8.7.0) and a
  `longhorn-snapshot` VolumeSnapshotClass (`parameters.type: snap`) for
  Longhorn's driver, so HA gets the data half out of the box. Only CSI-backed
  volumes can be snapshotted: PVCs on the external NFS class are skipped with a
  warning naming them — back those up on the NFS server. The single-node
  `local-path` provisioner is not a CSI driver either, so there the archive is
  metadata-only and the console says so. A cluster without a
  `VolumeSnapshotClass` still gets a valid archive.
- **No per-app restore.** A restore is console state + missing apps; restoring
  one app's data is the snapshot/PVC or extract-the-copy step above.
- **No encryption.** Archives are mode 0600 and contain secrets (`secrets.yml`,
  app values). Treat a copy like the cluster itself; encrypt it if it leaves the
  host.

## Drill

[`restore-drill.md`](restore-drill.md) is the full procedure, including the
etcd/cluster half that only the host-side `install.sh --backup` covers. Run it
once per release: a backup you have never restored is a hypothesis.
