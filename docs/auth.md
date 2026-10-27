# Authentication: portal, OIDC, LDAP, and Gitea SSH

## Layers

| Layer | What it does |
|---|---|
| **Authelia portal** | The SSO session and the OIDC provider (`https://auth.<zone>`) |
| **Forward-auth** | Traefik asks Authelia before proxying. It gates a route but does **not** pass an identity to the app |
| **App identity** | The app's own session: OIDC, LDAP, or local |

Forward-auth alone never removes an app's login; the app must support OIDC or
trust a proxy identity header.

## Current matrix

| App | App-level auth | Only an Authelia prompt? |
|---|---|---|
| Immich | OIDC (Authelia client) | **yes** |
| Paperless | OIDC (django-allauth → Authelia) | **yes** |
| SparkyFitness | OIDC (Authelia client) | **yes** |
| Actual Budget | OIDC (Authelia client) | **yes** |
| HomeBox | OIDC (Authelia client) | **yes** |
| Gitea | OIDC source **configured**; LDAP also enabled | not yet reliable for new users |
| Nextcloud | LDAP (`user_ldap`) | no; `user_oidc` is disabled (see below) |
| Jellyfin | LDAP plugin | no (native clients need direct auth) |
| Sonarr / Radarr / Prowlarr | local forms | no |
| qBittorrent | local WebUI password | no |
| Seerr | Jellyfin login (LDAP-backed) | no |
| Console | own admin/breakglass login | no (deliberate) |
| WireGuard | keys | n/a |

## Gitea SSH (dedicated port)

- Traefik has a **`gitssh` TCP entry point**: service port **2222**, NodePort
  **30222**. The Gitea chart publishes an `IngressRouteTCP` to the Gitea SSH
  Service (2222).
- Gitea advertises `ssh://git@gitea.<zone>:2222/<owner>/<repo>.git`
  (`gitea.ssh.advertisedPort`/`domain`).
- The single-node firewall opens **2222** and **30222**; the HA firewalld is
  off, and the VIP carries the service port.
- Both routers forward **2222** to the target.
- The gating Playwright suite adds a real SSH key to the Gitea admin, does a
  `git clone` **and** a `git push` over `ssh://git@<host>:2222`, and verifies
  the file through the API. On single the host is the public
  `gitea.test.example.com:2222` (through the router); on HA the runner pins the
  host to the VIP, so it validates the cluster port (the public router path is
  external and cannot be tested from the lab without hairpin).
- Authentication is public-key only; add the key in Gitea (or sync an
  `sshPublicKey` LDAP attribute). Authelia is not involved in SSH.

## OIDC status and limits

**Working:** Immich, Paperless, SparkyFitness, Actual Budget and HomeBox use
real OIDC clients against Authelia; one portal login, then the app signs itself
in. SparkyFitness registers its provider from the `SPARKY_FITNESS_OIDC_*`
environment on startup; the chart checks it in `/api/auth/settings`. Its admin
panel is granted to the account whose email matches
`SPARKY_FITNESS_ADMIN_EMAIL` (the catalog uses the directory admin address).

Actual Budget uses the OAuth2 flow and `/openid/callback`; the first OIDC login
owns the server. HomeBox uses `/api/v1/users/login/oidc/callback` and creates a
personal group on first OIDC login. Both are behind the portal by default, but
Actual's native clients need either the public or VPN exposure because the
Authelia forward-auth is browser-only.

**Gitea (partial):** the installer creates an Authelia client, the chart creates
an `openidConnect` auth source (discovery URL, scopes), and the login page shows
the provider. Gitea's OAuth2 **auto-registration for a brand-new directory user
does not complete** in this setup (the global `ENABLE_AUTO_REGISTRATION`,
`ALLOW_ONLY_EXTERNAL_REGISTRATION`, and a non-disabled registration form were
tried; the source-level `AutoRegister` is not exposed by the CLI and needs the
admin auth-source API/UI). The gate therefore keeps the Gitea **LDAP** and
**SSH** checks, not an OIDC login.

**Nextcloud (not available):** the chart has an optional `user_oidc` mode
(provider, combined CA bundle), but on Nextcloud 35 the app's
`/apps/user_oidc/login/<id>` route returns 404 from the web even though
`occ router:match` resolves it, and the discovery rejects the portal
certificate (`cURL error 60`) although a PHP cURL with the same CA bundle
succeeds. The catalog is back to `forward_auth` + LDAP and the optional chart
mode stays disabled; the HA `user_oidc` app was disabled. Do not advertise
Nextcloud OIDC until that app-side path is fixed.

## What "one login" would require

| App | Path | Caveat |
|---|---|---|
| Gitea | OIDC source with auto-registration (admin auth-source API), or a trusted `Remote-User` header | git clients still use keys/tokens |
| Nextcloud | `user_oidc` with a working app route and CA, or SAML | desktop/mobile clients use app passwords |
| Jellyfin | third-party SSO plugin for the web | native clients cannot use forward-auth/OIDC |
| Sonarr/Radarr/Prowlarr | `AuthenticationMethod=External` + a proxy-set identity header | not OIDC; single-user; strip client headers |
| qBittorrent | none | no OIDC/header auth; API/download clients need its password |
| Seerr | Jellyfin login (already directory-backed) | no generic OIDC in this stack |
