# 4. Install your first app

**Goal:** install Nextcloud, open it from the internet, and confirm that auth is
in front of it.

## Install

1. Open the console → **Catalog**.
2. Find **Nextcloud** and click **Install**.
3. The form offers:
   - **Host**: `nextcloud.<zone>` by default. Leave it unless you have a reason.
   - **Config storage class**: `local-path` on the `install.sh` single-node
     path, `umkim-thin-nfs` on the Ansible single path, and `longhorn-rwo` on
     HA.
   - **Media storage class**: `local-path` on the `install.sh` path,
     `umkim-thin-nfs` on the Ansible single path, and `longhorn-rwx` on HA until
     an external NFS class is added. The ReadWriteMany warning is expected on
     `local-path`; a single node can run media on RWO, but an HA cluster needs
     NFS or Longhorn RWX.
   - **Media access mode**: follows the selected class (`ReadWriteMany` for an
     RWX NFS/Longhorn class, `ReadWriteOnce` for `local-path`).
   - To keep media on a separate host, create the export first and select its
     `nfs-<name>` class ([Use an NFS server on another host](12-external-nfs.md)).
4. Click **Install** and keep the page open. The job shows Helm's progress.
5. When the app is ready, **Applications** shows a green status and a link.

Installing does four things:

- creates the app's namespace and PVCs;
- installs the pinned chart with the catalog's values;
- publishes the route as an Ingress on Traefik with the Authelia middleware;
- adds the app's OIDC client and public-path rules to the identity release (if
  the entry needs them).

## Open it

Open `https://nextcloud.<zone>`.

You should see the Authelia login first (unless you already have a portal
session), then Nextcloud's own page. Nextcloud uses the directory over LDAP, so
a user you invited can sign in with their directory password.

### The app's own admin account

The Nextcloud chart creates a separate admin account, `nc-admin`, with a
generated password. Normal use does not need it; admin settings do. Read it from
the app's Secret:

```bash
sudo kubectl -n nextcloud get secret nextcloud-secrets \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

Treat that as a root credential for the app. Store it in your password manager,
and do not put it in a ticket or chat.

## Check that auth is really on

Open the app in a private browser window. You should be redirected to the
portal, not shown the app. Then:

- `https://nextcloud.<zone>/s/<a-share-token>` must work without a portal
  session (that is the declared public path for share links).
- The certificate must be valid (padlock, no warning). If it is not, see
  [Troubleshooting](08-troubleshooting.md#certificates).

The console's **Applications** section lists the published route and the
workload status. It does not prove an HTTP request reached the app; the
in-cluster verification suite does that in the maintainer test lab.

## Install another app

The same flow works for the other catalog entries. Before installing:

- read the entry's **auth** model (forward-auth, OIDC, LDAP, or local);
- check the storage and resource needs;
- check the hostname: the default is `<app>.<zone>`.

The app charts are pinned through the `charts/apps/` submodule, so the
single-node path does not need a private registry. The catalog still records the
canonical OCI reference; the installer resolves the local package by name and
version. Clone with `--recurse-submodules`, or run
`git submodule update --init --recursive` before installing.

## Next

[Optional: WireGuard →](05-wireguard.md)
