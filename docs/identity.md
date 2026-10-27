# Identity model

## One directory per cluster

umkim-thin deploys **one LLDAP** in the cluster and treats it as the directory
for everyone who uses the apps on it. That is the homelab case: one household,
one directory, one tenant.

This is a deliberate simplification of umkim, which can give every cluster its
own directory or bind them all to a shared hub directory. umkim-thin drops that
axis entirely — if you need more than one directory, you want umkim.

## Two ways an app can be protected

**Forward-auth (default).** Authelia sits in front of the route as a Traefik
`forwardAuth` middleware. The browser is challenged, and Authelia answers from
its own session. The app does not know about any of it — it just sees a request
with `Remote-User` / `Remote-Groups` headers.

Use it for: web apps with no identity of their own (a dashboard, a service UI).

**OIDC.** Authelia is the identity provider and the app does the login itself,
so the app knows who the user is and can map them to its own account model.

Use it for: apps that speak OIDC and benefit from real users inside the app
(Immich, Paperless, Gitea), and for apps whose native clients cannot complete a
browser-based forward-auth flow (Jellyfin).

## Sessions

Authelia keeps its portal sessions in the identity chart's **Valkey** service.
The store is deliberately in-cluster and ephemeral: an Authelia restart (for
example when an app install changes OIDC clients or public paths) does not sign
everyone out, while a Valkey restart does. The installer reuses
`secret_authelia_session` as the internal Valkey password; chart-only installs
can set `valkey.password` themselves.

## Public paths

Some apps are *meant* to be reachable without a session — a Nextcloud share
link, an Immich shared album. Those paths are declared per app in the catalog
and exempted from auth:

```yaml
route:
  requireAuth: true
  publicPaths:
    - "^/s/"
    - "^/index\\.php/s/"
```

Two things to get right:

- **Anchor the patterns.** `^/s/` exempts the share prefix; `/s` would also
  exempt `/settings`.
- **Prefer token-authenticated surfaces.** Share links carry an unguessable
  token and have their own password/expiry controls. Exempting "the login
  endpoint" instead does not work for native clients — the app breaks right
  after login.

For an app that is public by design, the honest configuration is a separate
hostname with `auth.kind: none`, not a growing exemption list.

## Invitations

An invitation creates a **new** account in the directory:

1. An operator creates an invite (`umkim-thin invite create --email …`). The
   invite is a single-use token with an expiry.
2. The invitee opens the link, picks a username and password.
3. The redeem call creates the account in LLDAP. It **refuses an existing
   username** — an invite never takes over an account, and never resets a
   password.

Adopting an existing user into a group is a separate, deliberate operation, not
a side effect of an invitation.

## WireGuard (optional)

WireGuard is a *transport*, not an identity: a client that connects over it
still goes through the same routes and the same Authelia. It exists so that an
operator can keep apps off the public internet entirely and still reach them
from a laptop or phone.

It is a **workload in the cluster**, installed from the catalog like any app —
not a host-level daemon. That means it installs and uninstalls cleanly, works on
managed clusters, and needs no changes to the machine running Kubernetes.

- The gateway runs with `NET_ADMIN` and `hostNetwork: true`, so it binds the
  node's UDP port and the node routes cluster IPs to it.
- Clients are created self-service (`umkim-thin wg client create laptop`), each
  with its own keypair; the private key is shown once.
- Peer addresses are pinned per client, so adding a client never renumbers the
  existing ones.
- Peers are stored in a Secret the gateway reloads; the API writes it, so no
  manual config editing.

Two things to know before enabling it:

- **The UDP port must be forwarded to the cluster** (a NodePort on a node, or a
  UDP-capable load balancer). HTTP ingress cannot carry WireGuard.
- **It runs as a per-node DaemonSet for failover.** The VIP holder already has
  a local listener; moving the VIP interrupts existing sessions and clients
  re-handshake.

The gateway is optional: a cluster can publish everything publicly, use
WireGuard only, or both. With it, apps can use `exposure: vpn` and be
reachable *only* over the overlay (or the VPN plus the local LAN with
`allowLocal`).
