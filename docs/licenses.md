# Licenses and third-party notices

umkim-thin itself is licensed under the **Apache License 2.0**. See
[`../LICENSE`](../LICENSE) and [`../NOTICE`](../NOTICE).

The platform installs and runs third-party software. This page records the
license of each project in the official catalog and of the platform components.
It is an attribution index, not a replacement for the upstream license text.

## Repository and charts

| Component | License | Notes |
|---|---|---|
| umkim-thin (source, console, API, installer) | Apache-2.0 | See `LICENSE`. |
| `charts/identity` | Apache-2.0 | Chart source; upstream LLDAP/Authelia/nginx keep their licenses. |
| `charts/umkim-thin` | Apache-2.0 | Chart source. |
| `charts/wireguard-gateway` | Apache-2.0 | Chart source; WireGuard keeps its license. |
| `charts/apps/*` | Apache-2.0 | The author's own UMKIM-tailored charts. No upstream app source is vendored. |
| `images/servarr-bootstrap` | Apache-2.0 | Vendored from the author's `servarr-helm` project; see its `SOURCE.md`. |
| Playwright test suites | Apache-2.0 | `@playwright/test` and its browser bundle. |

Packaged chart archives include a copy of `LICENSE` and `NOTICE`. The chart
metadata also declares `artifacthub.io/license: Apache-2.0`.

## Platform components

These are deployed as separate images, not linked into or redistributed by this
repository.

| Component | Upstream license | Source |
|---|---|---|
| LLDAP | GPL-3.0 | https://github.com/lldap/lldap |
| Authelia | Apache-2.0 | https://github.com/authelia/authelia |
| Traefik | MIT | https://github.com/traefik/traefik |
| Valkey | BSD-3-Clause | https://github.com/valkey-io/valkey |
| nginx | BSD-2-Clause | https://nginx.org/ |
| k3s | Apache-2.0 | https://github.com/k3s-io/k3s |
| Kubernetes | Apache-2.0 | https://github.com/kubernetes/kubernetes |
| Helm | Apache-2.0 | https://github.com/helm/helm |
| Calico | Apache-2.0 | https://github.com/projectcalico/calico |
| Longhorn | Apache-2.0 | https://github.com/longhorn/longhorn |
| nfs-subdir-external-provisioner | Apache-2.0 | https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner |
| WireGuard | GPL-2.0 | https://www.wireguard.com/ |
| Playwright | Apache-2.0 | https://github.com/microsoft/playwright |

The console image is based on Alpine Linux and includes Alpine packages
(`bash`, `curl`, `ca-certificates`, `tzdata`, `nfs-utils`) plus Helm and kubectl.
Those
packages retain their own licenses; the image keeps the project's `LICENSE` and
`NOTICE` under `/usr/share/licenses/umkim-thin/`. Image metadata declares
`org.opencontainers.image.licenses=Apache-2.0`.

## Official catalog applications

The catalog deploys upstream images. The chart is Apache-2.0; the application
inside the image keeps its own license.

### Gitea

| Component | License | Source |
|---|---|---|
| Gitea | MIT | https://github.com/go-gitea/gitea |
| PostgreSQL | PostgreSQL License | https://www.postgresql.org/about/licence/ |
| Valkey | BSD-3-Clause | https://github.com/valkey-io/valkey |

### Nextcloud

| Component | License | Source |
|---|---|---|
| Nextcloud | AGPL-3.0 | https://github.com/nextcloud/server |
| Collabora Online (CODE) | MPL-2.0 | https://github.com/CollaboraOnline/online |
| PostgreSQL | PostgreSQL License | https://www.postgresql.org/about/licence/ |
| Valkey | BSD-3-Clause | https://github.com/valkey-io/valkey |

### Immich

| Component | License | Source |
|---|---|---|
| Immich | AGPL-3.0 | https://github.com/immich-app/immich |
| VectorChord (in the Immich Postgres image) | AGPL-3.0 or Elastic-2.0 (dual) | https://github.com/tensorchord/VectorChord |
| PostgreSQL | PostgreSQL License | https://www.postgresql.org/about/licence/ |
| Valkey | BSD-3-Clause | https://github.com/valkey-io/valkey |

### Paperless-ngx

| Component | License | Source |
|---|---|---|
| Paperless-ngx | GPL-3.0 | https://github.com/paperless-ngx/paperless-ngx |
| PostgreSQL | PostgreSQL License | https://www.postgresql.org/about/licence/ |
| Valkey | BSD-3-Clause | https://github.com/valkey-io/valkey |

### Mealie

| Component | License | Source |
|---|---|---|
| Mealie | AGPL-3.0 | https://github.com/mealie-recipes/mealie |
| PostgreSQL | PostgreSQL License | https://www.postgresql.org/about/licence/ |

### Servarr stack

| Component | License | Source |
|---|---|---|
| Seerr | MIT | https://github.com/seerr-team/seerr |
| Jellyfin | GPL-2.0 | https://github.com/jellyfin/jellyfin |
| Sonarr | GPL-3.0 | https://github.com/Sonarr/Sonarr |
| Radarr | GPL-3.0 | https://github.com/Radarr/Radarr |
| Prowlarr | GPL-3.0 | https://github.com/Prowlarr/Prowlarr |
| qBittorrent | GPL-2.0-or-later source; GPL-3.0-or-later binary with OpenSSL exception | https://github.com/qbittorrent/qBittorrent |
| FlareSolverr | MIT | https://github.com/FlareSolverr/FlareSolverr |
| Jellyfin LDAP authentication plugin | MIT | https://github.com/jellyfin/jellyfin-plugin-ldapauth |
| LinuxServer.io image packaging | GPL-3.0 | https://github.com/linuxserver/docker-jellyfin (and the sibling images) |

The Servarr chart uses LinuxServer.io images for Sonarr, Radarr, Prowlarr,
qBittorrent and Jellyfin. The image packaging is GPL-3.0; the application
licenses above still apply.

## Custom catalog entries

A custom catalog is the operator's responsibility. If you add an app:

- record the upstream project and license in the entry (the proposed
  `upstream` field is described in [`catalog-sources.md`](catalog-sources.md));
- keep the upstream license text with any chart or image you redistribute;
- do not present a custom or community entry as an official umkim-thin entry.

The repository does not relicense an upstream application by wrapping it in a
chart. A chart's Apache-2.0 license covers the chart templates, not the image it
deploys.
