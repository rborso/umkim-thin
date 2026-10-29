# 11. Optional: three-node HA install

**Goal:** run the same platform on three servers with a control-plane VIP and
survive the loss of one node.

This is the **advanced path**. It uses the Ansible project in `deploy/ansible`,
driven by `deploy/provision.sh`; it is not the one-command single-server path in
[Install on a fresh server](01-install.md).

If you are unsure whether you need this, you probably do not. Start with the
single-node path and move to HA when the extra three servers, VIP and optional
NFS export are worth the operational cost.

## What HA adds

| Piece | HA choice |
|---|---|
| Kubernetes control plane | three servers with embedded etcd |
| API/ingress address | a free VIP owned by kube-vip |
| Config/database storage | Longhorn, `longhorn-rwo`, 3 replicas by default |
| Shared media | external NFS when configured; otherwise Longhorn RWX |
| Console | PVC-backed, ServiceAccount kubeconfig, no host storage agent |
| Firewall | firewalld off; Calico and NetworkPolicies enforce traffic |
| secrets encryption | off until etcd key distribution is automated |
| WireGuard | per-node DaemonSet; UDP 51820 points at the VIP |

## What you need

### Three servers

Use Rocky Linux or RHEL 9/10. A reasonable starting point is:

```text
6 vCPU, 12 GiB RAM, 120 GiB disk per server
```

Longhorn stores **3 replicas per volume by default**. On a small test cluster
where three replicas do not fit, set `longhorn_replica_count=2` in the environment
file (also offered by `install-ha.sh`). Size the Longhorn disk for the small
config/database volumes; keep large media on NFS once it is configured. A
dedicated Longhorn disk is better than sharing the root disk with the OS and
containerd.

Each server needs SSH with a sudo-capable user and outbound internet access.
Do not pre-install Kubernetes, Docker, or a conflicting firewall.

### A VIP

Pick a free IPv4 address on the same network as the servers. kube-vip will own
it and use it for the Kubernetes API and ingress.

Your DNS wildcard must point at the **VIP**, not at one server:

```text
example.com       -> <VIP or public/NAT address>
*.example.com     -> <VIP or public/NAT address>
```

With a NAT/router in front, public DNS usually points at the **public IP**, not
the private VIP. Dynamic DNS is supported: leave `public_ip` blank and
`verify-ha.sh` accepts the current common public/NAT address. Set `public_ip`
only when the public address is fixed and you want the preflight to assert it.

### External NFS (optional at install time)

HA can start without NFS: media uses Longhorn RWX until you add an external NFS
StorageClass in **Console → Storage** after the platform is up. You can still
configure an export during provisioning if you already have one:

```text
server: <nfs-host>
export: /srv/nfs/umkim-thin
```

When supplied, the installer creates the NFS StorageClass as
`umkim-thin-nfs` (older environments may still use the legacy `nfs` name); it
does not format or manage the NFS server. Apps installed before NFS is added
keep their Longhorn class; reinstall or migrate their media PVCs to switch.

### Ports

Forward or allow:

| External | To | Purpose |
|---|---|---|
| TCP 80 | VIP:80 | HTTP / ACME |
| TCP 443 | VIP:443 | HTTPS |
| TCP 2222 | VIP:2222 | Gitea SSH, if installed |
| UDP 51820 | VIP:51820 | WireGuard, if installed |

Do not expose `6443`, `30444`, `8472` or `10250`.

For WireGuard, the gateway is a per-node DaemonSet. Forwarding UDP 51820 to the
**VIP** is the intended HA setup: whichever node owns the VIP already has a
local WireGuard listener.

## Install the prerequisites

Install these on the machine that will run `provision.sh` — usually your
workstation. They are **not** requirements on every Kubernetes node; the
playbook installs the node-side packages.

### Build host

Rocky Linux 10 ships Go 1.24+, so the distro packages are enough:

```bash
sudo dnf install -y git curl tar gzip podman openssh-clients golang ansible-core
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
ansible-galaxy collection install ansible.posix kubernetes.core
```

If `JUMP` is a separate host, run the same `dnf` command there (only
`ansible-core` is strictly required on that host).

