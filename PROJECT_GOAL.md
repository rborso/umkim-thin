# Project goal

This is the *why* and the *who*. For the *how*, see
[`docs/design.md`](docs/design.md); for the auth model,
[`docs/auth.md`](docs/auth.md).

## The problem

Installing one self-hosted app is easy. Sharing a stack is not.

The moment more than one person uses it, the work multiplies: every app grows
its own account, people reuse one password across services (or write it on a
note), and adding or removing someone means hand-editing LDAP and a proxy config
in four places. Then the disk fills, a certificate expires, or the box dies — and
the person who set it up is now on call for the household, with no tested restore
path.

Docker Compose is excellent at the first step and painful at the second:
stateful high availability means teaching every app to replicate itself, or
hand-building failover around volumes, and there is no scheduler to put things
back where they belong.

## Who it is for

The **household admin**: someone technical enough to run a server, who is not a
platform team and does not want to become one. They are setting this up for
**2–20 people** — a family, flatmates, a club, a small team — and they want to
hand out access without becoming the sysadmin for every app.

Not the target:

- a solo tinkerer who is happy with Compose — Compose is genuinely better there;
- a platform team that wants GitOps, Argo and policy-as-code rather than a UI;
- a hostile multi-tenant environment (per-tenant directories, untrusted admins).

## The goal

**One non-platform-engineer should be able to go from a fresh server to a
private app platform the whole household uses — one account, install apps in a
click, and keep it running — without becoming a Kubernetes expert.**

"Keep it running" includes the unglamorous part: back up, restore, upgrade, and
survive a lost node.

## Topologies

Both are supported, and both are gated by the same assertions — a change that
only works on one is not done.

- **Single node** — the default path a household admin should take: one
  Rocky Linux host, k3s, local-path storage, the host firewall. Simplest to
  run, and fully covered by the app/auth gate.
- **HA** — three nodes, Longhorn for RWO config/database volumes, an optional
  external NFS export for shared media, and a control-plane VIP. For people who
  already have the hardware. HA is an option, not a requirement to start.

The single-node path is allowed to be simpler; it is never a second-class
product. Same charts, same catalog, same auth gate, and any real difference is
documented rather than discovered.

## The promise

What a user should be able to say when it is working:

- **One account.** One directory account and one password for every app.
  Browser SSO where the app speaks OIDC; where it does not, the app's own login
  accepts the same directory credentials. A second login screen is acceptable;
  a second password is not.
- **One command, one click.** A single supported install path, then an app
  install from the console or CLI — no YAML editing to add a route or auth.
- **Curated, not a dump.** Every catalog entry is pinned, wired for auth, and
  verified end to end. "It is in the catalog" means "it was tested", not "the
  manifest exists".
- **People, not shared credentials.** Invite by link; the invitee sets their own
  password. The operator can reset a password or remove a person.
- **A family surface, not an admin console.** The household sees the apps that
  are for them; admin tools keep their own login and are not part of that
  surface. Trusted members are not fenced off from each other's apps — the
  directory account is the boundary, not a per-app ACL.
- **Yours.** Data on your disk, TLS on your domain, backups you can restore, no
  subscription, no vendor account.
- **Reliable when it matters.** HA when you have the hardware; a documented,
  tested restore when you do not. Availability is a backup story first and a
  failover story second.

## Scope decisions

| Decision | Choice |
|---|---|
| Login model | **One account, not necessarily one prompt.** A second login screen that takes the directory credentials is fine; an app-local account is not. |
| Topologies | Single node and HA are both supported and both in the test gate. |
| Admin expertise | The admin brings DNS, ports/firewall, offsite backup and upgrades. Everything above that floor is the product's job. |
| Console | A single trusted admin console, never a household control plane. |
| Trust model | Everyone with a directory account is trusted with the family apps. No per-app ACLs; each app's own login is the second gate. |
| Catalog | A family-facing app must authenticate against the directory. An admin tool may keep its own login and stay off the family surface. |

## Design bets, and what they cost

