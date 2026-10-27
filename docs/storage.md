# Storage: local host disks and external NFS

The console has two storage paths:

- **Local host** (single node only): disks, RAID and host NFS exports managed by
  `umkim-thin-storage`.
- **External NFS** (single and HA): connect an existing NFS server/export and
  create a StorageClass for it.

HA does **not** use the host-disk layer. It uses Longhorn for RWO/RWX volumes
(3 replicas by default; set `longhorn_replica_count=2` for a small lab) and can
add external NFS for media before or after the platform install.

## How it is wired

- A root host service, `umkim-thin-storage`, runs the `umkim-thin storage-agent`
  command on the single node. It listens on a Unix socket
  (`/run/umkim-thin/storage.sock`).
- The console runs as UID 65532 and reaches that socket through a hostPath
  mount; the CLI on the host uses the same socket. The console's state lives on
  its PVC, so the socket is the only hostPath the console pod mounts.
- The agent is a host service, not a privileged Kubernetes pod. It only accepts
  a fixed storage request: no arbitrary shell or command.
- The installer creates the service, installs `mdadm`, `xfsprogs` and
  `nfs-utils`, and starts it. When the socket is absent (for example on HA),
  the console hides the local disk form but keeps **External NFS** available.

## What it manages

- one or more arrays (blank disks or an existing directory);
- optional mdadm RAID0/1/5;
- XFS on disk arrays;
- one NFS export per array/directory;
- one `nfs-subdir-external-provisioner` release and StorageClass per export;
- host exports under `/etc/exports.d/umkim-<name>.exports`.

The host NFS server is the source of truth; the provisioner only consumes the
export.

## Console

**Storage** has a **Local host** / **External NFS** switch.

Local host shows:

- candidate disks, their model/serial/size/type and whether they are blank;
- existing arrays/exports and their StorageClass;
- a plan/apply form.

The **Plan** button is read-only and does not format anything, so it does not
require confirmation. Clicking **Apply** for a disk/RAID export shows a red
confirmation box; type `FORMAT` there to enable the destructive apply. An
existing-directory install never formats anything: it validates the directory
and exports it.

External NFS asks for a name, server and export path, then optionally a
StorageClass name and mount options. **Plan** probes TCP/2049 from the console
and uses `showmount -e` when available to confirm the export is advertised;
`showmount` failures are warnings because NFSv4 pseudo-roots do not always
enumerate. **Apply** repeats the check, installs the provisioner and waits for
the StorageClass; the UI shows `Checking…`/`Applying…` and the plan output
while it runs (up to five minutes). There is no `FORMAT` step and no host
export. The console records the export on the PVC at
`/data/storage-external.yml`; names must be unique across local and external
exports because both use the `nfs-<name>` Helm release.

## CLI

Run on the node:

```bash
# Discover disks; only blank, unused, non-root disks are eligible.
umkim-thin storage disks

# See the plan first.
umkim-thin storage plan \
  --name media \
  --disk /dev/disk/by-id/ata-... \
  --disk /dev/disk/by-id/ata-... \
  --raid 1 \
  --filesystem xfs \
  --mount /srv/nfs/media \
  --storage-class nfs-media \
  --client 192.168.1.0/24 --client 10.42.0.0/16

# Apply only after reviewing the plan.
umkim-thin storage apply \
  --name media \
  --disk /dev/disk/by-id/ata-... \
  --disk /dev/disk/by-id/ata-... \
  --raid 1 \
  --confirm FORMAT

# Existing filesystem/directory: no mkfs, no RAID.
umkim-thin storage import-dir \
  --name archive \
  --path /srv/archive \
  --storage-class nfs-archive \
  --client 192.168.1.0/24 --client 10.42.0.0/16

# External NFS: no mkfs, no host export.
umkim-thin storage plan --name nas-media --kind external \
  --server 192.168.1.20 --export /volume1/media
umkim-thin storage apply --name nas-media --kind external \
  --server 192.168.1.20 --export /volume1/media

# Unexport + remove the StorageClass/release; data is kept.
umkim-thin storage remove --name archive
```

The console uses the socket for local host operations. External NFS is handled
by the console itself so it also works on HA; the CLI external path targets the
node's Helm/kubeconfig and is intended for a single-node host.

## Safety rules

