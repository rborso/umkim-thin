# Timed manual install: clean VM to first user

This is a measured run of the supported single-node path: a **clean VM**, the
platform, all six catalog apps, and finally one ordinary directory user
(`tenant`). It exists to answer two questions with numbers instead of guesses:

- how long a from-scratch install actually takes, per stage;
- where the time goes, so an install that feels slow can be compared with a
  known-good run.

**Result: 24 m 36 s** wall clock from reverting the VM to the `base` snapshot to
the new user's first successful login. The **install stages themselves sum to
21 m 20 s**; the remaining **3 m 16 s** is a 69 s verification re-check (the
first probe used the wrong vantage and waited on 404s) plus 127 s of stage
hand-off and in-VM diagnostics (SSH round trips, starting the next step).

This run was made **after the Paperless/Immich CA fix and the
Nextcloud/Collabora WOPI fix**, so the post-install checks include those paths.
The previous clean run, before those fixes, was **22 m 28 s**; its install
stages were 21 m 12 s. The install work is effectively unchanged; the longer
wall clock here is the extra verification pass.

| | |
|---|---|
| Date | 2026-10-22, times in UTC |
| Host | lab `single-node` (`192.0.2.10`), Rocky Linux 10, single node |
| Start | 19:19:53 |
| End (tenant logged in) | 19:44:29 |
| Total | **24 m 36 s (1476 s)** |
| Zone | `example.com`, production Let's Encrypt (no staging) |
| Apps | nextcloud, gitea, immich, paperless, servarr, wireguard (all six) |
| Last step | directory user `tenant` created and able to sign in |

## What "clean" meant

The VM was reverted to its `base` snapshot, not uninstalled in place. That
snapshot is a **stripped Rocky Linux 10** image: no k3s, no charts, no console
image, no ACME account, no `acme.json`, no `/etc/umkim-thin` secrets, no
directory users beyond the bootstrap admin. The first two stages below are
therefore genuinely from zero.

The lab `base` image is not a stock Rocky ISO: it is deliberately minimal and
needs the kernel, iptables and conntrack packages before k3s can work. That
preparation is called out as its own stage. On a stock Rocky 9/10 install those
packages are already present and the stage disappears (or shrinks to a reboot
after updates).

## Prerequisites done before the clock started

These are operator responsibilities, not install steps. They were already true
when the reset below was issued:

- DNS: `auth.example.com`, `console.example.com` and `*.example.com` resolve to the
  public address `203.0.113.10`.
- Router: WAN **80/443** forwards to `192.0.2.10:30080/30443` (the NodePorts
  the installer pins and validates). A random NodePort makes the forward hit a
  closed port.
- Public IPv4, no other service on 80/443/6443/51820.
- Internal images are built locally; the run built the console from the
  checkout and imported it into k3s. No registry pull was involved.

## Timed stages

| Stage | Start | End | Duration | Covers |
|---|---|---:|---:|---|
| 0. Reset to `base` | 19:19:53 | 19:20:06 | 13 s | `virsh snapshot-revert` + VM comes back |
| 1. Host preparation | 19:20:11 | 19:23:27 | 196 s | Wait for SSH; set clock; install kernel/iptables/conntrack; reboot; wait |
| 2. Copy the tree | 19:23:32 | 19:23:34 | 2 s | `tar` over SSH into `/tmp/umkim-thin` |
| 3. Platform install | 19:23:34 | 19:28:29 | 295 s | k3s, Helm, chart mirror, secrets, console image build, LLDAP+Authelia, Traefik, ACME, console |
| 4. Platform cert check | 19:28:33 | 19:28:33 | <1 s | Verify `auth`, `console`, `lldap` (already issued during stage 3) |
| 5. Apps (six) | 19:28:38 | 19:41:14 | 756 s | Install all catalog apps and create their routes/certificates |
| 6. Verify apps/certs | 19:41:22 | 19:41:34 | 12 s | Route codes, all 14 certificates, pod state |
| 7. `tenant` (last) | 19:44:23 | 19:44:29 | 6 s | Create invite, redeem it, first login |
| | | **Install stages** | **1280 s** | Sum of stages 0–7 |
| | | **Wall clock** | **1476 s** | Plus 69 s re-check and 127 s hand-off/diagnostic |

Apps dominate: **756 s of 1476 s (51%)**. The platform is the next largest at
295 s (20%), host preparation 196 s (13%), and reset/copy/checks/login together
are under a minute. The 295 s platform stage includes the console image build
and the first three ACME certificates.

The 69 s re-check was a probe-vantage mistake, not install work: the first
WOPI/Paperless probes were run from the workstation with
`--resolve <host>:443:127.0.0.1`, which points at the workstation, not the lab
VM. They returned 404s; the same probes run inside the VM returned 403/302.
The 12 s stage above is the route/certificate/pod pass; the in-VM re-check is
covered under “What was verified after the install”.

## Stage detail

### 0. Reset to the base snapshot

```bash
# on the hypervisor (through the lab jumphost)
sudo virsh snapshot-revert single-node base --running
```

