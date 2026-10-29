# Install umkim-thin

> **Beginner path:** one Rocky Linux server, one command, no Ansible or
> jumphost. Use [`install.sh`](../install.sh) and the
> [quickstart](quickstart/README.md). This guide is the advanced/HA path.

This is the operator guide: take a fresh RHEL-family host (or three for HA) to a
running install, sign in, invite a user, install an app, and (optionally) enable
WireGuard.

The supported path is the Ansible project in `deploy/ansible`, driven by
`deploy/provision.sh` from a workstation. If you have never run it before, read
the whole single-node section once before starting.

> **TL;DR**
>
> ```bash
> cp deploy/provision-single.env.example deploy/provision-single.env
> $EDITOR deploy/provision-single.env
> PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh
> ```
>
> For HA use `provision-ha.env` (three servers and a VIP). The rest of this
> guide explains the prerequisites, verification and day-2 operations.

---

## 1. Before you begin

### Hosts

| | Single node | HA |
|---|---|---|
| Servers | 1 | 3 (embedded etcd) |
| Suggested size | 6 vCPU, 12 GiB RAM, 80 GiB disk | 6 vCPU, 12 GiB RAM, 120 GiB disk each |
| Extra | — | a free VIP address on the server network |
| OS | Rocky Linux / RHEL 9 or 10 | same |

Every server needs:

- outbound internet for the OS packages, the k3s installer, Helm charts and app
  images;
- SSH as a sudo-capable user;
- a DNS name for the zone that points at the ingress.

The disk sizes are a starting point. The media stack and Nextcloud grow with
your data; the **shared media** volume is 50 GiB by default. On HA, Longhorn
holds RWO config/database volumes (3 replicas by default); large media can move
to external NFS once configured, and a dedicated Longhorn disk avoids competing
with the OS and containerd.

### DNS

Create these names (a wildcard is easiest):

```text
<zone>                 -> ingress address
*.<zone>               -> ingress address
```

That covers `auth.<zone>`, `console.<zone>`, `home.<zone>`, `lldap.<zone>`, `gitea.<zone>`,
`nextcloud.<zone>`, `immich.<zone>`, `paperless.<zone>`, `jellyfin.<zone>`,
`sonarr.<zone>`, and so on. The installer reads `<zone>` from the environment
file; it does not create DNS records.

### Ports

Forward or allow:

| Port | Protocol | Purpose |
|---|---|---|
| 80 | TCP | HTTP (ACME HTTP-01 and redirect) |
| 443 | TCP | HTTPS (portal, console, apps) |
| 2222 | TCP | Gitea SSH (`git clone`/`push`) |
| 51820 | UDP | WireGuard, only if you install the gateway |

On a NAT, map 80/443/2222 to the node's NodePorts **30080/30443/30222**
(the installer configures Traefik to listen on those). With a LoadBalancer or a
VIP, publish 80/443/2222/51820 directly and keep the NodePorts as the backend.

### Workstation

You need:

- `git`, `go` 1.24+ (Rocky 10's package is current; Rocky 9 may need an
  upstream toolchain), `helm` 3.16+, `podman` (or Docker, with a `podman`
  wrapper), `ssh` on the machine running `provision.sh` (not necessarily on
  the target nodes);
- a key that can reach the **Ansible executor** (`JUMP`), because
  `deploy/provision.sh` ships the playbook there;
- `ansible` on that executor (the playbook is run there, not on your laptop);
- access to the app charts: the catalog pins OCI references, or you can package
  them into the local chart mirror (see §4).

`JUMP` can be the first/bootstrap server **after the first-run reboot has
completed**. The `common` role may reboot every target to load a kernel with
`xt_*` modules; if `JUMP` is one of those targets, the reboot kills the Ansible
process. For the first run, use a separate management/jump host (or rerun after
the reboot). If the first server is used, set `JUMP_PORT=22` and make sure it
has `ansible` and passwordless SSH to all servers, including itself.

