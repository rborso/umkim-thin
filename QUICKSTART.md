# Quickstart: one server, one app

The short path: a Rocky Linux server → a running portal, console, directory, and
one app behind the portal. About 30 minutes, most of it downloads. The detailed
walkthrough is in [`docs/quickstart/`](docs/quickstart/README.md). For a
three-node HA install, see
[`docs/quickstart/11-ha-install.md`](docs/quickstart/11-ha-install.md) or run
`./install-ha.sh` for a prompted environment. The single-server `install.sh`
path below is not HA.

> **Status:** alpha. One trusted household admin, one directory, one tenant. Read
> [`docs/quickstart/10-security.md`](docs/quickstart/10-security.md) before
> exposing it to the internet.

## 0. What you need

- A fresh **Rocky Linux 9 or 10** host, 4 GB RAM, 2 vCPU, 20 GB disk, public IPv4.
- A domain. Create `A` records for:
  - `auth.<zone>`
  - `console.<zone>`
  - `home.<zone>`
  - `*.<zone>` (covers `lldap.<zone>` and every app hostname)
- Ports **80** and **443** reachable from the internet (ACME HTTP-01). Add
  **51820/UDP** only if you want WireGuard.
- An email address for ACME and the directory admin.

The installer builds and imports the console image locally; the internal
console and bootstrap images are not prebuilt. It installs `podman` if needed.

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

The installer pins those NodePorts and now fails if Traefik does not expose
them (a random NodePort makes the forward hit a closed port). Before switching
to production ACME, run the read-only check:

```bash
EXPECTED_IP=<public-ip> ./ci/acme-check.sh <zone> auth console lldap nextcloud ...
```

It validates DNS and the certificate issuer and never asks Let's Encrypt for a
certificate, so it cannot consume the rate limit.

For the measured version of this path — clean VM, all six catalog apps used at the time, and the
first ordinary user — see
[`docs/manual-install-timed.md`](docs/manual-install-timed.md): **24 m 36 s**
end to end, of which **21 m 20 s** is the install stages; 295 s was the platform
install.

OIDC apps (Immich, Paperless, and Gitea's optional OIDC source) also need the
issuer certificate to be trusted. On a test install using Traefik's self-signed
default certificate, their SSO bootstrap logs a warning and OIDC login will not
work; LDAP (Gitea/Nextcloud) and the app itself still do. Use a real public host
with a trusted certificate for full OIDC.

## 1. Install the platform

```bash
git clone --recurse-submodules https://github.com/rborso/umkim-thin.git
cd umkim-thin
sudo ./install.sh --zone example.com --email you@example.com
```

On a test host without public DNS/ACME, add `--acme-staging`. On a real public
host, leave ACME alone.
The default install builds the console locally; `--image` is only for an
already-available image.

Wait for the summary:

```
umkim-thin is installed.
  Portal     https://auth.example.com
  Console    https://console.example.com
  Home       https://home.example.com
  Directory  https://lldap.example.com
```

The generated passwords are in `/etc/umkim-thin/secrets.yml` (mode 0600). If the
installer ran without a terminal it does not print them, so read them:

```bash
sudo sed -n 's/^secret_lldap_admin_password: "\(.*\)"/directory admin: \1/p' \
  /etc/umkim-thin/secrets.yml
sudo sed -n 's/^secret_console_localadmin_password: "\(.*\)"/breakglass: \1/p' \
  /etc/umkim-thin/secrets.yml
```

If anything fails, the first command is always:

```bash
sudo ./install.sh --doctor
```

## 2. First login

1. Open `https://auth.<zone>` and sign in as `admin` with the directory password.
   Register a TOTP device when prompted.
2. Open `https://console.<zone>`. If the portal asks again, sign in; then the
   console's own login appears. Use `admin` and the same directory password.
3. The **breakglass** account is `localadmin` with the second password. It works
   on the console only, when the directory or portal is unavailable.
4. `https://lldap.<zone>` is the directory UI behind the portal. This is where
   you reset a forgotten password or remove a person. LLDAP has no email reset;
   that is deliberate.

## 3. Invite someone

Console → **Invitations** → create an invite (leave the group as
`default-tenant`) → copy the link → send it. The invitee opens the link, chooses
a username and password, and is in the directory.

There is no shared password: the invite creates one account for that person.

## 4. Install Nextcloud

1. Console → **Catalog** → **Nextcloud** → **Install**.
2. Leave the defaults. On a single node, the media storage class is
   `local-path`; the ReadWriteMany warning is expected (single node only).
3. Wait for the job. **Applications** should show:

   ```
   nextcloud (nextcloud) — helm deployed, ready
   Deployment nextcloud           1/1 ready
   Deployment nextcloud-collabora 1/1 ready
   Deployment nextcloud-postgres  1/1 ready
   Deployment nextcloud-redis     1/1 ready
   Job        nextcloud-bootstrap 1/1 complete
   ```

4. Open `https://nextcloud.<zone>`. You get the portal first, then Nextcloud's
   own login. Nextcloud reads the directory over LDAP, so an invited person uses
   **the same username and password** there. A second login screen is expected;
   a second *password* would not be.
5. In a private window, `https://nextcloud.<zone>` must redirect to the portal,
   not show the app.

The app's own admin (`nc-admin`) is in the `nextcloud-secrets` Secret; normal
use does not need it.

## 5. Verify

```bash
sudo ./install.sh --doctor
```

You want `All checks passed.` — node, API, identity/console workloads, routes,
no pending Helm releases, storage, and a backup. Take the first backup:

```bash
sudo ./install.sh --backup
```

On a public host, check the padlock on the portal and console. On a test host
using `--acme-staging`, the self-signed warning is expected; use
`sudo ./install.sh --doctor --insecure`.

## If it fails

| Symptom | Fix |
|---|---|
| `image ... not found` / `ErrImagePull` | Re-run the local image build/import; no prebuilt internal image is expected |
| ACME / certificate errors | DNS must resolve to the host and port 80 must reach it; on a test host use `--acme-staging` and expect a self-signed certificate |
| `unsupported OS` | Use Rocky 9/10; the Ansible path covers other RHEL-family hosts |
| `k3s is already installed` | Use a fresh VM, or `--reuse-k3s` to upgrade an existing install |
| `kernel ... has no xt_mark` | `sudo dnf -y install kernel kernel-modules && sudo reboot` |
| App pods pending | `sudo ./install.sh --doctor`, then `kubectl -n <app> get pods` |
| App install failed | Console → Applications → retry; the job log is in the console. Fallback: `kubectl -n umkim-thin exec deploy/umkim-thin-console -- /usr/local/bin/umkim-thin --catalog /opt/umkim-thin/catalog app install nextcloud` |

## What this does **not** give you yet

- No per-app access lists: every directory user is trusted with the family apps.
  Each app's own login is the second gate (see `PROJECT_GOAL.md`).
- No email/SMTP: password reset is manual from `lldap.<zone>`.
- The console is the admin's; family members get the read-only app launcher at `https://home.<zone>`.
- Only the verified catalog apps are installable. An entry is accepted only
  after it passes the verification suite; do not add one by hand.
