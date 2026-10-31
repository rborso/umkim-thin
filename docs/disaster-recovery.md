# Disaster recovery

Rebuild a host to its previous state from three artifacts. They are
complementary: none of them is a complete backup on its own.

| Artifact | Covers | Where it lives |
|---|---|---|
| **Platform backup** (`umkim-thin-<ts>-platform.tar.gz`) | every Kubernetes object, the console state and secrets, the platform volumes (LLDAP, Authelia, Traefik ACME) | the backup target (NFS), not the host |
| **Data backup** (`umkim-thin-<ts>.tar.gz`) | the console state, the app inventory and per-app values, and the app volumes (CSI snapshots or file copies) | the backup target |
| **Host backup** (`install.sh --backup`) | the etcd datastore and the host `/etc/umkim-thin` (storage agent config, installer values) | wherever you write it (`--out`) — keep it off the host |

Take all three before a platform upgrade. A data backup with **Data mode =
`copy`** (or `auto`) is what makes app volumes restorable on NFS/local-path;
with `snapshot` and no CSI driver it contains no volume data.

## Before you start

- Reinstall the same release with the **same zone** and the same flags:
  `sudo ./install.sh --zone <zone> --email <email> --backup-nfs <server:/path>`
  (add `--trusted-access` if you used it).
- The backup target must be reachable; the installer mounts it at
  `/backups/nfs`, and the console lists it under **Backups → Target**.
- Keep the archives on a different machine or disk than the host.

## Rebuild

1. **Clean install** the host. Do not restore anything yet.
2. **Select the target and verify** the archives:
   ```bash
   umkim-thin backup targets
   umkim-thin backup schedule --target /backups/nfs
   umkim-thin backup list
   umkim-thin backup verify <platform-archive>
   umkim-thin backup verify <data-archive>
   ```
3. **Platform restore** — objects, console state and platform volumes:
   ```bash
   umkim-thin backup restore <platform-archive> --data --platform --confirm
   kubectl -n umkim-thin rollout restart deploy/umkim-thin-console
   ```
   `--data` extracts the LLDAP, Authelia and Traefik copies into their PVCs
   (the workloads that mount them are scaled to zero and back); `--platform`
   applies `platform/k8s-objects.yaml` with server-side apply.
4. **Recreate chart-managed Services.** A platform restore strips allocated
   Service IPs (and keeps headless ones). An app whose chart pins a ClusterIP —
   the WireGuard gateway's `vpn-dns` at `10.43.200.53` — gets a random one, so
   VPN DNS breaks. Reinstall that app and restart its workload:

   ```bash
   umkim-thin app install wireguard
   kubectl -n wireguard rollout restart daemonset/wireguard-gateway
   ```

5. **Data restore** — console state, app volumes and missing apps:
   ```bash
   umkim-thin backup restore <data-archive> --data --apps --confirm
   ```
   Each app whose volume is restored is stopped, the copy is extracted into its
   PVC, and the app is started again. Finished bootstrap Jobs do not block a
   restore (their pods are skipped); a *running* Job, a DaemonSet or a bare pod
   does. To retry just one volume without re-copying the rest:
   ```bash
   umkim-thin backup restore <data-archive> --data --confirm --volume <ns>/<pvc>
   ```
6. **Verify**: sign in to the portal and console, check the app list, the
   directory users, the WireGuard clients and a few app logins.
7. **Host backup**: restore `/etc/umkim-thin` (storage agent config, installer
   values) and, if you need the exact cluster datastore, the etcd snapshot.

### App checks after a restore

Pods being Ready is not the same as the apps working. After a restore, check at
least:

- **Portal login** with a directory account (Authelia → LLDAP):
  `curl -k -X POST https://auth.<zone>/api/firstfactor -d '{"username":"…","password":"…"}'`.
- **Each app's own API** through its Service: Jellyfin `/health`, Prowlarr /
  Radarr / Sonarr `/ping`, Seerr `/api/v1/status`, Immich `/api/server/ping`,
  SparkyFitness `/`.
- **qBittorrent's WebUI password** may be the user's own (not the bootstrap
  Secret, which is stale after a manual change); log in with the real one.
- **App databases**: e.g. `psql` in the postgres pod and count a table. An app
  whose chart kept its database on an ephemeral volume starts with an empty DB;
  its own dump in the data volume is the recovery source.
- **The tunnel end to end**: create a client, handshake, split DNS (an app host
  resolves to the gateway), and an app URL over the VPN.