Rocky 9’s `golang` package may be older than 1.24; use a Rocky 10 build host or
install an upstream Go toolchain.

Check:

```bash
go version && helm version && podman version && ssh -V && ansible --version
```

It also needs passwordless SSH to every server, including itself. For a
dedicated key:

> **Important:** do not use one of the three target servers as the Ansible
> controller for the first run if the `common` role may reboot it. The Ansible
> process would be killed by that reboot. Use a separate management host for
> the first run, or expect to rerun after the nodes come back. The run is
> idempotent.

```bash
ssh-keygen -t ed25519 -f "$HOME/.ssh/umkim-ha" -N ''
ssh-copy-id -i "$HOME/.ssh/umkim-ha.pub" linux@10.0.0.11
ssh-copy-id -i "$HOME/.ssh/umkim-ha.pub" linux@10.0.0.12
ssh-copy-id -i "$HOME/.ssh/umkim-ha.pub" linux@10.0.0.13
```

If the first server cannot SSH to itself, use a separate management host for
`JUMP`.

### External NFS server (optional)

Skip this if you will add NFS from the console later. The NFS server can be a
NAS or a separate Linux host. It must have a shared export and allow the cluster
nodes, the pod CIDR and the WireGuard subnet. The full setup — exports,
firewall, ownership, verification and NAS notes — is in
[Use an NFS server on another host](12-external-nfs.md).

If the export is ready before provisioning, set `nfs_mode=external`,
`nfs_server` and `nfs_export` in the environment (or let `install-ha.sh` prompt
for the server and path). The installer uses NFSv4.2 by default and creates its
own subdirectories under the export.

### Optional WireGuard client tools

Only needed for testing the tunnel from a client machine:

```bash
sudo dnf install -y wireguard-tools
```

## Quick interactive setup

If you prefer prompts over editing the environment file, run:

```bash
./install-ha.sh
```

It asks for the domain, three server IPs (a range such as `10.0.0.11-13`
works), the VIP, optional fixed public/NAT IP (blank for dynamic DNS), optional
NFS server/export, Longhorn replicas (3 default, 2 optional), TLS mode and SSH
details. It writes the ignored
`deploy/provision-ha.env`, runs a read-only preflight (`verify-ha.sh`), and then
offers to run provisioning. Use `--dry-run` to inspect the generated values,
`--no-run` to stop after writing them, and `--yes` to accept preflight warnings
without the extra prompt.

The preflight checks the local build tools and versions, config values, DNS
records, SSH/sudo to all three nodes, Ansible and its collections, existing k3s
state, Longhorn replica count, optional NFS reachability and the WireGuard
values. Fatal problems stop. If the **only** warning is `JUMP` being one of the
target nodes, the wrapper asks `Run provisioning now? [Y/n]` and defaults to
yes. Any other warning set defaults to **No**; use `--yes` to accept it.

## 1. Prepare the environment file

From a fresh checkout:

```bash
git clone --recurse-submodules https://github.com/rborso/umkim-thin.git
cd umkim-thin

cp deploy/provision-ha.env.example deploy/provision-ha.env
$EDITOR deploy/provision-ha.env
```

If you already have a checkout, update it first:

```bash
git pull --ff-only
git submodule update --init --recursive
```

Minimum changes:

```bash
# The first server can also be the Ansible host ("jump host").
JUMP=linux@10.0.0.11
JUMP_PORT=22
VSSH_KEY=/path/to/key

VHOST=10.0.0.11
servers="10.0.0.11 10.0.0.12 10.0.0.13"
k3s_control_plane_vip=10.0.0.100

zone=example.com
portal_host=auth.example.com
console_host=console.example.com
base_dn=dc=example,dc=com
admin_email=you@example.com
acme_email=you@example.com

storage_backend=longhorn
storage_class_config=longhorn-rwo
# Before NFS is added: media stays on Longhorn RWX.
storage_class_media=longhorn-rwx
nfs_storage_class=umkim-thin-nfs
longhorn_replica_count=3
# Optional: configure NFS during provisioning. Otherwise add it later in the
# console under Storage and select it for new app installs. If enabling NFS,
# also set storage_class_media=umkim-thin-nfs.
# nfs_mode=external
# nfs_server=10.0.0.50
# nfs_export=/srv/nfs/umkim-thin
storage_agent=false
storage_socket=""

secrets_encryption=false
# Optional fixed public/NAT address. Leave blank for dynamic DNS; verify-ha
# accepts the current common public address.
public_ip=""
# Public DNS name clients dial, not the private VIP.
wg_endpoint=wg.example.com
wg_subnet=192.168.169.0/24
wg_server_ip=192.168.169.1
```

