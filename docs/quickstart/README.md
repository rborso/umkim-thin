# Quickstart

This is the beginner path: **one Rocky Linux 9/10 server, one command, one
browser**. For the one-page version, see [`../../QUICKSTART.md`](../../QUICKSTART.md). It installs a private app platform with a login portal, a console,
and a place to install apps.

If you already run Kubernetes, or you want the three-node HA / RHEL / Ansible
path, start with [`11-ha-install.md`](11-ha-install.md) or the deeper
[`../install.md`](../install.md). The paths deploy the same charts; they differ
in how the hosts are prepared.

## What you need

- A fresh **Rocky Linux 9** or **10** server (a VPS or a spare machine), 4 GB
  RAM, 2 vCPU, 20 GB disk. Minimal install is enough.
- A **domain** you control (for example `example.com`). You will point three
  DNS records at the server: `auth`, `console`, `home`, and a wildcard for apps.
- Ports **80** and **443** reachable from the internet (for certificates).
  Port **51820/UDP** only if you want WireGuard.
- An email address for the certificate authority and the directory admin.

No Kubernetes or Helm knowledge is required. You will need a terminal and
`sudo`.

A timed clean-VM run that installs the platform, all six catalog apps measured at the time and the
first household user is in [`../manual-install-timed.md`](../manual-install-timed.md)
(**24 m 36 s** wall clock, **21 m 20 s** of install stages; the platform alone
was 4 m 55 s).

## The single-node path

1. [Install on a fresh server](01-install.md) — one command.
2. [First login](02-first-login.md) — the portal, the console, and a second
   factor.
3. [Invite someone](03-invite.md) — a link, not a shared password.
4. [Install your first app](04-first-app.md) — Nextcloud, with a route and auth.
5. [Optional: WireGuard](05-wireguard.md) — a private way in.
6. [Back up](06-backup.md) — before you put anything important in it.
7. [Upgrade safely](07-upgrade.md).
8. [Something is broken](08-troubleshooting.md) — start with `--doctor`.
9. [Uninstall](09-uninstall.md) — what is removed and what is kept.
10. [First-week security checklist](10-security.md).

## Advanced: three-node HA

The beginner path is single-node. If you need a control-plane VIP, Longhorn and
external NFS shared media, use the [three-node HA quickstart](11-ha-install.md).
That page includes prerequisite installation commands and a prompted
`./install-ha.sh` wrapper. HA still requires three servers, a VIP and DNS
pointing at the VIP; external NFS can be added during install or later in the
console.

The same Ansible path can provision one host from a workstation with a jump
host: `./install-single.sh` prompts for the domain, server, jump host,
WireGuard and TLS mode, writes `deploy/provision-single.env`, runs a read-only
preflight and can start the provisioner. The on-host beginner path above
(`install.sh`) needs no jump host or Ansible.

## What the installer puts on the server

| Piece | What it is |
|---|---|
| k3s | A small Kubernetes distribution; runs the workloads |
| Traefik | The web server and certificate manager |
| LLDAP | The directory (users and groups) |
| LLDAP UI | The directory admin page at `lldap.<zone>`, behind the portal (resets, removals) |
| Authelia | The login portal, forward-auth, and OIDC provider |
| umkim-thin console | The browser UI and API for installing apps and inviting people |
| Home page | Read-only app launcher for household users at `home.<zone>` |
| local-path | Storage for the database and config volumes |

The apps you install later come from the catalog. Each one gets a hostname, a
route, and the right auth wiring.

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

## Before you start: DNS

At your DNS provider, create these records (replace `203.0.113.10` with the
server's public IPv4 address):

| Type | Name | Value |
|---|---|---|
| A | `auth` | `203.0.113.10` |
| A | `console` | `203.0.113.10` |
| A | `home` | `203.0.113.10` |
| A | `*` | `203.0.113.10` |

The wildcard covers app hostnames such as `nextcloud.example.com` **and**
`lldap.example.com`. If your provider does not support a wildcard, add a record
for each app and for `lldap`.

Wait for DNS to propagate (`dig +short auth.example.com` should print the
address) before running the installer. Certificates use the HTTP-01 challenge,
so port 80 must reach the server from the internet.

## A note on expectations

This is **alpha** software, and it is a single-admin product today. It is
suitable for a household, a small team, or a lab — not for a hostile multi-tenant
environment. Read the [security checklist](10-security.md) and
[`../security-plan.md`](../security-plan.md) before exposing it to the internet.