## What is restored where

| Item | Platform restore | Data restore | Host backup |
|---|---|---|---|
| Kubernetes objects | `--platform` | — | etcd snapshot |
| Console state, secrets, per-app values | `state/` | `state/` | console PVC archive |
| App volumes | — | `--data` | — |
| LLDAP/Authelia/Traefik volumes | `--data` | — | — |
| Host seed (`storage.yml`, installer values) | — | — | `/etc/umkim-thin` |
| etcd datastore | — | — | etcd snapshot |

## Caveats

- **NFS subdirectories.** The NFS provisioner names a directory
  `<namespace>-<pvc>-<pv-uid>`. A fresh PVC gets a new UID, so it does **not**
  reuse the old directory. The data backup's file copies are the reliable path;
  alternatively recreate the PV with the old path.
- **Local-path volumes** live on the host disk and are gone after a wipe: only
  the file copies bring them back. Use Data mode `copy`/`auto` for those.
- **CSI snapshots** (HA/Longhorn): create the PVC from the snapshot the console
  lists instead of using `--data`; snapshots are not portable across clusters
  unless the storage class/backend matches.
- **A platform restore is powerful**: it applies cluster objects and writes
  volumes. It always writes a safety archive of the console state first, and it
  requires `--confirm` (CLI) or the confirmation body (API/UI).
- **The object export is API-level**, not etcd. The host `install.sh --backup`
  is the byte-exact cluster state.
- **`config.env` is not restored** by design; re-run the installer with the same
  flags (the zone, backup target and trusted-access choice are reused from it on
  the host backup).
- **Immich's Postgres PGDATA.** The immich Postgres image (Postgres 14 based)
  declares `VOLUME /var/lib/postgresql/data`. Mounting the PVC at the parent let
  the runtime create an anonymous volume there, which shadowed the PVC, so the
  database vanished with the pod. Chart 0.1.10 sets
  `PGDATA=/var/lib/postgresql/pgdata`; a database created by an older chart is
  empty and has to come from the data backup.

## Lessons from a real drill

A full wipe-and-rebuild of a live host (apps, media, identity, WireGuard)
surfaced these sharp edges; the code and the steps above reflect them:

- **The console state is the authoritative secret store.** `/data/secrets.yml`
  on the console PVC can be newer than the host seed in
  `/etc/umkim-thin/secrets.yml` (a password changed in the directory, a
  re-render). After a platform restore, sync the seed from the console
  **before** re-running the installer or upgrading identity:

  ```bash
  for f in secrets.yml identity-values.yml identity-apps.yml; do
    kubectl -n umkim-thin exec deploy/umkim-thin-console -c console -- cat /data/$f \
      | sudo tee /etc/umkim-thin/$f >/dev/null
    sudo chmod 600 /etc/umkim-thin/$f
  done
  ```

  Otherwise the fresh installer regenerates identity Secrets that no longer
  match the restored LLDAP/Authelia databases.
- **Secrets do not roll pods.** After restoring the identity Secrets, restart
  the identity deployments so the containers pick up the new env values
  (`kubectl -n identity rollout restart deploy/lldap deploy/authelia deploy/valkey`).
- **The console's own Deployment and RBAC are not applied** by a platform
  restore: the installer owns them. Restoring an older ClusterRole would
  downgrade the console mid-restore.
- **Never replay cluster identity.** `kube-system/k3s-serving` (the API
  server's serving certificate) and the derived `kube-root-ca.crt` ConfigMaps
  belong to the cluster being restored into. Applying the old Secret makes k3s
  serve the previous cluster's certificate, and every client fails with
  `x509: certificate signed by unknown authority`. The export now drops both; if
  an older archive is used, delete the Secret and
  `/var/lib/rancher/k3s/server/tls/dynamic-cert.json`, then restart k3s.
- **Static PersistentVolumes are restored; dynamic ones are not.** The NFS
  provisioner's root PV is static and must exist, or its PVC stays Pending and
  every NFS claim goes Lost.
- **Stale NFS mounts** can leave the console pod stuck in `Init`; restart k3s
  (or unmount the stale kubelet path) to recover.
- A data restore verifies the archive metadata and trusts the tar/gzip CRC for
  the copies, so it does not read 8 GiB twice.

## Drill

Run this at least once per release on a lab host: install, take both backups,
wipe, rebuild. A backup you have never restored is a hypothesis.