For copy-paste installation of the build tools, Go, Helm, Podman, Ansible and
NFS server packages, see the
[HA quickstart prerequisites](quickstart/11-ha-install.md#install-the-prerequisites).
Build tools are needed on the machine running `provision.sh`, not on every
target node.

### TLS: pick one

| Mode | Env | Result | Use for |
|---|---|---|---|
| **Lab CA fallback** | `lab_ca_default=true`, `acme_staging=true` | Traefik serves a wildcard signed by a local CA; ACME is attempted but the default cert covers the routes | air-gapped/lab, or when HTTP-01 cannot reach the cluster |
| **ACME staging** | `lab_ca_default=false`, `acme_staging=true` | real Let's Encrypt *staging* certs, untrusted by browsers | testing ACME end to end |
| **ACME production** | `lab_ca_default=false`, `acme_staging=false` | trusted certs | production |

The default in the examples is the lab CA. Import `<state_dir>/tls/ca.crt`
(default `/etc/umkim-thin/tls/ca.crt` on the primary) into your browser or OS
trust store, or use `curl --cacert`.

Never enable production ACME in lab or repeated test runs: the limit is 50
certificates per registered domain per 7 days.

---

## 2. Get the environment file

```bash
git clone --recurse-submodules https://github.com/rborso/umkim-thin.git
cd umkim-thin

# single node
cp deploy/provision-single.env.example deploy/provision-single.env
# or, for HA
cp deploy/provision-ha.env.example deploy/provision-ha.env

$EDITOR deploy/provision-single.env   # or provision-ha.env
```

The important fields:

| Field | Meaning |
|---|---|
| `JUMP`, `JUMP_PORT`, `VSSH_KEY` | how the script reaches the provisioner; the key is the one that logs into the jumphost |
| `VHOST`, `VUSER`, `servers` | the primary server and the full server list (primary first) |
| `zone`, `portal_host`, `console_host` | the public names; `auth.<zone>` and `console.<zone>` are conventional. The home page is `home.<zone>` by default; set chart `homeHost` for a custom name. |
| `base_dn`, `admin_email`, `acme_email` | directory DN and the addresses used for the admin/ACME account |
| `lab_cidr` | your node/management network (the firewall trusts it) |
| `public_ip` | optional **fixed** public/NAT address for the DNS preflight; leave blank for dynamic DNS (the current common public address is accepted) |
| `wg_endpoint`, `wg_subnet`, `wg_server_ip` | WireGuard's public endpoint and tunnel network |
| `storage_backend` | empty: local-path + NFS (single); `longhorn` for HA |
| `storage_class_media` | Media class. HA defaults to `longhorn-rwx` until NFS is configured; an NFS install uses `umkim-thin-nfs` (legacy `nfs`). |
| `nfs_storage_class` | Name of the provisioned NFS class; default `umkim-thin-nfs`. |
| `longhorn_replica_count` | Longhorn replicas per volume: `3` default, `2` optional for small labs. |
| `longhorn_storage_over_provisioning_percentage` | Longhorn's thin nominal-capacity ceiling; default `100` |
| `storage_agent` | `true` on single (host disks/RAID/NFS); `false` on HA |
| `storage_socket` | `/run/umkim-thin` on single; empty on HA |
| `chart_repos` | optional `url:subdir` or `repo:subdir` list for out-of-tree charts; vendored app charts are packaged automatically |
| `ci_apps` | only used by the maintainer e2e; harmless for a manual install |

Keep the file out of git: `deploy/provision*.env` is ignored.

Maintainers running the project lab should use
[`maintainer-lab.md`](maintainer-lab.md); that page is not needed for a normal
install.

---

## 3. Prepare the servers

### Fresh servers

- Install Rocky/RHEL 9/10, set the hostname, and make sure the `linux` (or
  `VUSER`) account can `sudo`.
- Do **not** pre-install Kubernetes, Docker, or a firewall rule set that
  conflicts with the playbook. The `common` role installs what it needs,
  disables swap, enables forwarding, and may reboot once into a kernel with
  netfilter support.
- Make sure the clock is sane. The playbook configures chrony and steps a stale
  clock, but a VM restored from a snapshot can still boot hours behind.

### Maintainer lab

The project's disposable lab uses snapshots instead of uninstalling in place.
Maintainers should follow [`maintainer-lab.md`](maintainer-lab.md); those steps
are not part of a normal install.

---

## 4. Provision

### Single node

```bash
PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh
```

### HA

The prompted setup can generate the HA environment, run `verify-ha.sh` and then
run the provisioner:

```bash
./install-ha.sh
```

To verify an existing environment without changing anything:

```bash
./verify-ha.sh deploy/provision-ha.env
```

Or edit the environment manually:

```bash
PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh
```

The script:

1. cross-compiles the console binary for Linux/amd64;
2. packages the identity, console, WireGuard and app charts into
   `chart-mirror/`;
3. ships the playbook, catalog and chart mirror to `JUMP`;
4. runs `ansible-playbook site.yml` there — host setup, firewall, k3s, Calico,
   storage, identity, Traefik, console, NetworkPolicies;
5. builds the console image locally with `podman` and imports it into each
   node's containerd;
6. runs the `console` tag again so the deployment uses that image.

The first run takes 15–40 minutes depending on package and image downloads. It
is idempotent: fix the error and re-run the same command.

Success looks like:

```text
PLAY RECAP *********************************************************************
192.0.2.10 : ok=... changed=... unreachable=0 failed=0 ...
```

and the console answering:

```bash
kubectl -n umkim-thin get deploy,pod
```

> On the primary the k3s kubeconfig is root-only. The installer exposes
> `/usr/bin/kubectl`, so `sudo kubectl` works without adding `/usr/local/bin`
> to sudo's secure PATH. If an older install still says `kubectl: command not
> found`, create the shim once:
>
> ```bash
> sudo ln -s /usr/local/bin/kubectl /usr/bin/kubectl
> ```
>
> The remaining examples use `kubectl` for brevity.

### Verify the install

From the primary:

```bash
kubectl get nodes -o wide                 # the node is Ready
kubectl -n kube-system get pods           # calico, coredns, traefik (plus kube-vip on HA)
kubectl -n identity get pods              # authelia, lldap, invite-signup
kubectl -n umkim-thin get deploy,pod      # umkim-thin-console 1/1 Running
kubectl get storageclass                  # local-path and umkim-thin-nfs (or legacy nfs)
kubectl get ingress -A                    # auth.<zone>, console.<zone>, home.<zone>
```

Expected: one `Ready` node, the console pod `1/1 Running`, both storage classes
present, and three Ingresses for the portal, console and home page. The first app
install adds more.

From your workstation:

```bash
curl -k -o /dev/null -w '%{http_code}\n' https://console.<zone>/healthz
curl -k -o /dev/null -w '%{http_code}\n' https://auth.<zone>/
```

`302` for the console (it redirects to its login) and `200` for the portal are
the normal answers. A TLS warning is expected until you trust the lab CA.

The successful `provision.sh` run also prints the two `sudo sed` commands for
the directory admin and console breakglass passwords. The same commands are in
the [HA quickstart](quickstart/11-ha-install.md#4-get-the-login-passwords).

`./ci/assert.sh single` runs the same checks end-to-end on a host that can
reach the cluster: portal/console reachability and login, security headers, TLS
issuer, etcd encryption, the Nextcloud directory lookup, cross-namespace
NetworkPolicy, shared RWX media, and the WireGuard handshake/revocation. The
WireGuard check dials the node directly, so run it from a connected host.

> **App charts:** the catalog pins the app charts by OCI reference. The source
> lives in the `umkim-charts` repository and is pinned as the `charts/apps/`
> submodule; `provision.sh` packages it into the node's chart mirror, so the
> cluster does not need to pull the OCI ref. Clone with
> `--recurse-submodules`, or run `git submodule update --init --recursive`.
> Use `chart_repos` only for an out-of-tree chart. A full URL such as
> `https://example.com/me/charts.git:helm/app` is supported; it is cloned on
> the workstation and packaged into the mirror.

### If it stops early

The script is safe to re-run. Common first-run issues:

| Symptom | Cause / fix |
|---|---|
| `dnf` retries or times out | the host cannot reach the package mirror; fix DNS/egress |
| `kubectl`/`helm` missing | the `common`/`k3s` task failed earlier; fix that first |
| reboot mid-run | expected when the image lacks `xt_*` kernel modules; the playbook continues |
| `Unreachable` on a server | the jumphost cannot SSH to it; verify the key and network path in your environment file |
| chart mirror empty | `chart_repos` points at a repository or subdirectory that was not cloned; check the URL and the script's `helm package` lines |

---

## 5. First login

### Get the admin password

The install generates the directory admin and breakglass passwords on the
primary:

```bash
sudo sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p' /etc/umkim-thin/secrets.yml
sudo sed -n 's/^secret_console_localadmin_password: "\(.*\)"/\1/p' /etc/umkim-thin/secrets.yml
```

`./ci/credentials.sh single` prints them masked; add `--reveal` to print the
values. The helper reads the cluster, never writes a file, and works for both
console modes.

Store these in a password manager. The directory admin is also the Authelia
user that can reach the portal and the console.

### Sign in

1. Open `https://console.<zone>`.
2. Authelia asks for a directory login: use `admin` and the directory password.
3. The console then asks for its own login: `admin` again, or the breakglass
   `localadmin`.

The console has its own session on purpose: Authelia protects the route, but the
console remains usable when the portal or directory is unavailable
(`localadmin`).

### Invite the first real user

Console → **Invitations** → **Create invite**, then copy the link. The invitee:

1. opens `https://auth.<zone>/invite?token=...`;
2. chooses a username and password (at least 8 characters);
3. signs in at `https://auth.<zone>`.

The CLI has the same operation:

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin invite create --email someone@example.com
```

An invitation always creates a **new** account and never resets or takes over an
existing one. Revoking it removes the link; redeeming it twice is refused.

---

## 6. Install your first app

Console → **Catalog** → choose an app (start with **Gitea** or **Nextcloud**),
pick the hostname and storage classes, and click **Install**. The **Advanced**
section in the same panel can render the chart's PVCs and override each claim's
storage class and size, and can choose a namespace for an ordinary app. The same
from the CLI:

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin app install gitea --namespace gitea
```

The `servarr` and `wireguard` namespaces are fixed because their bootstrap and
gateway wiring use namespace-specific names. Existing apps cannot be moved by
changing the namespace field; uninstall/reinstall is required.

The installer:

- creates the namespace;
- publishes the Ingress with the Authelia middleware;
- wires directory authentication (LDAP bind Secret, or an OIDC client in the
  identity release);
- runs `helm upgrade --install` with the pinned chart and values;
- exempts the app's public paths (share links, native clients) in Authelia.

First installs can take a few minutes while images pull. Check progress in
**Applications** (status chips) or:

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- /usr/local/bin/umkim-thin app status gitea
```

Open `https://gitea.<zone>`; sign in with the invited directory user (Gitea
uses LDAP, and the admin source is LDAP/OIDC). The catalog's auth modes are in
[`docs/auth.md`](auth.md).