13 s, including the domain coming back. Reset is by snapshot on purpose:
uninstalling k3s/Calico/Longhorn in place is slower and leaves state behind.

### 1. Host preparation (lab base image only)

The stripped image boots without a kernel package matching the running kernel
and without the iptables/conntrack modules Traefik and kube-proxy need. With the
clock set first (package metadata and TLS need a sane date):

```bash
sudo date -u -s @<epoch>
sudo dnf -y install kernel kernel-modules iptables iptables-nft conntrack-tools
sudo reboot
```

196 s, of which most is the `dnf` transaction and the two boot waits. On a stock
Rocky server skip this stage; the installer's own host checks cover swap,
forwarding, firewall and SELinux.

### 2. Copy the tree

```bash
tar czf - --exclude .git --exclude local . \
  | ssh <target> 'cat > /tmp/umkim-thin.tgz'
ssh <target> 'rm -rf /tmp/umkim-thin && mkdir -p /tmp/umkim-thin \
  && tar xzf /tmp/umkim-thin.tgz -C /tmp/umkim-thin'
```

2 s for this checkout. The copy is only for the installer and the source of the
console image build; it is not a git clone.

### 3. Platform install

```bash
cd /tmp/umkim-thin
sudo ./install.sh --zone example.com --email admin@example.com
```

295 s. The installer logs each phase:

```text
==> preparing the Rocky Linux host
==> installing k3s
==> installing helm 3.16.3
==> generating directory, portal and console secrets (first run only)
==> packaging charts into /var/lib/umkim-thin/chart-mirror
==> building the console image (this can take a few minutes)
==> importing localhost/umkim-thin:novice into k3s
==> installing the identity stack (LLDAP + Authelia)
==> configuring Traefik (ACME + the identity middleware)
    traefik ports: 2222:30222 80:30080 443:30443
==> seeding the console state
==> installing the console
```

The `traefik ports:` line is not decoration: the installer waits for and
validates `80:30080` and `443:30443`, and fails with a clear message if the
service does not expose them. That is the check that would have caught the
router mismatch in the earlier run before it reached Let's Encrypt.

This stage also issues the first three certificates. Production ACME was used;
there is no staging CA and no `--skip-dns-check`.

### 4. Platform certificate check

```bash
EXPECTED_IP=203.0.113.10 ./ci/acme-check.sh example.com auth console lldap
```

Under a second, because the certificates were already valid when the installer
returned. All three had `Let's Encrypt` issuers. This check is **read-only**: it
resolves DNS and inspects the served certificate, but never asks ACME for
anything, so it cannot consume the failed-authorization or certificate rate
limits.

`lldap` is included deliberately. In an earlier manual run it was the host that
hit the rate limit because a plain-HTTP Ingress shadowed the challenge. On this
clean install the identity chart renders that Ingress only in external-proxy
mode, HTTP-01 succeeds, and `lldap` gets a real certificate.

### 5. Install all six apps

Apps were installed through the console's own CLI, the same code path the
Applications screen uses:

```bash
for a in nextcloud gitea immich paperless servarr wireguard; do
  kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
    /usr/local/bin/umkim-thin --catalog /opt/umkim-thin/catalog app install "$a"
done
```

756 s, cold image cache included. The installer prints the routes as each app
becomes ready:

```text
nextcloud  -> nextcloud.example.com, collabora.example.com
gitea      -> gitea.example.com
immich     -> immich.example.com
paperless  -> paperless.example.com
servarr    -> servarr.example.com, jellyfin/sonarr/radarr/prowlarr/qbittorrent
wireguard  -> (no HTTP route)
```

Most of the time is image pulls and the app bootstraps (Nextcloud, Gitea,
Immich, Paperless). The app Ingresses are created during this stage, so their
certificates are requested and issued here too; they were valid by the time the
loop finished. The Nextcloud bootstrap enables `richdocuments` and points it at
Collabora; the Gitea/Immich/Paperless bootstraps run as Jobs.

### 6. Verify apps and certificates

The stage checks every host:

```bash
for h in nextcloud collabora gitea immich paperless servarr \
         jellyfin sonarr radarr prowlarr qbittorrent; do
  curl -sS -o /dev/null -w "$h %{http_code}\n" "https://$h.example.com/"
done
EXPECTED_IP=203.0.113.10 ./ci/acme-check.sh example.com \
  auth console lldap nextcloud collabora gitea immich paperless \
  servarr jellyfin sonarr radarr prowlarr qbittorrent
```

12 s. Collabora returned 200; Nextcloud and the other app hosts returned 302
(the portal redirect), which is expected. All **14 hosts** passed the DNS +
issuer check with Let's Encrypt certificates. The Nextcloud, Gitea, Immich,
Paperless and LLDAP bootstrap Jobs are `Complete`; no pod is failed or stuck.

### 7. `tenant` — the last step

This is deliberately the final step: the platform and every app are already
working, and the only change is a normal household account.

1. In the console (`https://console.example.com`) → **Invitations** → create an
   invite for `tenant` in `default-tenant`.
