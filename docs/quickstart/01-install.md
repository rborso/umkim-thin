# 1. Install on a fresh server

**Goal:** a running portal and console on your own domain.
**Time:** 10–20 minutes, mostly downloads and certificate issuance.

> This page installs **one server**. For a three-node HA cluster with a VIP,
> Longhorn and external NFS, use the
> [HA quickstart](11-ha-install.md); `install.sh` is not the HA path.

## Before the command

1. A fresh Rocky Linux 9 or 10 server with a public IPv4 address.
2. DNS records for `auth.<zone>`, `console.<zone>`, and `*.<zone>` pointing at
   that address (the wildcard covers `lldap.<zone>` and every app hostname; see
   the [index](README.md#before-you-start-dns)).
3. Ports 80 and 443 open in the cloud firewall / security group.
4. Nothing else using ports 80, 443, 6443, or 51820/UDP.

### Port forwards

If the server is behind a NAT/router or cloud firewall, forward or allow:

| External | To the server | When |
|---|---|---|
| TCP **80** | **30080** (Traefik HTTP) | required |
| TCP **443** | **30443** (Traefik HTTPS) | required |
| TCP **2222** | **30222** (Gitea SSH) | only if you use Gitea SSH |
| UDP **51820** | **51820** (WireGuard) | only if you enable the gateway |

The installer pins Traefik's NodePorts to `30080`/`30443` and the Gitea SSH
entrypoint to `30222`. The host firewall is opened for these ports; the router
or cloud security group still has to allow them. Do **not** expose `6443` (the
k3s API), `30444` (the VPN-only Traefik entrypoint), `8472`, or `10250`.

If you use a cloud load balancer instead of a router, point it at the
`30080`/`30443` NodePorts (or the server's `80`/`443` if the k3s service
load-balancer is publishing them).


The installer refuses to run if a port is busy or if `auth.<zone>` does not
resolve. That is deliberate: these are the two most common causes of a failed
first install.

## Install

From a checkout of this repository:

```bash
git clone --recurse-submodules https://github.com/rborso/umkim-thin.git
cd umkim-thin
sudo ./install.sh --zone example.com --email you@example.com
```

The installer builds and imports the console image locally; internal images are
not prebuilt. It needs `podman` or `docker` (the installer installs `podman` if
neither is present). Use `--image` only for an already-available image.

Useful flags:

| Flag | Why |
|---|---|
| `--dry-run` | Check the host and print the plan; change nothing |
| `--image <ref>` | Use an already-available console image instead of building locally |
| `--render-only --out DIR` | Generate the values/config without root or a cluster |
| `--acme-staging` | Use the Let's Encrypt staging CA (certificates are untrusted; for testing) |
| `--skip-dns-check` | Install before DNS has propagated (the portal will not get a certificate until it does) |
| `--reuse-k3s` | Use an existing k3s (advanced; the installer will not reinstall it) |

## What the command does

1. Checks the OS (Rocky Linux 9/10), architecture, RAM, disk, and ports.
2. Prepares the host: swap off, IP forwarding, chrony, the firewalld ports, and
   the SELinux policy k3s needs (`container-selinux`, `k3s-selinux`).
3. Installs k3s with the console and portal hostnames as TLS SANs.
4. Installs Helm (pinned).
5. Packages the platform and app charts into a local chart mirror.
6. Generates the directory, portal, and console secrets **once** in
   `/etc/umkim-thin/secrets.yml` (mode 0600).
7. Installs LLDAP + Authelia, then configures Traefik for ACME and the
   forward-auth middleware.
8. Seeds the console's state in the cluster, creates the console and
   `wireguard` namespaces, and installs the console.
9. Installs the host storage agent (`umkim-thin-storage`) for local disks, RAID
   and NFS exports.
10. Prints the portal, console and directory URLs and the first-login passwords.

Re-running is the upgrade path, but k3s is already installed: pass
`--reuse-k3s` so the installer keeps the existing cluster instead of refusing
to touch it. It keeps the secrets and upgrades the charts:

```bash
sudo ./install.sh --zone example.com --email you@example.com --reuse-k3s
```

This path uses `local-path` for both config and media. For shared media on a
separate host, add an NFS class from **Storage → External NFS** (see
[Use an NFS server on another host](12-external-nfs.md)); the Ansible
single-node path starts with an NFS media class instead.

## What you should see

```text
============================================================================
umkim-thin is installed.

  Portal     https://auth.example.com
  Console    https://console.example.com
  Home       https://home.example.com
  Directory  https://lldap.example.com  (admin / ...)
  Breakglass localadmin / ...

  Admin password:      ...
  Breakglass password: ...
  Save these now. They are also in /etc/umkim-thin/secrets.yml (mode 0600).
  ...
============================================================================
```

If the summary does not print, the install did not finish. Read the error and
use the [troubleshooting guide](08-troubleshooting.md). The first place to look
is always:

```bash
sudo ./install.sh --doctor
```

## If it fails

| Message | Fix |
|---|---|
| `port 80 is already in use` | Stop the other web server (`systemctl stop nginx` / `apache2`), or use a fresh host |
| `auth.example.com does not resolve` | Create the DNS record and wait for propagation, or `--skip-dns-check` and fix DNS before login |
| `k3s is already installed` | Use a fresh host, or `--reuse-k3s` if you mean it |
| `unsupported OS` | Use Rocky Linux 9/10 for this path, or the Ansible path in [`../install.md`](../install.md) |
| `image ... not found` | Re-run the local build/import, or pass a reachable `--image` |
| A pod is in `ImagePullBackOff` | `kubectl -n umkim-thin describe pod -l app=umkim-thin-console`; rebuild/import the image |

## Next

[First login →](02-first-login.md)