**Re-apply the NetworkPolicies after installing apps.** The `netpol` role only
sees namespaces that exist when it runs; the first provision therefore covers
the platform, not apps installed later. After a batch of installs:

```bash
PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh --tags netpol
```

This is the default-deny ingress policy for every app namespace (same
namespace + Traefik + the WireGuard path). The lab e2e does the same step after
installing its app set.

### Uninstall

From the console or:

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin app uninstall gitea          # keeps data
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin app uninstall gitea --purge  # deletes the namespace/PVCs
```

### Remove the platform

Run the full host teardown (it removes k3s and all cluster data; see
[the clean-host uninstall guide](quickstart/09-uninstall.md)):

```bash
sudo ./teardown.sh
```

`./install.sh --uninstall` delegates to it. For HA, use the wrapper from the
checkout (`./uninstall-ha.sh --env deploy/provision-ha.env`), which runs
`teardown.sh` on every server with the primary last.

---

## 7. Trust the certificate

With the default lab profile, every route is covered by a wildcard signed by
the local CA:

```bash
# on the primary
sudo cat /etc/umkim-thin/tls/ca.crt
```

Import that `ca.crt` into your browser/OS trust store (or use
`curl --cacert ca.crt https://console.<zone>/healthz`). A private CA is not
trusted by a browser automatically; this is expected, not a broken install.

