# 12. Use an NFS server on another host

**Goal:** create an NFS export on a separate Linux host or NAS, then connect it
to umkim-thin as a shared ReadWriteMany StorageClass.

Use this when app media (Nextcloud files, Immich photos, Paperless documents,
Servarr downloads) should live outside the cluster, or when an HA cluster needs
shared media that is not Longhorn. The console does **not** format, mount or
manage the remote export: the NFS server remains the source of truth.

> The single-node `install.sh` path uses `local-path` for config and media by
> default. The Ansible single-node path uses `local-path` for config and
> `umkim-thin-nfs` for media. Either path can add an external NFS class later;
> existing PVCs are not rebound.

## What the cluster needs

- The NFS server reachable from **every cluster node** on TCP/2049.
- An export path that the cluster can read and write.
- Client access for the node network, the **pod CIDR**, and the WireGuard
  subnet if you use the gateway.
- NFSv4.2 if possible. The console's default mount options are
  `nfsvers=4.2,rw,nolock`; a server that only supports 4.1 or 4.0 works with
  different mount options.

Find the CIDRs on the cluster:

```bash
# Pod CIDR (the NFS client is a pod, not the node)
sudo kubectl get nodes -o jsonpath='{range .items[*]}{.spec.podCIDR}{"\n"}{end}'

# Node/management subnet
ip -4 addr show scope global | awk '/inet /{print $2}'

# WireGuard subnet (single 192.168.168.0/24, HA 192.168.169.0/24 by default)
sudo grep WG_SUBNET /etc/umkim-thin/config.env
```

Typical values:

| Profile | Pod CIDR | WireGuard subnet |
|---|---|---|
| `install.sh` single node | `10.42.0.0/16` | `192.168.168.0/24` |
| Ansible single node | `10.244.0.0/16` | `192.168.168.0/24` |
| Ansible HA | `10.244.0.0/16` | `192.168.169.0/24` |

The Ansible path uses Calico with native routing, so the NFS server sees the
**pod** source address. The on-host `install.sh` path uses k3s's default
flannel, which masquerades pod egress to the node address. Allow both the node
subnet and the pod CIDR; that covers both paths and future changes.

## Create the export (Rocky/RHEL)

On the NFS host:

```bash
sudo dnf install -y nfs-utils
sudo mkdir -p /srv/nfs/umkim-thin /etc/exports.d

# Lab: let the cluster's app UIDs write. For production, set ownership to the
# app's UID/GID instead of making the directory world-writable.
sudo chmod 0777 /srv/nfs/umkim-thin

sudo tee /etc/exports.d/umkim-thin.exports >/dev/null <<'EOF'
/srv/nfs/umkim-thin 192.168.1.0/24(rw,sync,no_subtree_check,no_root_squash)
/srv/nfs/umkim-thin 10.244.0.0/16(rw,sync,no_subtree_check,no_root_squash)
/srv/nfs/umkim-thin 192.168.168.0/24(rw,sync,no_subtree_check,no_root_squash)
EOF

sudo exportfs -rav
sudo systemctl enable --now nfs-server
sudo firewall-cmd --permanent --add-service=nfs \
  --add-service=rpc-bind --add-service=mountd
sudo firewall-cmd --reload
```

Replace the three CIDRs with your node network, pod CIDR and VPN subnet. NFSv4
only needs TCP/2049; `rpc-bind` and `mountd` are needed for NFSv3 and for
`showmount`.

### Debian/Ubuntu

```bash
sudo apt update && sudo apt install -y nfs-kernel-server
sudo mkdir -p /srv/nfs/umkim-thin
sudo chmod 0777 /srv/nfs/umkim-thin

sudo tee -a /etc/exports >/dev/null <<'EOF'
/srv/nfs/umkim-thin 192.168.1.0/24(rw,sync,no_subtree_check,no_root_squash)
/srv/nfs/umkim-thin 10.244.0.0/16(rw,sync,no_subtree_check,no_root_squash)
/srv/nfs/umkim-thin 192.168.168.0/24(rw,sync,no_subtree_check,no_root_squash)
EOF

sudo exportfs -ra
sudo systemctl enable --now nfs-server
sudo ufw allow from 192.168.1.0/24 to any port nfs
```

### NAS

Create a shared folder, enable NFS, and allow the node network, pod CIDR and VPN
subnet. Use the NAS's export path (for example `/volume1/media`) in the console;
do not assume `/srv/nfs/...`.

### NFSv4 pseudo-root (`fsid=0`)

If the server exports the share with `fsid=0`, that directory is the NFSv4
**pseudo-root**. Clients mount `server:/`, not `server:/mnt/.../share`, and
child exports are addressed **relative to the pseudo-root**:

```text
server:/bck-hdd          # child export /mnt/.../share/bck-hdd
```

The console requires an export path other than `/`, so create a dedicated child
directory/export and enter its client path (for example `/bck-hdd`). `showmount`
prints the server path, which may be `/mnt/.../share/bck-hdd`; use the relative
path in the console and verify it with a test mount. Mounting the full server
path returns `No such file or directory` (mount exit status 32).

