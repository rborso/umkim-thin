# 6. Back up (and restore)

**Goal:** know what is backed up, what is not, and how to get back.

Backups are the difference between a bad afternoon and a lost photo library.
Do this before you put anything important on the platform.

## What the installer backs up

```bash
sudo ./install.sh --backup
# or choose a directory:
sudo ./install.sh --backup --out /var/backups/umkim-thin
```

It writes:

- an **etcd snapshot** (`k3s etcd-snapshot save`), which contains the Kubernetes
  objects: namespaces, Deployments, Services, Ingresses, Secrets, PVC
  definitions, and the identity configuration stored in ConfigMaps/Secrets;
- `umkim-thin-state-<timestamp>.tgz`, an archive of the host seed in
  `/etc/umkim-thin`: secrets, identity values, and the OIDC keys;
- `console-data-<timestamp>.tgz`, streamed from the console pod's `/data` PVC:
  `identity-apps.yml`, `invites.json`, `apps-state.json`, `wg-clients.json`, and
  the runtime `secrets.yml` (the media-stack settings live here).

Everything is written with mode 0600 in a 0700 directory.

## What it does **not** back up

**Application data in PersistentVolumeClaims.** Nextcloud files, Immich photos,
Paperless documents, Gitea repositories and the media stack live in PVCs. The
single-node path uses `local-path`, which has no snapshot support.

For those, use one of:

- **A VM snapshot** (the simplest reliable method on a single server). Stop the
  stack or quiesce the apps, snapshot the VM, then test a restore.
- **Your storage's volume snapshots** if you replace `local-path` with a CSI
  driver that supports them (Longhorn, NFS with snapshots, cloud volumes).
- **Application-native export/backup** for the apps that provide one.

A backup you have never restored is a hypothesis. Test it once.

## Restore outline (advanced)

The etcd snapshot restores the cluster objects; the state archive restores the
secrets; the PVC data comes from the VM or volume snapshot.

1. Stop k3s on the node.
2. Restore the VM (or the PVCs) from your infrastructure snapshot.
3. Restore etcd from the snapshot:

   ```bash
   sudo k3s server --cluster-reset \
     --cluster-reset-restore-path=/var/lib/rancher/k3s/server/db/snapshots/<snapshot>
   ```

4. Start k3s and wait for the API.
5. Restore `/etc/umkim-thin` from the state archive and the console's `/data`
   PVC from `console-data-*.tgz`, preserving ownership by UID 65532 and mode
   0600.
6. Run `sudo ./install.sh --doctor` and check the portal, console, and one app.

This is a deliberate, manual procedure: an automated restore that guesses wrong
is worse than a documented one. If you need point-and-click restore, use a
storage layer with volume snapshots and a Kubernetes backup tool (Velero or
Longhorn), and test it.

For the full procedure, including the datastore restore and the verification
list, see [`../restore-drill.md`](../restore-drill.md). Run it once before you
trust the backup.

## A simple routine

- **Daily/weekly:** `sudo ./install.sh --backup` to a directory that is copied
  off the host (or to object storage).
- **Before every upgrade:** a backup plus a VM snapshot.
- **Monthly:** restore one app's data into a scratch namespace and open it.
- **Keep** at least one copy offline; a compromised host should not be able to
  delete its own backups.

## Next

[Upgrade safely →](07-upgrade.md)