With `lab_ca_default=false` Traefik obtains certificates per host. In staging
they are signed by the Let's Encrypt staging roots and browsers will warn; in
production they are trusted. ACME HTTP-01 needs port 80 to reach Traefik from
the internet. A wildcard default certificate makes Traefik skip ACME for a
matching router, so leave it off when you want real issuance.

---

## 8. WireGuard (optional)

1. Forward `51820/udp` to the node (or the HA VIP).
2. Install the **WireGuard gateway** from the catalog (or
   `/usr/local/bin/umkim-thin app install wireguard`).
3. Console → **WireGuard clients** → create a client.
4. Scan the QR code or download the `.conf`; import it into the WireGuard app.

The gateway runs as a `hostNetwork` DaemonSet with `NET_ADMIN`: every node has
a local UDP 51820 listener. On HA, forward UDP 51820 to the VIP; the node that
owns the VIP already has a gateway. If an app is meant to be reachable only
over the tunnel, install it with `--exposure vpn` (and optionally
`--allow-local`); the route is published only on the internal `vpn` entrypoint.
The failover is not zero-downtime: an existing WireGuard session re-handshakes
when the VIP moves.

### Same-host trusted access

To give native clients the public app URL over the VPN and/or management
network, enable the trusted-access bypass in the generated `config.env`:

