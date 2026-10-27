# Security policy

umkim-thin is **alpha** software. It is a cluster-admin-adjacent component: the
console installs charts and reads cluster state. Treat it like an admin tool, not
like a multi-tenant service.

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Use GitHub's private vulnerability reporting for this repository
(**Security → Report a vulnerability**). Include:

- the version, commit, and install path (`install.sh` or the Ansible path);
- what an attacker can do, and the preconditions;
- a minimal reproduction (commands or a redacted log);
- whether the issue is in this repository or in a deployed third-party app.

Do not include live passwords, invite links, WireGuard configs, session cookies,
or `/etc/umkim-thin/secrets.yml`. If a secret was exposed, rotate it first and say
that you did.

This is a best-effort, community project; there is no paid support or bug bounty.
We will acknowledge a report, agree on a disclosure window, and credit you unless
you ask us not to.

## Supported versions

| Version | Supported |
|---|---|
| `0.1.x` (alpha) | yes, latest patch |
| `main` | yes, development |
| older tags | no |

Only the latest release and `main` are supported. There are no backports yet.
The manual e2e and the Tier 1 checks are the current gate; see `ci/README.md`.
A release is only cut after `./ci/static.sh` and the single/HA e2e pass.

## Scope

In scope:

- the console/API/CLI (`cmd/`, `internal/`);
- the platform charts (`charts/identity`, `charts/umkim-thin`,
  `charts/wireguard-gateway`);
- the installer (`install.sh`, `deploy/`) and the quickstart;
- the catalog format and the vendored app charts' auth/route wiring.

Out of scope (report upstream):

- vulnerabilities in LLDAP, Authelia, Traefik, Kubernetes, or a catalog app;
- a misconfiguration an operator made despite the quickstart (for example,
  exposing the k3s API or port 2222);
- the maintainer lab infrastructure and its keys.

The current threat model, known limits, and open hardening work are in
[`docs/security-plan.md`](docs/security-plan.md) and
[`docs/security-followup.md`](docs/security-followup.md). Read them before
exposing an install to the internet.

## Hardening expectations

- The console is single-admin today; invitations create directory users, not
  console users.
- The console ships behind Authelia, but its own login and the `localadmin`
  breakglass account exist. Keep both strong; register a second factor.
- Application data protection is the operator's job: `local-path` has no
  snapshots. See `docs/quickstart/06-backup.md`.
- The optional WireGuard gateway needs `NET_ADMIN`; the optional builder needs
  the node's containerd socket. Both are documented as privileged-ish.