`VHOST` must be the first entry in `servers`. The `JUMP` host is only where
Ansible runs; no separate jump host is required. It needs `ansible` installed
and passwordless SSH to all three servers, including itself. If the first node
cannot SSH to itself, point `JUMP` at a separate management host instead.

Keep the file out of Git: `deploy/provision-ha.env` is ignored.

For the full field list, see the
[advanced install guide](../install.md#2-get-the-environment-file).

## 2. Provision

The script itself runs where the checkout and build tools are, usually your
workstation. `JUMP` is the machine where Ansible executes; it can be the first
/ bootstrap server. If you run the script directly on the first server, point
`JUMP` at that host or use `localhost`.

```bash
PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh
```

You need `git`, **Go 1.24+**, **Helm 3.16+**, `podman` (or a `podman` wrapper)
and `ssh` on the machine running the script. These are build-host tools, not
requirements on every target node. If you run the script on the first server,
install them there; otherwise run it from your workstation and keep `JUMP` on
the first server. Ansible must be installed on `JUMP`; the script ships the
playbook there.

The script installs the hosts, firewall, k3s with embedded etcd, kube-vip,
Calico, Longhorn, identity, Traefik and the console. The first run usually
takes 15–40 minutes. It is safe to re-run after fixing an error.

## 3. Verify

On the primary:

```bash
kubectl get nodes -o wide
kubectl -n kube-system get pods
kubectl -n longhorn-system get pods
kubectl get storageclass
kubectl -n umkim-thin get deploy,pod,pvc
```

`sudo kubectl` works through the `/usr/bin/kubectl` shim. On an older install
that still says `command not found`, create it once:

```bash
sudo ln -s /usr/local/bin/kubectl /usr/bin/kubectl
```

Expected:

- three `Ready` nodes;
- Calico, CoreDNS, Traefik and kube-vip healthy;
- Longhorn healthy with 3 replicas (or 2 if selected);
- `longhorn-rwo` and `longhorn-rwx`; the external NFS class (`umkim-thin-nfs`,
  or `nfs` on an older environment) appears only after NFS is configured.
  Exports added from the console's **External NFS** form appear as
  `nfs-<name>` instead;
- before NFS, media PVCs use `longhorn-rwx`; after adding NFS, new installs
  should select the NFS class. Small config/database PVCs stay on Longhorn;
- console Running with a Bound PVC.

Check the VIP path from a machine on the same network:

```bash
curl -k https://console.example.com/healthz \
  --resolve console.example.com:443:10.0.0.100
```

## 4. Get the login passwords

`provision.sh` creates the directory admin and console breakglass passwords but
does **not** print them. They are in `/etc/umkim-thin/secrets.yml` on the
**primary/first server**, mode `0600`:

```bash
sudo sed -n 's/^secret_lldap_admin_password: "\(.*\)"/directory admin: \1/p' \
  /etc/umkim-thin/secrets.yml
sudo sed -n 's/^secret_console_localadmin_password: "\(.*\)"/breakglass: \1/p' \
  /etc/umkim-thin/secrets.yml
```

Store them in a password manager before continuing.

- **Directory admin** — use this at the portal (`https://auth.<zone>`) and as
  `admin` in the console.
- **Breakglass** — use this as `localadmin` in the console if the directory or
  portal is unavailable.

Do not paste the secrets into chat or an issue. `./ci/credentials.sh ha` prints
the same values masked; add `--reveal` to print them. Maintainers in the project
lab can use [`../maintainer-lab.md`](../maintainer-lab.md) for the lab-specific
network notes.

## 5. First login and apps

The first login and invitation flow are the same as the single-node path:

- [First login](02-first-login.md)
- [Invite someone](03-invite.md)
- [Install your first app](04-first-app.md)

After installing apps, apply NetworkPolicies to the new namespaces:

```bash
PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh --tags netpol
```

### Add external NFS after installation

This is the normal path when the export is not ready at install time. Create the
export first if needed: [Use an NFS server on another host](12-external-nfs.md).

1. Console → **Storage** → **External NFS**.
2. Enter a name, the NFS server, the export path and, if needed, a StorageClass
   name and mount options. The defaults are `nfs-<name>` and
   `nfsvers=4.2,rw,nolock`. Plan first, then apply. The console creates
   `nfs-<name>` and a ReadWriteMany, Retain StorageClass.
3. Install new apps with that class as the **media** class in the Catalog form.
4. Existing apps keep their current PVC class. To switch an existing media
   volume, reinstall it with the new class or migrate the PVC; changing a PVC's
   StorageClass in place is not supported.

## 6. WireGuard on HA

1. Forward **UDP 51820 to the VIP**.
2. Install **WireGuard gateway** from the console catalog.
3. Keep the gateway's `nodeSelector` empty so it runs on every node.
4. Set `wg_endpoint` to the public DNS name clients can reach
   (for example `wg.example.com`), not the private VIP.
5. Create clients in **Console → WireGuard clients**.

WireGuard failover is not zero-downtime: when kube-vip moves the VIP, existing
sessions re-handshake against the new node. Client configs stay valid because
they point at the VIP.

If you are upgrading from the old single-replica chart, delete the old
Deployment once before installing the new chart:

```bash
kubectl -n wireguard delete deploy wireguard-gateway --ignore-not-found
```

## 7. Uninstall

Use the HA wrapper from the checkout (or the Ansible controller):

```bash
./uninstall-ha.sh --env deploy/provision-ha.env --dry-run
./uninstall-ha.sh --env deploy/provision-ha.env
```

It reads `JUMP`, `VSSH_KEY`, `VUSER`, `VHOST`, `k3s_control_plane_vip` and
`servers` from the environment file, then runs `teardown.sh` on each server with
the **primary last**, so the shared Helm releases are removed while the API is
still available. It asks once; use `--yes` for a non-interactive run. The
control-plane VIP is removed from any host interface that still has it.

By default it keeps `/etc/umkim-thin`, `/var/lib/umkim-thin`,
`/opt/umkim-thin`, `/var/lib/longhorn` and `/srv/nfs`. k3s cluster data,
including local-path application volumes, is always removed. Add
`--purge-data` to remove the kept host state, chart and Longhorn data as well;
it still does not wipe filesystems under `/srv/nfs`.

The VIP, DNS records and external NAT are infrastructure; remove those
separately. The host-level details are in [Uninstall](09-uninstall.md).

## HA caveats

- **VIP and DNS:** the wildcard must point at the VIP, not a fixed server.
- **Longhorn disk:** if a PVC stays Pending, check Longhorn node conditions and
  reserved space. Longhorn is thin-provisioned, but its scheduler limits nominal
  capacity (`longhorn_storage_over_provisioning_percentage`, default `100`).
  Once configured, keep large media on NFS; raise over-provisioning only on a
  dedicated disk with real free space.
- **NFS:** optional at install and configurable later in the console. Once used,
  shared media depends on the external NFS server; back it up as part of the app
  data.
- **etcd encryption:** leave `secrets_encryption=false` until key distribution
  is automated.
- **Certificate:** a test HA zone may not complete public ACME; use the local
  CA or staging mode as appropriate, and trust `/etc/umkim-thin/tls/ca.crt`.
- **Backups:** back up `/etc/umkim-thin`, the console PVC, Longhorn/NFS volumes
  and app-native exports. A three-node cluster is not a backup.

## Next

- [Back up](06-backup.md)
- [Upgrade safely](07-upgrade.md)
- [Troubleshooting](08-troubleshooting.md)
- [Uninstall](09-uninstall.md)
- [Advanced HA install details](../install.md#9-ha-specifics)
