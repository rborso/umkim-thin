# Backups and restore

The console owns backups. It writes one archive per run and asks the storage for
a snapshot per application PVC. Two halves, deliberately:

| Half | What it is | Where it lives |
|---|---|---|
| **Metadata** | Console state (invites, app state, WireGuard peers, per-app Helm values), the installer seed (secrets, identity values, OIDC clients), the installed app inventory, catalog pins | One `.tar.gz` on the console PVC (`/data/backups`) |
| **Data** | Application PVCs (Nextcloud files, Immich photos, Paperless documents, Gitea repositories, media) | `VolumeSnapshot` objects created through the CSI driver |

A metadata-only backup cannot rebuild an app's data, and a data snapshot without
the state cannot rebuild the console. Keep both.

## Schedule

Set by the installer and read from the console config:

```env
UMKIM_THIN_BACKUP_DIR=/data/backups
UMKIM_THIN_BACKUP_KEEP=7              # archives kept; 0 keeps everything
UMKIM_THIN_BACKUP_INTERVAL=24h        # "0" or "off" disables the schedule
UMKIM_THIN_BACKUP_SNAPSHOTS=true      # create VolumeSnapshots
```

The schedule runs inside the console process: one archive per interval, then a
prune to `KEEP`. A restart does not trigger an immediate backup. There is no
cron expression — a daily archive is the supported granularity; use the storage's
own schedule for anything finer.

**The archives live on the console PVC.** Copy them off the host (`scp`, an NFS
mount, or a backup agent) or the backup dies with the disk it protects. On HA
with Longhorn, the PVC itself is replicated; that is redundancy, not a backup.

## What one archive contains

```text
manifest.json                     schema, created, version, zone, app inventory,
                                  checksums, snapshot list, warnings
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

   In the console: **Backups → Restore**. It does the same state restore and
   reports the snapshots to use for the data step.

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
  one app's data is the snapshot/PVC step above.
- **No encryption.** Archives are mode 0600 and contain secrets (`secrets.yml`,
  app values). Treat a copy like the cluster itself; encrypt it if it leaves the
  host.

## Drill

[`restore-drill.md`](restore-drill.md) is the full procedure, including the
etcd/cluster half that only the host-side `install.sh --backup` covers. Run it
once per release: a backup you have never restored is a hypothesis.