- Only `/dev/disk/by-id/...` or `/dev/disk/by-path/...` are recommended; the
  planner also accepts a normal path but displays the stable ID.
- The root disk, its partitions, mounted filesystems, filesystems, partition
  tables, and mdadm metadata are never eligible.
- A disk operation requires `confirm=FORMAT`/`--confirm FORMAT`.
- The plan is re-checked immediately before formatting; a disk that changed is
  refused.
- XFS is the only filesystem. Mountpoints must be under `/srv/nfs/`.
- Directory exports never format and never create an array.
- `no_root_squash` is the default for the cluster media export; use
  `root_squash` or `ro` for ordinary external clients.
- NFS clients are limited to the supplied CIDRs. The default is the lab LAN,
  pod and VPN networks; never `0.0.0.0/0`.

## StorageClasses and app installation

The shared NFS class created by `provision.sh` is named `umkim-thin-nfs`; the
older name was the generic `nfs`. Environments written before the rename that
still set `storage_class_media=nfs` keep that name (the provisioner pins
`nfs_storage_class=nfs` for them). To adopt the new name, set both
`storage_class_media=umkim-thin-nfs` and `nfs_storage_class=umkim-thin-nfs`.
StorageClass names are immutable, so renaming a class on a live cluster means
recreating the PVCs (or keeping the old name in the environment); the installer
does not migrate bound claims.

For every export the agent installs a provisioner release named
`nfs-<name>` and a StorageClass named by `--storage-class` (default
`nfs-<name>`). The class appears in the console's storage-class picker; use it
for the **media** class of an app when you want that export.

Catalog entries bind their database/config volumes to
`{{ .storage.configClass }}` and their large media volumes to
`{{ .storage.mediaClass }}`. When HA has no NFS class yet, the media class is
`longhorn-rwx`; after you add external NFS in the console, select the new
`nfs-<name>` class for new app installs. Existing PVCs are never rebound or
renamed in place, so switching an existing app means reinstall or migration.

The class uses `ReadWriteMany`, `Retain`, and NFSv4.2 by default. A test PVC
reached `Bound` through the local path in the lab. External NFS defaults to
`nfsvers=4.2,rw,nolock`; mount options are validated before Helm is called.

## Longhorn is thin-provisioned

Longhorn volumes are sparse: the PVC's requested size is nominal, and actual
disk use grows with written blocks. Each volume keeps **3 replicas by default**;
set `longhorn_replica_count=2` in the HA environment when three replicas do not
fit. The scheduler still accounts for the nominal size in **Storage Scheduled**.
Its ceiling is:

```text
storageMaximum - storageReserved
```

multiplied by the over-provisioning percentage. The HA default is `100`, with
Longhorn's 25% minimum-free guard. That keeps the shared root disk safe but can
block a new 5 GiB config volume when the nominal scheduled space is already
full, even if the physical disk has free space.

You can raise the scheduler ceiling for a dedicated lab disk:

```env
longhorn_storage_over_provisioning_percentage=200
longhorn_storage_minimal_available_percentage=10
```

then re-run the Longhorn role
(`PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh --tags longhorn`).
This only changes scheduling; it does not create physical space. Add external
NFS for large Nextcloud/Immich/Paperless media as soon as possible; until then
those volumes use `longhorn-rwx` and consume replicas. Do not raise the
percentages on a root disk that also hosts the OS and containerd.

## Removal and recovery

`storage remove` removes the export file, refreshes exports, uninstalls the
provisioner release, and forgets the state. It **does not** unmount the array,
delete the filesystem, or remove the data. The `nfs-subdir` provisioner keeps
created subdirectories on the export.

For a failed array, use the normal host tools:

```bash
mdadm --detail /dev/md/<name>
mdadm --assemble --scan
mount -a
exportfs -rav
systemctl status nfs-server
```

The state file is `/etc/umkim-thin/storage.yml`; `/etc/mdadm.conf` and
`/etc/fstab` are updated for persistence.

## Limits

- local host disks are single node only; external NFS works on single and HA;
- external NFS does not verify the remote export is reachable until the
  provisioner mounts it for a PVC;
- no resize, reshape, RAID migration, scrub/monitoring or SMART alerts yet;
- no automatic NFS client setup or split DNS;
- StorageClass removal keeps data; there is no purge command;
- the directory mode assumes the directory is already on a mounted filesystem
  and is intentionally not responsible for that mount.
