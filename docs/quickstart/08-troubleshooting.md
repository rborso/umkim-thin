# 8. Something is broken

**Goal:** find the failing layer without guessing.

## Start here

```bash
sudo ./install.sh --doctor
```

It checks the host, the Kubernetes API, the identity and console workloads, DNS,
HTTPS, storage, free disk, and whether a backup exists. It prints `ok` / `FAIL`
for each. The first `FAIL` is usually the cause; later ones are often fallout.

If the console is up, the **Applications** section also shows per-workload
readiness.

## Common problems

### DNS

Symptom: `--doctor` says `auth.<zone> does not resolve`, or certificates fail.

```bash
dig +short auth.example.com
dig +short console.example.com
dig +short nextcloud.example.com
```

They must return the server's public IPv4 address. Create the `A` records
(including a wildcard for apps), then wait for propagation.

### Ports 80/443

Symptom: the installer refuses to start, or certificates never issue.

```bash
sudo ss -lntp | grep -E ':(80|443)\b'
```

Stop the other web server. Then check the **cloud firewall / security group**:
80 and 443 must reach the node, and 51820/UDP if you use WireGuard. The host
firewalld ports are opened by the installer; a cloud firewall is not.

### Firewall (firewalld)

Symptom: the installer is fine, but nothing answers from outside.

The installer opens these in firewalld when it is running: **80/443** (Traefik),
**2222/30222** (Gitea SSH, if you install it), **51820/UDP** (WireGuard),
**8472/UDP** (flannel) and **10250** (kubelet); it also trusts the pod and
service CIDRs. Check:

```bash
sudo firewall-cmd --state
sudo firewall-cmd --list-all
sudo firewall-cmd --list-all --zone=trusted
```

It does **not** open the Kubernetes API (6443) to the internet. If you need
external `kubectl`, forward it over SSH instead of opening the port.

A cloud firewall / security group is separate; open 80/443 there yourself.

### SELinux

Rocky enforces SELinux by default. The installer installs `container-selinux`
and the `k3s-selinux` policy from Rancher's repository; do not disable SELinux
to work around a denial. If pods are stuck, check:

```bash
getenforce
sudo ausearch -m avc -ts recent | tail -30
sudo rpm -q k3s-selinux container-selinux
```

If `k3s-selinux` is missing, rerun the installer (it installs it on an
enforcing host) or install it from the `rancher-k3s-common-stable` repository.

### Kernel modules

Symptom: pods start but Service traffic does not work.

k3s networking needs the kernel's `xt_*` netfilter modules. A stripped or
non-default kernel can lack them. The installer warns when `xt_mark` is missing:

```bash
find /lib/modules/$(uname -r) -name 'xt_mark.ko*'
```

Boot the distribution kernel (`sudo grubby --set-default ...`) and rerun.

### Certificates

Symptom: the browser warns, or the portal redirects in a loop.

Let's Encrypt uses HTTP-01: the certificate authority must reach
`http://<host>/.well-known/acme-challenge/...` on port 80. Check:

```bash
sudo kubectl -n kube-system logs deploy/traefik --tail=200
sudo kubectl -n kube-system get svc traefik
```

With `--acme-staging`, certificates are intentionally untrusted; use a real
email without staging for a browser-trusted certificate. There are rate limits;
do not retry in a loop.

### The console pod

```bash
sudo kubectl -n umkim-thin get pods
sudo kubectl -n umkim-thin describe pod -l app=umkim-thin-console
sudo kubectl -n umkim-thin logs deploy/umkim-thin-console --tail=200
```

- `ImagePullBackOff`: the local image is not on the node. Re-run the build/import,
  or pass a reachable `--image`.
- `CrashLoopBackOff`: read the log. A missing seed Secret or an unreadable
  state directory is the usual cause on a re-install; the installer’s
  `--doctor` reports the file modes.
- `Pending`: check the PVC and the storage class.

### The identity stack

```bash
sudo kubectl -n identity get pods
sudo kubectl -n identity logs deploy/lldap --tail=100
sudo kubectl -n identity logs deploy/authelia --tail=100
```