2. Open the invite link (`https://auth.example.com/invite?token=…`), choose
   username `tenant`, display name `tenant`, email `tenant@example.com`, and a
   password of at least 8 characters.
3. Sign in at `https://auth.example.com` with that username and password.

6 s. The redeem returned `{"status":"created","username":"tenant"}`, and the
first-factor login returned 200. `tenant` is a regular user in
`default-tenant` — not `lldap_admin` and not the console admin. The console
admin remains `admin`, with the password printed by the installer and stored in
`/etc/umkim-thin/secrets.yml` (mode 0600).

## What was verified after the install

These checks were run after the timer, inside the VM, and are the state a user
gets on this clean install:

- **Paperless OIDC, full flow as `tenant`:** the allauth confirmation page,
  Authelia authorize redirect, callback, and final `https://paperless.example.com/`
  **200**. The production CA bundle is now the image's public roots
  (`/etc/ssl/combined/ca.crt`), so there is no more
  `/etc/ssl/umkim-thin/ca.crt` 500.
- **Nextcloud/Collabora WOPI:** `richdocuments` is enabled, the bootstrap
  fetched `/hosting/discovery`, and an unauthenticated
  `/index.php/apps/richdocuments/wopi/files/1?access_token=ci-probe` returns
  **403 with no redirect** — not the Authelia 401/portal redirect that caused
  “Unauthorized WOPI host.” Collabora's admin console returns **401**.
- **Immich:** the OIDC issuer is reachable with
  `NODE_EXTRA_CA_CERTS=/etc/ssl/combined/ca.crt`; the OAuth start returns the
  Authelia authorize URL with `client_id=immich`.
- **All 14 certificates** still pass `ci/acme-check.sh` after the apps and
  identity were installed.

## Validation that runs with the install

Each failure found during the manual runs now has a check:

| Failure | Guard |
|---|---|
| Traefik picked random NodePorts, so the router's 80/443 forward hit closed ports | `install.sh` pins `80:30080` / `443:30443` and `validate_traefik` fails the install if the service does not expose them |
| An LLDAP plain-HTTP Ingress shadowed `/.well-known/acme-challenge` | The identity chart renders the LLDAP HTTP Ingress **only** when the portal is explicitly on `web` (external-proxy mode); Tier 1 asserts it is absent in `websecure` mode |
| A hidden failure could burn the ACME failed-authorization limit | `ci/acme-check.sh` is read-only (DNS + issuer, no ACME requests); HTTP-01 is kept with `allowACMEByPass`; the installer does not loop or clear `acme.json` on failure |
| Paperless/Immich pointed the OIDC client at the lab CA, which is absent on a public install | Both charts build `/etc/ssl/combined/ca.crt` from the image roots plus the optional platform CA; Tier 1 renders both with OIDC on and asserts the combined path |
| Collabora's WOPI calls were answered by Authelia, so the editor showed “Unauthorized WOPI host” | The two `richdocuments/wopi` prefixes are in the Nextcloud `publicPaths`; `ci/apps.json` probes a token-bearing WOPI path and expects no auth redirect |
| A render/route regression could reintroduce any of these | `ci/static.sh` and `ci/novice-test.sh` assert the rendered Traefik config, identity HTTP-ingress condition, app CA bundles and catalog/chart versions |

## What this timing does not include

- DNS propagation and router configuration (done beforehand).
- The full browser DOCX editing session. The WOPI auth path is checked (403
  with no session, 200 with a valid token), but the editor is not scripted
  end-to-end in this timed run.
- Gitea's OIDC **first-login auto-registration**, which is still partial; LDAP
  and SSH are the tested path (`docs/auth.md`).
- Post-install app setup: the Servarr media bootstrap's app-side configuration
  and WireGuard client/port-forwarding. The timed target is “installed and the
  first household user can sign in.”
- Human decision time. The run was executed as a deliberate sequence; the
  numbers are wall clock, not a usability study. The 69 s verification
  re-check and 127 s hand-off/diagnostic gaps are included in the wall clock
  but are not install work.
- A warm image cache. All app images were pulled cold; a second run on the same
  host would be faster. Conversely, slower DNS, ACME or registry latency makes
  the app stage longer.

## Reproduce

```bash
# 0. clean VM (lab): revert, don't uninstall in place
sudo virsh snapshot-revert single-node base --running

# 1. lab base image only
sudo dnf -y install kernel kernel-modules iptables iptables-nft conntrack-tools
sudo reboot

# 2. copy the checkout, then:
cd /tmp/umkim-thin
sudo ./install.sh --zone example.com --email admin@example.com

# 3. read-only certificate check (safe to repeat)
EXPECTED_IP=203.0.113.10 ./ci/acme-check.sh example.com auth console lldap

# 4. all six apps through the console CLI
for a in nextcloud gitea immich paperless servarr wireguard; do
  kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
    /usr/local/bin/umkim-thin --catalog /opt/umkim-thin/catalog app install "$a"
done

# 5. invite tenant in the console, redeem at
#    https://auth.example.com/invite?token=... , then sign in once.
```

On a real public host without the lab's stripped image, skip step 1. Internal
images are always built locally, so no prebuilt console image is used.
