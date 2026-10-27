# identity

LLDAP directory + Authelia forward-auth, OIDC and invitations for the apps in
the cluster.

- LLDAP (LDAP directory) is deployed as its own container.
- Authelia provides the portal, forward-auth middleware, OIDC provider and
  session/regulation policy.
- Valkey stores Authelia sessions, so an Authelia restart for an app's OIDC or
  public-path change does not sign every portal user out.
- nginx serves the public invitation page and proxies redemption to the console.

## License

The **chart source** is Apache-2.0, part of umkim-thin.

The deployed components keep their upstream licenses:

| Component | License |
|---|---|
| LLDAP | GPL-3.0 |
| Authelia | Apache-2.0 |
| Valkey | BSD-3-Clause |
| nginx | BSD-2-Clause |

The chart renders Kubernetes objects; it does not vendor or relicense LLDAP,
Authelia, Valkey or nginx. See [`../../docs/licenses.md`](../../docs/licenses.md) and
[`../../NOTICE`](../../NOTICE).

## Values

The chart refuses to render without `lldap.baseDN`, `lldap.adminPassword`,
`lldap.jwtSecret`, an Authelia portal host and the console endpoint used for
invite redemption. The installer renders those values from the cluster's site
variables; do not hand-write secrets into a values file in Git.
