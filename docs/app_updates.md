# Upstream version audit — 2026-11-16

Read-only audit of the catalog, the `charts/apps` submodule and the live HA dev
cluster, followed by the applied upgrades. Sources: GitHub Releases API, the
Docker Hub / GHCR tag APIs, the catalog entries, `charts/apps/*/Chart.yaml` and
`values.yaml`, and `helm list` / `kubectl` on ha-dev.

## Applied in v0.3.8

| App | Chart | Was | Now |
|---|---|---|---|
| Gitea | **0.1.22** | 28.3.0 | **28.4.0** |
| Immich | **0.1.12** | 3.3.5 | **3.4.1** |
| Mealie | **0.1.6** | 3.30.0 | **3.31.0** (plus the Recreate fix) |
| Nextcloud | **0.1.21** | 35.0.2 | **35.0.3** |
| Paperless-ngx | **0.1.14** | 3.4.1 | **3.4.2** |
| Servarr | **0.3.4** | see below | Radarr **7.0.3**, Sonarr 4.0.20 (ls5), Jellyfin 12.4 (ls59), Seerr **3.5.2** |
| SparkyFitness | **0.1.3** | 1.11.0 | **1.12.1** (plus the Recreate fix) |
| Identity | 0.1.12 | — | external-proxy support (unchanged apps) |

Already current, so untouched: Actual Budget 26.11.0 (0.1.2), HomeBox 0.27.1
(0.1.1), Vaultwarden 1.37.5 (0.1.2), WireGuard gateway 0.1.8. Prowlarr and
qBittorrent stayed on 2.7.0.5723 and 5.3.0; those are the newest stable tags.

Deliberately not taken: **Sonarr 5.0.0** (only pre-releases so far) and
**Prowlarr 3.0.x** (nightly/develop only). Both stay on the stable line.

## Fixed along the way: RWO workloads must recreate

`mealie` (both Deployments) and `sparkyfitness` postgres mounted a
ReadWriteOnce claim without `strategy: Recreate`. The first upgrade attempt
scheduled the new pod before the old one released the volume, so it hung in
`FailedAttachVolume` and the `--atomic` upgrade hit its deadline and rolled
back. Every other app chart already set it; these three now do too, and
`ci/static.sh` fails if a Deployment that mounts a claim loses it.

## ha-dev: pre-existing storage fault (found, not caused)

The Immich restart during the upgrade exposed a fault that had been latent
since **2026-11-09T00:56Z**:

- `k3s.service` on thin-ha-0 exited (`status=1/FAILURE`) under a 17–25 s etcd
  stall and was restarted by systemd 11 seconds later.
- The restart tore down the iSCSI sessions that back the Longhorn engines
  (`iscsid: Connection … is shutdown` on all three nodes), so the engine for
  each volume died mid-write and Longhorn marked **every replica** failed.
- Four volumes went `faulted`: `mealie-data`, `sonarr-config`, `radarr-config`,
  `immich-postgres`. With `auto-salvage=false` (deliberate lab safety) they stay
  faulted until an operator salvages them, so the apps kept running on the last
  attached state while their config directories returned `I/O error`.
- No data was lost: all three replicas of all four volumes still held their
  files on disk (91–620 MiB each).

Repair: the four volumes were salvaged through Longhorn's own API action
(`POST /v1/volumes/<name>?action=salvage` with the replica names — the
`salvageRequested` spec field does not exist in 1.6). All four are
`attached healthy` again, `sonarr`/`radarr` read `/config` and return 200 on
`/ping`, `immich` returns 200, and `mealie` reads its data directory.

Note for the lab: the 2026-11-02 data-copy backups on the NFS backup target
covered all four volumes and were intact, so recovery-from-backup was available
but unnecessary. The backup taken *after* the fault (2026-11-16) reports 23
snapshots for those volumes but cannot restore them, because Longhorn cannot
snapshot a faulted volume — worth remembering when reading `backup list`.

## Still open

- `auto-salvage` stays `false`. Salvaging by hand is the documented safety
  trade-off (a wrong replica must not become the truth automatically), but it
  means a k3s restart can leave volumes faulted until someone notices. A lab
  watchdog that alerts on `robustness != healthy` would catch this in minutes
  rather than days.