```text
UMKIM_THIN_TRUSTED_ACCESS_ENABLED=true
UMKIM_THIN_TRUSTED_MANAGEMENT_ENABLED=true
UMKIM_THIN_TRUSTED_MANAGEMENT_CIDRS=172.16.10.0/24
UMKIM_THIN_TRUSTED_MANAGEMENT_EXCLUDE_CIDRS=172.16.10.1/32
```

The exclude list must contain the source address a public router or proxy
masquerades to when that address falls inside the management CIDR; otherwise
public requests would match the bypass. It can be empty when the public path
preserves the real client source.

If the ingress path SNATs every request to a node address (common with
`externalTrafficPolicy: Cluster` and a remote endpoint), public and management
are indistinguishable: disable the management bypass with
`UMKIM_THIN_TRUSTED_MANAGEMENT_ENABLED=false` and use the VPN path. Do not
enable it on a path that destroys the source address.

The management path is a high-priority Traefik `ClientIP` route on
`websecure` with no Authelia. The VPN path uses the same host on the internal
`vpn` entrypoint, with the existing source allowlist. Both still require the
app's own `route.appAuth`. The public route remains Authelia-gated. The
gateway resolves opted-in app hosts to itself and forwards `auth`, `console`
and `home` normally; its DNAT is restricted to the gateway address so a
full-tunnel client can still reach public names.

Do not use `0.0.0.0/0` as a management CIDR. The installer rejects it.
After changing the flag or CIDRs, reapply the console config and reinstall or
upgrade the affected app so the route objects are reconciled; turning the flag
off removes the stale bypass on the next install. The legacy `.vpn.<zone>`
host remains available when a route sets `trusted.sameHost: false`.

When upgrading from the old single-replica chart, delete the old Deployment
first (`kubectl -n wireguard delete deploy wireguard-gateway`); the new chart
creates a DaemonSet.

---

## 9. HA specifics

HA runs the same platform on three servers and adds:

