# 7. Upgrade safely

**Goal:** move to a new version without losing data or locking yourself out.

## Before you upgrade

1. Run a backup:

   ```bash
   sudo ./install.sh --backup
   ```

2. Take a VM snapshot if you can. Application data lives in PVCs (see
   [Backup](06-backup.md)).
3. Read the release notes for breaking changes
   ([`../releases/`](../releases/)). App charts and the platform charts version
   independently.

## Upgrade the platform

From the checkout:

```bash
git pull
sudo ./install.sh --zone example.com --email you@example.com --reuse-k3s
```

Use the **same flags** as the first install, plus `--reuse-k3s`: k3s is already
installed, and without that flag the installer refuses to touch it. The same
applies when resuming after a failed install that already reached k3s.

The installer builds the console
image locally by default; the installer:

- keeps `/etc/umkim-thin/secrets.yml` and every existing secret;
- preserves the console-managed OIDC clients by copying
  `/data/identity-apps.yml` from the console PVC before upgrading identity;
- re-renders the values and re-packages the charts;
- runs `helm upgrade --install` for the identity stack and the console;
- waits for the rollouts.

If you have an already-built console image, pass it explicitly:

```bash
sudo ./install.sh --zone example.com --email you@example.com \
  --image localhost/umkim-thin:my-build
```

## Upgrade an app

App upgrades are explicit and versioned. Take a backup/VM snapshot first, then
list the available versions:

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin app versions nextcloud
```

Upgrade to a specific chart version (no implicit "latest"):

```bash
kubectl -n umkim-thin exec deploy/umkim-thin-console -- \
  /usr/local/bin/umkim-thin app upgrade nextcloud --to 0.1.17 --yes
```

The same flow is in the console: **Applications → Upgrade…**. It shows the
current and catalogued versions, requires a backup confirmation, and runs
`helm upgrade --atomic`; a failure rolls the release back. The app's exposure
(`public`/`authelia`/`vpn`) and certificate mode are preserved.

Read the app chart's release notes first. Database migrations are the risk; the
backup is the real rollback for stateful apps, because Helm rollback does not
reverse a schema migration.

## If it goes wrong

- **Platform rollout stuck:** `sudo ./install.sh --doctor`, then
  `kubectl -n umkim-thin rollout status deploy/umkim-thin-console` and
  `kubectl -n identity get pods`.
- **Console won't start:** `kubectl -n umkim-thin logs deploy/umkim-thin-console`
  and `kubectl -n umkim-thin describe pod -l app=umkim-thin-console`.
- **Certificate errors after an upgrade:** the ACME resolver needs port 80 and
  DNS; check `kubectl -n kube-system logs deploy/traefik`.
- **Roll back:** check out the previous tag, re-run the installer with the same
  flags (including `--reuse-k3s`), and restore app data from the pre-upgrade
  backup if a migration ran.

Do not “fix” a stuck rollout by deleting the PVC. That is the data.

## Next

[Something is broken →](08-troubleshooting.md)