If the server does not use `fsid=0`, use the export's full path instead.

## Verify from a cluster node

Run this on one of the cluster nodes, not on your laptop:

```bash
showmount -e 192.168.1.20
sudo mkdir -p /mnt/nfs-test
sudo mount -t nfs -o nfsvers=4.2 192.168.1.20:/srv/nfs/umkim-thin /mnt/nfs-test
sudo touch /mnt/nfs-test/.umkim-test
sudo rm /mnt/nfs-test/.umkim-test
sudo umount /mnt/nfs-test
```

If `showmount` or `mount.nfs` is missing, install `nfs-utils` on the node. A
mount that hangs is almost always a firewall or client-CIDR problem.

## Connect it in the console

1. Open **Storage → External NFS**.
2. Fill in the form:

| Field | Default | Notes |
|---|---|---|
| **Storage name** | — | 2–31 lowercase letters, digits or hyphens. Used for the Helm release `nfs-<name>`. |
| **NFS server** | — | Hostname or IPv4 address reachable from the console pod. |
| **Export path** | — | Absolute path, not `/`, no `..`, spaces or commas. |
| **StorageClass** | `nfs-<name>` | Same 2–31 character rule; dots are allowed. Must be unique. |
| **Mount options** | `nfsvers=4.2,rw,nolock` | Comma-separated. Use `nfsvers=4.1` or `nfsvers=4.0` if the server does not support 4.2. |

3. Click **Plan**. The console probes TCP/2049 from the console pod and runs
   `showmount -e` when available. `showmount` being unavailable is a warning,
   not a failure; an export that is advertised but does not contain the path is
   an error.
4. Click **Add external NFS**. It installs
   `nfs-subdir-external-provisioner` as release `nfs-<name>` in the
   `nfs-provisioner` namespace and creates a `ReadWriteMany`, `Retain`
   StorageClass. Apply can take up to five minutes.
5. Install new apps with that class as the **media** class in the Catalog form,
   and set **Media access mode** to `ReadWriteMany` (the form selects it
   automatically when the class advertises RWX). Keep config/database volumes
   on `local-path` (single) or `longhorn-rwo` (HA).

Existing PVCs keep their class. To switch an existing app, reinstall it with the
new media class or migrate the PVC; changing a bound PVC's StorageClass in place
is not supported.

### CLI alternative

On the single node, the same operation is available through the CLI:

```bash
sudo umkim-thin storage plan --name nas-media --kind external \
  --server 192.168.1.20 --export /volume1/media
sudo umkim-thin storage apply --name nas-media --kind external \
  --server 192.168.1.20 --export /volume1/media
```

The CLI external path targets the node's Helm/kubeconfig. The console path works
on both single and HA.

## Configure it at provision time

The Ansible path can create the class during provisioning instead. In
`deploy/provision-single.env` or `deploy/provision-ha.env`:

```env
nfs_mode=external
nfs_server=192.168.1.20
nfs_export=/srv/nfs/umkim-thin
storage_class_media=umkim-thin-nfs
```

`nfs_mode=host` (the single-node default) exports from the node itself. For HA,
leave external NFS out of the first install if the server is not ready; add it
from the console afterwards and select the new class for new app installs.

The class created by the Ansible `nfs` role is `umkim-thin-nfs` by default. The
class created by the console is `nfs-<name>`. They are different names; set
`storage_class_media` to whichever class the apps should use.

## Troubleshooting

| Symptom | Check |
|---|---|
| `NFS server … is not reachable on TCP/2049` | Firewall, routing, and that the server is listening (`ss -ltnp \| grep 2049`). |
| `does not advertise export …` | `sudo exportfs -v` on the server; the path must match exactly. |
| `showmount` warning | Not fatal; verify the export with a test mount from a node. |
| Mount hangs | Client CIDR missing the node or pod network (see above), or NFSv3 services blocked. |
| `No such file or directory` / mount exit 32 | The export is an NFSv4 pseudo-root (`fsid=0`): use the child export's path relative to the pseudo-root (for example `/bck-hdd`), not the server path. |
| PVC stays `Pending` | `sudo kubectl -n nfs-provisioner get pods,events`; check the class name. |
| `Permission denied` on writes | Export ownership and `no_root_squash`; the app's UID must be able to write. |
| NFSv4.2 unsupported | Set mount options to `nfsvers=4.1` or `nfsvers=4.0` and re-apply. |
| Class already exists | Use another name, or remove the old `nfs-<name>` release and class first. |

## Back up the NFS server

The cluster only holds PVC definitions; the files are on the NFS server. Back up
that host (snapshots, `rsync`, or the NAS's own backup) as part of the app data
backup. See [Back up](06-backup.md) for what the platform itself covers.

## Related

- [`../storage.md`](../storage.md) — local disks, RAID, class naming and safety.
- [`../console.md`](../console.md#external-nfs) — the console fields and API.
- [`../install.md`](../install.md) — storage settings in the provisioning
  environment.