| Piece | What changes |
|---|---|
| Control plane | embedded etcd, 3 servers; the first initialises, the rest join |
| VIP | `k3s_control_plane_vip`; kube-vip owns it and the API/ingress use it |
| Storage | `storage_backend=longhorn`; `longhorn-rwo` for config with 3 replicas by default (`longhorn_replica_count=2` optional); external NFS for shared media is optional and can be added later in the console. Until then media uses `longhorn-rwx`. Longhorn is thin, with `longhorn_storage_over_provisioning_percentage=100` by default. |
| Console | A PVC holds state/state secrets, the pod uses its ServiceAccount kubeconfig, no hostPath state/hostNetwork; the storage-agent socket is absent |
| Firewall | firewalld is **disabled** on multi-node; Calico's routing and NetworkPolicies are the enforcement |
| Secrets at rest | etcd encryption stays **off** until the k3s key is distributed (documented in `docs/security-plan.md`) |

### Provision

```bash
PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh
```

After installing apps, apply the NetworkPolicies to the new namespaces:

```bash
PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh --tags netpol
```

### Verify

```bash
kubectl get nodes -o wide                 # 3 Ready
kubectl -n kube-system get pods           # calico, traefik, kube-vip
kubectl -n longhorn-system get pods       # longhorn-manager/engines
kubectl get storageclass                  # longhorn-rwo, longhorn-rwx; NFS class if configured
kubectl -n umkim-thin get deploy,pod,pvc  # console Running, PVC Bound
kubectl -n umkim-thin get endpointslices  # the console has an endpoint
```

Check the VIP and the ingress from a machine on the same network:

```bash
curl -k https://console.<zone>/healthz --resolve console.<zone>:443:<VIP>
```

or add `<VIP> console.<zone> auth.<zone> ...` to `/etc/hosts` when public DNS
does not hairpin to the VIP.

`./ci/assert.sh ha` checks the same platform invariants as single, including the
cross-node RWX media volume and the WireGuard tunnel. When public DNS does not
hairpin to the VIP, pin the HA names to the VIP on the test runner.

External NFS is not required at provision time. Add it later in the console's
**Storage → External NFS** form; it creates an `nfs-<name>` class. Select that
class for new app installs. Existing media PVCs keep their current class;
reinstall or migrate them to switch.

### HA caveats

- **One VIP for the API and ingress.** DNS for `*.<zone>` should point at the
  VIP (or a load balancer in front of it). A test runner may pin the names to the
  VIP when the public DNS record does not hairpin.
- **Longhorn needs disk.** The default data path is `/var/lib/longhorn` on each
  node. The default is 3 replicas per volume; set `longhorn_replica_count=2` if
  three do not fit. Prefer external NFS for large media once it is configured;
  until then media uses `longhorn-rwx` and competes for the same disk. If a PVC
  stays `Pending`, check `kubectl -n longhorn-system get nodes.longhorn.io` and
  the volume conditions; `ReplicaSchedulingFailure: insufficient storage` means
  the nominal scheduled capacity is exhausted, not that the app is broken.
- **WireGuard failover HA.** The gateway is a DaemonSet, so every node has a
  listener. Forward UDP 51820 to the VIP; kube-vip can move the VIP to another
  node without moving the workload. Existing sessions re-handshake.
- **The default certificate is still local.** Copy the primary's
  `/etc/umkim-thin/tls/ca.crt` to your browser. A test HA zone may not complete
  public ACME from inside the test network; that is why the local CA is the
  default.

---

## 10. Day 2

| Task | Command |
|---|---|
| Re-apply the platform (idempotent) | `PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh` |
| Re-run only one role | `PROVISION_ENV=... ./deploy/provision.sh --tags identity` |
| Rebuild/import the console image | Run the full `PROVISION_ENV=... ./deploy/provision.sh` (it builds/imports), then the console tag is deployed; `--tags console` only redeploys an already-imported image |
| List installed apps | `kubectl -n umkim-thin exec deploy/umkim-thin-console -- /usr/local/bin/umkim-thin app list` |
| See app health | `kubectl -n umkim-thin exec deploy/umkim-thin-console -- /usr/local/bin/umkim-thin app status <name>` |
| Console/directory credentials | `./ci/credentials.sh single` (or `ha`) |
| Lab reset | See [`maintainer-lab.md`](maintainer-lab.md) |
| Tests | Tier 1: `./ci/static.sh`; Tier 2: `./ci/e2e.sh single` or `tests/e2e/run.sh all` |

