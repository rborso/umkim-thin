# Restore drill

A backup you have never restored is a hypothesis. This is the procedure to turn
it into a fact. Run it on a disposable VM (or the lab target after a base-snapshot
reset), record the wall-clock time, and keep the result in the issue/release
notes.

Two different things have to come back:

| Piece | Where it lives | How it is restored |
|---|---|---|
| Cluster objects (deployments, routes, secrets, PVCs) | k3s datastore / etcd snapshot | k3s `--cluster-reset-restore-path` |
| Console state (invites, app state, WireGuard peers) | console PVC + `/etc/umkim-thin` seed | PVC snapshot + the state archive |
| App data (photos, documents, media) | app PVCs | storage snapshots or a file copy |

`install.sh --backup` writes the etcd snapshot, the host seed and the console
PVC archive. It deliberately **does not** copy app PVCs; snapshot them with the
storage (Longhorn snapshot, NFS copy, or a VM snapshot).

## Before the drill

- [ ] Take a backup: `sudo ./install.sh --backup` (or the HA equivalent).
- [ ] Copy the backup **off the host**: `scp`/`rsync /var/backups/umkim-thin`
      to the runner or another machine. A backup on the failing disk is not a
      backup.
- [ ] Snapshot the app PVCs (or the VM) at the same time as the backup.
- [ ] Write down the release tag and the zone. The restore must use the same
      release, or upgrade *after* the restore succeeds.

## Console state without a full restore

The console archives its own state daily (`docs/backup.md`). When the cluster is
healthy and only the console state is wrong — a lost invite store, a bad app
value, a deleted OIDC client — restore the newest archive instead of the whole
host:

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin backup list
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin backup restore umkim-thin-<stamp>.tar.gz
```

It writes a safety archive first, so a wrong choice is recoverable. This does
not touch application PVCs; the archive's snapshots are the data step.

## Restore onto a fresh host

1. **Fresh install, same zone, same release.** Follow
   [`quickstart/01-install.md`](quickstart/01-install.md) from the exact release
   checkout so the console image is built locally, and stop before inviting
   anyone:

   ```bash
   sudo ./install.sh --zone example.com --email you@example.com
   ```

   This creates a new datastore and new secrets. Do **not** keep the new
   `/etc/umkim-thin/secrets.yml`; the backup replaces it.

2. **Stop k3s and restore the datastore.**

   ```bash
   sudo systemctl stop k3s
   sudo /usr/local/bin/k3s server --cluster-reset \
     --cluster-reset-restore-path=/var/lib/rancher/k3s/server/db/snapshots/<snapshot>
   sudo systemctl start k3s
   sudo kubectl get nodes,pods -A
   ```

   On an HA cluster, run the reset on one server with the restored snapshot;
   the other servers rejoin. Follow the k3s HA restore notes.

3. **Restore the host seed and console state.**

   ```bash
   sudo tar xzf umkim-thin-state-<stamp>.tgz -C /
   sudo tar xzf console-data-<stamp>.tgz -C /tmp/console-data
   sudo kubectl -n umkim-thin cp /tmp/console-data/. deploy/umkim-thin-console:/data
   sudo kubectl -n umkim-thin rollout restart deploy/umkim-thin-console
   ```

   The state archive restores `/etc/umkim-thin` (secrets, identity values,
   config). Keep it 0600.

4. **Restore app data.** Recreate each app's PVC from its storage snapshot, or
   copy the files back:

   ```bash
   # Longhorn: create a volume from the snapshot and bind the PVC, or
   # NFS: rsync the app's media directory back to the export path.
   sudo kubectl -n immich get pvc
   ```

   Databases are inside the app PVCs for apps that use SQLite; Postgres
   apps have their own PVC. Restore both.

5. **Verify, in this order.**
   - [ ] `sudo ./install.sh --doctor` is green (allowing the expected
         certificate/DNS warnings of the drill host).
   - [ ] The portal and console logins work with the **backed-up** secrets.
   - [ ] One invited user still exists in the directory.
   - [ ] One app opens and its data is present (a photo, a document, a recipe).
   - [ ] The console shows the previously installed apps and their routes.
   - [ ] A new invite can be created and redeemed after the restore.

6. **Record the result.** Time from "fresh host" to "verified", what was
   missing, and the snapshot/storage details. If any step needed an undocumented
   command, fix the doc in the same PR.

## Upgrade drill (N-1 -> N)

The restore procedure covers node loss. Upgrades are the other half:

1. Install the previous release tag on a disposable host.
2. Invite a user and install one app with real data.
3. Upgrade: `sudo ./install.sh --zone ... --email ... --image ...:<new-tag>`
   (the script keeps the existing secrets and chart mirror).
4. Verify the user, the app and the data survive; run a console app upgrade
   (Applications -> Install/Upgrade) and check the Job.
5. If Helm is left `pending`, follow
   [`quickstart/07-upgrade.md`](quickstart/07-upgrade.md) and roll back by
   installing the previous image tag; the app data is untouched by the console
   chart, but an app migration may not be reversible — restore from the
   pre-upgrade backup if it is not.

Do not call a release "upgrade-safe" until both drills have run on it (single
and HA).