1. **Kubernetes, because stateful HA is the real gap.** Replicated volumes
   (Longhorn), a scheduler and self-healing replace per-app replication and
   hand-written failover. The cost is a cluster to operate — acceptable only if
   the installer, upgrades and backup do the operating.
2. **One directory, one policy point.** LLDAP + Authelia is what keeps one
   identity true as apps are added: identity is central, auth is a property of
   the route. The cost: an app that speaks neither OIDC nor LDAP is admin-only.
3. **A curated catalog over a template marketplace.** The catalog format forces
   route, auth mode, public paths and a verification suite to be declared, so an
   entry means the same thing everywhere. The cost is breadth; breadth has to
   come from contributors following the same contract.
4. **One admin console, not multi-tenant RBAC.** The operator is trusted; the
   household is not given a control plane. The cost: not a fit for hostile
   multi-tenancy, and it should stay that way.

## Who does what

"User-friendly" has a concrete meaning here: **the admin never has to read a
Traefik middleware or an LLDAP GraphQL call to finish a task, and an error
always says what to do next.**

The admin is expected to bring:

- a domain, and the DNS records that point at the install;
- a host (or three), the ports, and any port forwarding;
- offsite storage for backups, and the discipline to run upgrades on a schedule;
- occasional directory operations — a forgotten password, removing a person —
  done from a directory UI rather than by editing a file.

That floor is real and it is not going away — this is a server, not an appliance.
Everything above it is the product's job:

- identity, routes, auth, public paths, OIDC/LDAP client wiring;
- invites, plus a working directory UI for the occasional reset or removal;
- app install/uninstall with health checks and readable failures;
- backup and a tested restore path;
- a doctor that diagnoses the install instead of a log the admin has to decode.

The console should never expose a Kubernetes concept the admin cannot act on. If
a screen needs a paragraph of explanation, it is the wrong screen.

## What success looks like

- A person who is not the author reaches a working install on either topology,
  and an invited family member is inside an app, in under 30 minutes using only
  [`docs/quickstart/`](docs/quickstart/).
- Every catalog entry passes the route, auth-challenge, public-path and
  real-directory-login checks in the lab gate before it is accepted.
- Restoring from backup onto a fresh host is written down and exercised;
  upgrading from the previous release is part of the gate.
- Every family-facing app authenticates against the directory: a second login
  screen is fine, an app-local account is not.
- Single node and HA pass the same gate; a difference is documented, not
  discovered.
- Removing a person means deleting their directory account in one place; apps
  that authenticate through it stop letting them in.
- A forgotten password has a documented manual path that does not require
  reading Kubernetes logs.

## Non-goals

- cluster lifecycle, multi-cluster fleets, hub/spoke, OCM, Harbor;
- more than one directory per cluster, or per-tenant directories;
- an unbounded app marketplace, or running unverified manifests;
- per-user or per-app RBAC beyond the directory account itself;
- production SLAs and hostile multi-tenancy;
- rebuilding upstream images — apps are pulled at pinned versions;
- replacing Compose for a single technical user.

## Where we are

Pre-1.0, working end to end on the project's single-node and HA lab. Both
topologies pass the same gate (42/42 acceptance checks and 18/18 functional
user journeys on each, plus the platform conformance checks); a clean-VM single-node install of the
platform, the six apps used at the time and the first ordinary user was measured at **22 m 28 s**
before the app fixes and **24 m 36 s** after them (21 m 20 s of install stages;
the rest is verification and hand-off) — see
[`docs/manual-install-timed.md`](docs/manual-install-timed.md); the lab base
image still needs a stock
kernel and iptables, which the Ansible `common` role now installs and reboots
into; the restore drill is documented but not yet exercised. Internal images
(the console and the optional Servarr bootstrap) are built locally by the
installer/console; they are not prebuilt or published. App charts are consumed
from the `umkim-charts` submodule and packaged into the local chart mirror.

## The pitch

> A private app server for your household: one account, your own domain,
> install apps in a click, and a backup you can actually restore.
