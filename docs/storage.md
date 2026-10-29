# Storage: local host disks and external NFS

The console has two storage paths:

- **Local host** (single node only): disks, RAID and host NFS exports managed by
  `umkim-thin-storage`.
- **External NFS** (single and HA): connect an existing NFS server/export and
  create a StorageClass for it.

What a fresh install starts with:

| Path | Config class | Media class | NFS |
|---|---|---|---|
| `install.sh` single node | `local-path` | `local-path` | Host storage agent; add a local or external export from **Storage** |
| Ansible single node | `local-path` | `umkim-thin-nfs` | `nfs_mode=host` exports from the node by default |
| Ansible HA | `longhorn-rwo` | `longhorn-rwx` | Optional; add an external export at install time or later |

HA does **not** use the host-disk layer. It uses Longhorn for RWO/RWX volumes
(3 replicas by default; set `longhorn_replica_count=2` for a small lab) and can
add external NFS for media before or after the platform install.

For the server-side setup — exports, firewall, client CIDRs and verification —
see [Use an NFS server on another host](quickstart/12-external-nfs.md).

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

External NFS asks for these settings:

| Setting | Default | Rules |
|---|---|---|
| **Storage name** | — | 2–31 lowercase letters, digits or hyphens. Names must be unique across local and external exports because both use the `nfs-<name>` Helm release. |
| **NFS server** | — | Hostname or IP address. The console probes it on TCP/2049. |
| **Export path** | — | Absolute path, not `/`, no `..`, spaces or commas. |
| **StorageClass** | `nfs-<name>` | 2–31 characters; lowercase letters, digits, hyphens and dots. |
| **Mount options** | `nfsvers=4.2,rw,nolock` | Comma-separated. Supported: `rw`, `ro`, `sync`, `async`, `hard`, `soft`, `lock`, `nolock`, `noatime`, `relatime`, and `nfsvers=`/`vers=`/`port=`/`mountport=`/`timeo=`/`retrans=`/`rsize=`/`wsize=`/`actimeo=` with numeric values. |

**Plan** probes TCP/2049 from the console and uses `showmount -e` when
available to confirm the export is advertised; `showmount` failures are
warnings because NFSv4 pseudo-roots do not always enumerate. **Apply** repeats
the check, installs `nfs-subdir-external-provisioner` as release `nfs-<name>`
in the `nfs-provisioner` namespace, and waits for a `ReadWriteMany`, `Retain`
StorageClass (up to five minutes). There is no `FORMAT` step and no host
export. The console records the export on the PVC at
`/data/storage-external.yml`.

The server-side export, firewall and client-CIDR setup is in
[Use an NFS server on another host](quickstart/12-external-nfs.md).

When installing an app, choose the new class as **media** and set **Media
access mode** to `ReadWriteMany`. The install form derives the mode from the
class's advertised access modes, so an RWX NFS class switches it automatically;
the value is sent with the install request and recorded in the release values.

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

External NFS defaults: `--storage-class nfs-<name>` and
`--mount-options nfsvers=4.2,rw,nolock`. `--name` and `--storage-class` use the
same 2–31 character rule, and `--export` must be an absolute path other than
`/` without `..`, spaces or commas. `plan` probes TCP/2049 and `apply` installs
the provisioner; neither needs `--confirm FORMAT`.

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
- External NFS is not formatted, mounted or managed by the platform. Its
  exports, firewall, ownership and backups remain the NFS server's
  responsibility; restrict clients and test the export from a cluster node.

## StorageClasses and app installation

Class names depend on the path:

| Path | Class |
|---|---|
| `install.sh` single node | `local-path` for config and media until you add a host or external export |
| Ansible single node | `local-path` config + `umkim-thin-nfs` media |
| Ansible HA | `longhorn-rwo` config + `longhorn-rwx` media |
| Console **External NFS** | `nfs-<name>` (or the name you choose) |

The shared NFS class created by `provision.sh` is named `umkim-thin-nfs`; the
older name was the generic `nfs`. Environments written before the rename that
still set `storage_class_media=nfs` keep that name (the provisioner pins
`nfs_storage_class=nfs` for them). To adopt the new name, set both
`storage_class_media=umkim-thin-nfs` and `nfs_storage_class=umkim-thin-nfs`.
StorageClass names are immutable, so renaming a class on a live cluster means
recreating the PVCs (or keeping the old name in the environment); the installer
does not migrate bound claims.

For every local host export the agent installs a provisioner release named
`nfs-<name>` and a StorageClass named by `--storage-class` (default
`nfs-<name>`). External NFS uses the same release/class convention but is
installed by the console (or the CLI). The class appears in the console's
storage-class picker; use it for the **media** class of an app when you want
that export.

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

## Per-PVC overrides and catalog defaults

The console's install panel has an **Advanced** section that renders the chart
once and lists every PVC. Each claim can get its own storage class and size.
The values are applied by a Helm post-renderer after the chart is rendered, so
the platform does not need a chart-specific list of values paths. This is
especially useful for moving a user-data volume to NFS while keeping the
database on Longhorn, or for sizing one oversized claim independently.

The same panel's **Validate** button checks the plan against the live cluster
and the install repeats that preflight server-side. For Longhorn it compares the
new nominal claim sizes, multiplied by the configured replica count, with the
remaining nominal capacity reported by the Longhorn node disks. NFS capacity is
reported as a warning because Kubernetes cannot inspect the external export;
local-path classes are also warning-only. Invalid sizes, missing classes and
impossible Longhorn capacity are blocking errors.

Catalog install defaults are intentionally small for metadata volumes because
Longhorn counts the full request per replica. The current defaults include
`2Gi` for the app PostgreSQL volumes, `4Gi` for Immich PostgreSQL, and smaller
requests for Mealie/SparkyFitness/Nextcloud/Servarr configuration volumes.
These are install-time defaults: existing PVCs keep their original size and
class. Longhorn cannot change an existing claim's class, and shrinking a bound
PVC is not supported; use the advanced form on a reinstall/migration when a
larger legacy request must be reduced.

Do not use over-provisioning as a substitute for a real disk. It changes the
scheduler's nominal ceiling, not the bytes available to Longhorn; keep the
default 100% on shared root disks and add a dedicated disk or NFS storage
instead.

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