A broken LLDAP means no logins. The console’s breakglass account works through a
local port-forward (see [First login](02-first-login.md#the-breakglass-account)).

### OIDC login returns 404 at the issuer

If `https://auth.<zone>/api/oidc/authorization` or
`https://auth.<zone>/.well-known/openid-configuration` returns 404, Authelia
has no `identity_providers.oidc` block. The console keeps the app OIDC clients
on its PVC (`/data/identity-apps.yml`). A current `install.sh` copies that file
into `/etc/umkim-thin/identity-apps.yml` before upgrading identity; on an older
checkout, pull and re-run:

```bash
git pull
sudo ./install.sh --zone example.com --email you@example.com --reuse-k3s
```

Reinstalling an OIDC app also makes the console re-sync the identity release.

### Storage is Pending

Which class is expected depends on the path:

- `install.sh` single node: `local-path` for config and media;
- Ansible single node: `local-path` for config, `umkim-thin-nfs` for media;
- HA: `longhorn-rwo`/`longhorn-rwx`, or `nfs-<name>` after external NFS is
  added.

Check:

```bash
sudo kubectl get storageclass
sudo kubectl get pvc -A
sudo kubectl -n <app> describe pvc
```

`local-path` binds on the node that runs the pod. If the node is full, free
space or add a disk. Do not delete a PVC to “reset” an app; that is the data.

For an NFS class, check the provisioner and its events:

```bash
sudo kubectl -n nfs-provisioner get pods,events
sudo kubectl get storageclass <class> -o yaml
```

### NFS mounts hang or writes fail

The NFS server must allow the cluster's source addresses. The Ansible path uses
Calico native routing, so the server sees **pod** addresses and needs the pod
CIDR; the on-host `install.sh` path uses k3s's default flannel, which
masquerades pod egress to the node address. Allowing both the node subnet and
the pod CIDR covers both. A mount that hangs is usually a firewall or
client-CIDR problem, and `Permission denied` is usually export ownership or
`no_root_squash`. See [Use an NFS server on another host](12-external-nfs.md)
for the export and firewall setup, then test with a mount from a cluster node.

### HA pod Pending with `FailedAttachVolume`

A pod event such as:

```text
AttachVolume.Attach failed ... volume pvc-... is not ready for workloads
```

means the Longhorn volume is not ready, not that the app image is bad. Check:

```bash
sudo kubectl -n <app> get pvc
sudo kubectl -n longhorn-system get volumes.longhorn.io
sudo kubectl -n longhorn-system get volumes.longhorn.io <volume> -o yaml
sudo kubectl -n longhorn-system get nodes.longhorn.io -o yaml
```

In the volume YAML, a condition like:

```text
type: Scheduled
status: "False"
reason: ReplicaSchedulingFailure
message: insufficient storage
```

means Longhorn cannot place the replicas. The default is **3 replicas** per
volume; set `longhorn_replica_count=2` if three do not fit. The `Storage
Scheduled` value on a disk is limited by its usable size (`storageMaximum` minus
`storageReserved`); with the default 100% over-provisioning it cannot exceed
that. A 20 GiB media PVC cannot fit on a disk that is already nearly fully
scheduled, even if the physical disk has some free space.

For HA, large app media should be on the external NFS class, not Longhorn. Check:

```bash
sudo kubectl -n <app> get pvc -o custom-columns=NAME:.metadata.name,CLASS:.spec.storageClassName
```

If media still shows `longhorn-rwo`, the app was installed before its catalog
entry used the media class (or the console image predates that change).
Reinstall it so the PVCs are recreated with the media class, or move the data
with a planned migration. Longhorn is still the right place for the small
database/config volumes.

Ways to unblock an existing cluster:

- move large media volumes (Nextcloud/Immich/Paperless media) to the external
  NFS class;
- free scheduled Longhorn capacity, or add/expand a Longhorn disk;
- do not simply delete a data PVC; preserve or migrate its data first.

The console's app install now passes `configClass` and `mediaClass` into the app
charts; older installs keep whatever class their existing PVCs already use.

### Disk full

```bash
df -h /var
sudo du -sh /var/lib/rancher/k3s /var/lib/umkim-thin 2>/dev/null
```

The k3s containerd store and app PVCs are the usual consumers. Prune unused
images carefully; never delete a PVC without a backup.

### Forgot the admin password

The generated passwords are in `/etc/umkim-thin/secrets.yml` (root only):

```bash
sudo grep -E 'secret_lldap_admin_password|secret_console_localadmin_password' \
  /etc/umkim-thin/secrets.yml
```

If the file is gone and you have no backup, log in with `localadmin` through a
port-forward. Resetting the directory admin without the secret is an advanced
LLDAP operation; restore the backup instead.

### An invite fails

- “unknown invitation”: the token is wrong or revoked.
- “already been used”: single-use.
- The form posts but errors: check the console log and whether LLDAP is ready.
  The signup page proxies to the console Service, so the console must be up.

### “Configuring” never finishes

The console shows Helm's `pending-*` state as **configuring** (amber). Look at
the job log in the console, then:

```bash
sudo kubectl -n <namespace> get pods,jobs
sudo kubectl -n <namespace> describe pod <pod>
```

A failed bootstrap Job (the media stack) does not fail the app install; it can
be retried from **Applications**.

## Collect diagnostics

Before opening an issue, collect (and redact secrets):

```bash
sudo ./install.sh --doctor > diagnostics.txt 2>&1
sudo kubectl get nodes -o wide >> diagnostics.txt
sudo kubectl get pods -A >> diagnostics.txt
sudo kubectl -n umkim-thin logs deploy/umkim-thin-console --tail=500 >> diagnostics.txt
```

Do not include `/etc/umkim-thin/secrets.yml`, invite links, WireGuard configs, or
app admin passwords.

## Next

[Uninstall →](09-uninstall.md)