Back up the state directory (`/etc/umkim-thin`, the seed on both targets), the
console PVC, the Longhorn/NFS volumes, and the app databases. The security plan
(`docs/security-plan.md`) lists the remaining hardening work; do not expose the
console to the internet without reading it.

### Single-node state migration

Single-node installs now use the same PVC-backed console as HA. On the first
provision after the change, the console chart seeds the PVC from
`/etc/umkim-thin` (secrets, identity values, invites, WireGuard peers, app state
and the chart mirror); the host directory remains as a backup. Verify it with:

```bash
TARGET=single tests/e2e/run.sh migration
```

The check is a no-op on HA.

---

## 11. Troubleshooting

| Symptom | Check |
|---|---|
| Login appears to loop | clock skew: `chronyc makestep` on the nodes; compare `date -u` with the client |
| Browser warns about the certificate | import `state_dir/tls/ca.crt`; staging certs are untrusted by design |
| `404`/`502` on an app host | `kubectl -n <app> get ingress,svc,endpoints`; the host must match the catalog route |
| App install cannot find the chart | the entry is not vendored; add it under `charts/apps/` (or set `chart_repos`) and re-run the provisioner |
| PVC `Pending` | storage class/access mode; `kubectl describe pvc`; on HA check Longhorn disks/replicas |
| Pods `ContainerCreating`/`Pending` on HA | Calico/firewalld; firewalld must be off, pod CIDR must not overlap the node network |
| Console `401`/`403` | Authelia session vs console login; `localadmin` breakglass; check `/etc/umkim-thin/console-session-key` exists (otherwise sessions are ephemeral) |
| WireGuard handshake fails | UDP 51820 forwarding, `wg_endpoint`, and a gateway pod on the node that owns the VIP |
| `helm`/`kubectl` not found on the target | the `common`/`k3s` role did not finish; re-run that tag |
| Certificate issuance never happens | a matching default cert is present (`lab_ca_default=true`); set it false and ensure port 80 reaches Traefik |
| HA VIP not reachable | kube-vip pod/lease, the VIP is outside the DHCP pool, and the router forwards to it |
| SSH warns that the host key changed after a reset | expected after a snapshot restore; remove the old key on the connecting host with `ssh-keygen -R <address>` |

Logs:

```bash
kubectl -n umkim-thin logs deploy/umkim-thin-console
kubectl -n identity logs deploy/authelia
kubectl -n identity logs deploy/lldap
kubectl -n kube-system logs deploy/traefik
kubectl get events -A --sort-by=.lastTimestamp | tail -30
```

---

## Appendix: the steps `provision.sh` performs

`provision.sh` is a thin wrapper. This outline is for debugging or embedding
the steps in your own automation; it is **not** a replacement for the wrapper,
which also renders `inventory.ini` and `group_vars/all/99-local.yml` from the
environment file. Run the build and packaging on the workstation, the playbooks
on the jumphost, and the image import from the workstation.

