# Catalog format

One YAML file per app under `catalog/`. An entry is everything needed to install
the app, expose it, protect it, and *verify* it. The proposed model for an
official catalog plus private/community sources is in
[`catalog-sources.md`](catalog-sources.md).

## Example

```yaml
name: nextcloud
displayName: Nextcloud
description: Files, shares and collaboration, with Collabora for editing.

license:
  chart: Apache-2.0
  application: AGPL-3.0
  url: https://github.com/nextcloud/server/blob/master/COPYING

chart:
  # An OCI reference to the chart's own repository, pinned by version.
  ref: oci://ghcr.io/example/charts/nextcloud
  version: 0.1.17
  versions:
    - version: 0.1.17
      appVersion: 35.0.1
      channel: stable
    - version: 0.1.18
      appVersion: 35.0.2
      channel: candidate

namespace: nextcloud

route:
  mode: site                 # site (omit the route for no HTTP surface)
  host: "nextcloud.{{ .zone }}"
  requireAuth: true
  exposure:
    default: authelia
    allowed: [public, authelia, vpn]
  certificate:
    default: acme
    allowed: [acme, default, secret]
  # Paths that must stay reachable without a session. Share links are
  # token-authenticated by the app itself, so forward-auth must not intercept
  # them, and the request must not be redirected to the portal. For a
  # single-page app, also exempt its static assets and public share API; the
  # app still enforces its own share key/password on those endpoints.
  publicPaths:
    - "^/s/"
    - "^/index\\.php/s/"
    - "^/public\\.php"

auth:
  kind: forward_auth         # forward_auth | oidc | none
  # forward_auth: Authelia challenges the browser session.
  # oidc: the app authenticates against Authelia itself (see `oidc`).

values:
  # Templated with {{ .zone }}, {{ .release }}, {{ .namespace }}.
  nextcloud:
    externalUrl: "https://nextcloud.{{ .zone }}"

verify:
  # What "tested" means for this entry. Public, and run in the lab.
  playwright: tests/playwright/nextcloud.spec.ts
  checks:
    - route_reachable          # the host answers
    - auth_challenge           # an unauthenticated request is challenged
    - public_path_exempt       # /s/ is *not* challenged
    - logout_clears_session
```

## Fields

| Field | Required | Notes |
|---|---|---|
| `name` | yes | catalog key; also the default release name |
| `chart.ref`, `chart.version` | yes | pinned; the catalog never floats |
| `chart.versions` | no | tested chart/app pairs and `stable`/`candidate` channels |
| `route.exposure` | no | `default`/`allowed`: `public`, `authelia`, `vpn`; `allowLocal` for VPN |
| `route.certificate` | no | `default`/`allowed`: `acme`, `default`, `secret:<name>` |
| `route.vpnHost` | no | optional VPN hostname; derived as `<host>.vpn.<zone>` when omitted |
| `namespace` | yes | created if absent |
| `license.chart` | yes | licence of the Helm chart itself |
| `license.application` | yes | licence of the deployed application; use a short mixed-stack expression where needed |
| `license.url` | no | authoritative upstream licence text or notice |
| `license.note` | no | short warning, e.g. a non-commercial restriction |
| `route.mode` | yes | `site` publishes it; omit the route for no HTTP surface |
| `route.host` | when `site` | supports `{{ .zone }}` |
| `route.requireAuth` | yes | whether Authelia is in front |
| `route.publicPaths` | no | anchored regexes exempted from auth |
| `auth.kind` | yes | `forward_auth`, `oidc`, or `none` |
| `oidc.*` | when `oidc` | client id, redirect URIs and optional scopes; the secret is generated |
| `values` | no | chart values, templated |
| `verify` | yes | the suite that must pass for the entry to ship |

## Rules

1. **Pin everything.** An entry names an exact chart version. Upgrades are a new
   entry revision, reviewed like code.
2. **Charts live in their own repos.** `chart.ref` is an OCI URL
   (`oci://ghcr.io/<owner>/charts/<name>`). A fork changes the ref to its own
   registry, or packages the chart into the local mirror; the catalog pins the
   version, not the location.
3. **Auth is explicit.** An app is either forward-authed, OIDC, or deliberately
   public. There is no implicit default.
4. **Exposure is explicit.** `public`, `authelia`, or `vpn`; `public` is
   opt-in per entry, `vpn` is the most restricted browser route, and
   `cluster-only` is no longer a mode (apps with no HTTP surface omit the route).
5. **Certificates are explicit.** ACME is the default; `default` uses the
   manual wildcard TLS store; `secret:<name>` uses a TLS Secret in the app
   namespace.
6. **Public paths are anchored and narrow.** `^/s/`, never `/s` — an unanchored
   pattern silently exempts more than intended.
7. **Every entry has a verification suite.** An app without passing tests is not
   in the catalog. `verify.checks` names the assertions; the Playwright spec
   implements them.
8. **Licences are explicit and shown.** Every entry records both the chart
   licence (`license.chart`, normally Apache-2.0) and the deployed application
   licence (`license.application`). The console shows both, and the latter is
   the one to read before installing. Source-available or non-commercial terms
   belong in `license.note`, not only in prose.
9. **Native clients are not forward-authed.** An app whose mobile/desktop client
   cannot complete a browser login (Jellyfin, for one) gets `oidc` or a
   route with `exposure: vpn`, not a path exemption list.

## Multi-route apps

An app that publishes more than one service lists them instead of a single
route. Each key becomes its own route with its own host:

```yaml
routes:
  - key: seerr
    host: "servarr.{{ .zone }}"
    requireAuth: true
    serviceName: seerr
    servicePort: 80
  - key: jellyfin
    host: "jellyfin.{{ .zone }}"
    requireAuth: true
    serviceName: jellyfin
    servicePort: 80
```

## Templating

`values` and hosts are rendered with a small context, so an entry does not
hardcode a cluster's choices:

| Variable | Meaning |
|---|---|
| `{{ .zone }}` | the configured public zone |
| `{{ .host }}` | the app's primary route host (the install's hostname override, or the site default) |
| `{{ .release }}` / `{{ .namespace }}` | the app's release and namespace |
| `{{ .storage.configClass }}` | the ReadWriteOnce class for databases and config |
| `{{ .storage.mediaClass }}` | the ReadWriteMany class for shared media |
| `{{ .storage.mediaAccessMode }}` | the media PVC access mode (`ReadWriteMany`, or `ReadWriteOnce` on a single node) |

Catalog entries that create persistent volumes must wire these classes into the
chart values; the chart defaults are standalone examples, and relying on the
cluster default puts media on Longhorn in HA. The current entries bind
`persistence` (or the chart's equivalent) explicitly.
| `{{ .cluster.serviceCIDR }}` / `{{ .cluster.podCIDR }}` | cluster networks |
| `{{ .cluster.dnsServiceIP }}` | the cluster DNS service address |
