# Design

## Scope

umkim-thin turns an existing Kubernetes cluster into a self-hosted app platform:

1. **Install once** — the supported Ansible path prepares a single Rocky/RHEL
   host or a three-node HA cluster; one install deploys the identity stack
   (LLDAP + Authelia), the console/API, and the Traefik wiring.
2. **Install apps** — pick from the catalog. Each entry is a pinned chart plus
   the route, auth mode and values it needs. Installing an app creates its
   namespace, its route, its auth wiring and, where the app speaks OIDC, its
   client.
3. **Onboard people** — an operator issues an invitation; the invitee sets their
   own password in the directory. No shared credentials, no manual LDAP edits.
4. **Use it** — household users open `home.<zone>`, a read-only app launcher
   with availability and links, without getting the admin console.
5. **Reach it privately (optional)** — create WireGuard clients self-service and
   keep the apps off the public internet, or publish them with auth in front.

## Non-goals

Explicitly out of scope, because they are what makes umkim a fleet manager:

- fleet lifecycle: multi-cluster placement, managed upgrades, deletion and
  node replacement (the single/HA Ansible path is provisioning, not a fleet
  controller)
- multi-cluster or multi-site topologies, hub/spoke, OCM
- a registry (Harbor) or image mirroring
- more than one directory per cluster, or per-tenant directories
- an app-placement engine

## Architecture

```
                    ┌──────────── browser ────────────┐
                    │                                 │
             public │                                 │ WireGuard
                    ▼                                 ▼
            ┌───────────────┐                 ┌───────────────┐
            │ Traefik       │◄────────────────│ wg gateway    │
            │ (platform)    │                 │ (optional)    │
            └───────┬───────┘                 └───────────────┘
                    │ route + middleware
        ┌───────────┴───────────┐
        ▼                       ▼
┌───────────────┐       ┌───────────────┐
│ Authelia      │       │ app           │
│ forward-auth  │       │ (catalog)     │
└───────┬───────┘       └───────────────┘
        │ LDAP
        ▼
┌───────────────┐
│ LLDAP         │◄──── console / CLI / invites
└───────────────┘
```

- **Identity** lives in `charts/identity`: LLDAP, Authelia, the Traefik
  `forwardAuth` middleware, the invite sign-up page, and the bind account apps
  use to read the directory.
- **Routes** are plain Kubernetes Ingresses on Traefik, carrying a middleware
  annotation. Auth is a property of the route, not of the app.
- **Catalog** entries describe the app; the installer turns an entry into
  resources. See [catalog.md](catalog.md).
- **Console/API/CLI** are the operator surface: apps, invites, WireGuard
  clients, and the directory.

## Relationship to umkim

umkim-thin is not a fork of umkim's controllers. It is the same *ideas* at a
much smaller scope:

| Concern | umkim | umkim-thin |
|---|---|---|
| Cluster | provisioned and managed | pre-existing, or a supported single/HA install; no fleet lifecycle |
| Identity | per-cluster or shared hub directory | one directory, in-cluster |
| Routing | hub + site routes across clusters | site routes on one cluster |
| Apps | FleetApp on a named cluster | catalog entry on this cluster |
| Catalog | chart + route metadata | chart + route + auth + **verification** |

The parts worth sharing, and the only ones that should be shared:

1. **The identity chart** — same LLDAP + Authelia + middleware + invite page.
2. **The catalog format** — so an entry means the same thing in both.
3. **The invite/tenant semantics** — an invite always creates a *new* account;
   it never takes over an existing one.

Everything else is intentionally separate, because the thin product can drop
the machinery the fleet needs (OCM, hub/spoke, placement, ACME coordination).

**Avoid copy-paste drift.** Two repos with the same chart will diverge. The
chart and the catalog format are the contract: either vendor them from a single
source with a sync script, or publish them as their own small artifact that both
products consume.

## Decisions

| Decision | Choice |
|---|---|
| License | Apache-2.0. LLDAP is deployed, never linked — see `NOTICE` and `docs/licenses.md` |
| Stack | one small Go binary serving the API, a compact UI and the CLI |
| Ingress | Traefik only (the middleware, file-provider fragment and route rendering are Traefik-specific) |
| App charts | the `umkim-charts` repository, pinned as a `charts/apps` submodule and by OCI version |
| WireGuard | optional, and a **workload in the cluster** — see below |
| Catalog | the apps verified on the lab cluster, then grow; sources are proposed in `docs/catalog-sources.md` |

### WireGuard as an in-cluster workload

Running the gateway as a pod rather than on the host is the right shape here:

- It installs and uninstalls like any other catalog entry, and needs no changes
  to the host — which matters on managed clusters.
- The pod needs `NET_ADMIN`, and `hostNetwork: true` keeps the UDP path simple:
  the pod binds the node's port, and the node already routes cluster IPs, so
  peers reach services and the cluster DNS without extra routing.
- Clients are managed by the API writing a peer Secret that the gateway reloads,
  so `wg client create` is self-service.

The honest caveats, which the entry documents:

- **UDP does not go through an HTTP ingress.** The operator forwards the port to
  the cluster (a NodePort on a node, or a UDP-capable load balancer). That is
  the one manual step, and it is a prerequisite, not a bug.
- **Failover, not seamless HA.** The gateway is a per-node DaemonSet and the
  VIP provides failover. A node loss interrupts existing sessions; clients
  re-handshake on the new VIP holder.
- **It is privileged-ish.** `NET_ADMIN` is required; treat the gateway like any
  other privileged workload and keep it optional.