```bash
# 1. build and stage (workstation)
stage=$(mktemp -d)
cp -r deploy/ansible "$stage/ansible"
mkdir -p "$stage/ansible/group_vars/all" "$stage/ansible/artifacts/charts" "$stage/ansible/artifacts/chart-mirror"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$stage/ansible/artifacts/umkim-thin" ./cmd/umkim-thin
cp -r catalog "$stage/ansible/artifacts/catalog"
cp -r charts/identity charts/umkim-thin "$stage/ansible/artifacts/charts/"
for d in charts/apps/*; do
  helm package "$d" -d "$stage/ansible/artifacts/chart-mirror"
done
helm package charts/wireguard-gateway charts/identity -d "$stage/ansible/artifacts/chart-mirror"

# 2. render the inventory and site vars, ship, and run the host/platform play
#    (the wrapper writes 99-local.yml and inventory.ini from the env file; see
#    deploy/provision.sh for the exact template)
tar czf - -C "$stage" ansible | ssh "$JUMP" 'rm -rf ~/umkim-thin-deploy && mkdir -p ~/umkim-thin-deploy && tar xzf - -C ~/umkim-thin-deploy'
ssh "$JUMP" 'cd ~/umkim-thin-deploy/ansible && ansible-playbook -i inventory.ini site.yml --skip-tags console'

# 3. build/import the console image (workstation -> each node)
podman build -t localhost/umkim-thin:dev .
for h in $servers; do
  podman save --format docker-archive localhost/umkim-thin:dev | gzip -1 | \
    ssh "$JUMP" "ssh $VUSER@$h 'sudo /usr/local/bin/k3s ctr images import -'"
done

# 4. deploy the console with the image now present (jumphost)
ssh "$JUMP" 'cd ~/umkim-thin-deploy/ansible && ansible-playbook -i inventory.ini site.yml --tags console'
```

The wrapper also renders `inventory.ini` and `group_vars/all/99-local.yml` from
the environment file, so use it for a normal install; the manual path is for
debugging or embedding the steps in your own automation.

## Appendix: files and state

| Path | What it is |
|---|---|
| `state_dir/secrets.yml` | LLDAP/Authelia/console secrets, generated once, mode 0600 |
| `state_dir/identity-values.yml` | the rendered identity chart values |
| `state_dir/identity-apps.yml` | OIDC clients and public-path rules contributed by apps |
| `state_dir/apps-state.json` | OIDC client secrets/state for re-installs |
| `state_dir/apps/<app>-values.yaml` | the last rendered values for each app |
| `state_dir/invites.json` | invitation tokens (the console writes it) |
| `state_dir/wg-clients.json` | WireGuard peers |
| `state_dir/console-session-key` | the console's own session HMAC key |
| `state_dir/authelia-oidc-hmac`, `authelia-oidc-key.pem` | Authelia's OIDC signing material |
| `state_dir/tls/` | platform CA, wildcard certificate and the CA bundle |
| `state_dir/kubeconfig` | the installer's copy of the k3s kubeconfig; the console pod uses its ServiceAccount |
| `remote_dir/chart-mirror/` | packaged charts the installer prefers |
| `remote_dir/charts/` | the in-repo charts (identity, console) |

Back up `state_dir` (the seed on both targets) and the console PVC together with
the app volumes; without `secrets.yml` a rebuild loses the directory admin
password and the generated OIDC client secrets.

## Appendix: what the playbook does, in order

1. `common` — packages, swap, forwarding, chrony, SELinux/k3s policy, kernel
   reboot if `xt_*` is missing.
2. `firewall` — single: open the published ports and restrict the API to
   `lab_cidr`; HA: disable firewalld (Calico owns the dataplane).
3. `k3s` — install the server(s), kube-vip on HA, join the extra servers, install
   Helm.
4. `calico` — pin the IP pool to `pod_cidr`, native routing, wait for nodes.
5. storage classes — `nfs` for a single-node or external shared export;
   `longhorn` + `longhorn_host` for HA (3 replicas by default, or 2). External
   NFS is optional for HA and can be added later in the console; until then the
   media class is `longhorn-rwx`. Single-node installs also get the host storage
   agent (`docs/storage.md`) for blank disks/RAID/multiple NFS exports and
   per-export StorageClasses.
6. `identity` — LLDAP, Authelia, Traefik middleware, invite page, generated
   secrets and OIDC signing material.
7. `traefik` — NodePorts, ACME, the lab CA/default cert, the CA bundle for
   in-cluster OIDC clients.
8. `console` — image build/import, config, chart, rollout.
9. `netpol` — default-deny ingress for the app namespaces.

The runtime state lives on the console PVC in both targets: secrets, identity
values, the app values, the console session key and the WireGuard/invite JSON.
The primary's `state_dir` (`/etc/umkim-thin` by default) is the seed for the PVC
and still holds the installer kubeconfig, the TLS material and the chart mirror.
Treat both as sensitive.
