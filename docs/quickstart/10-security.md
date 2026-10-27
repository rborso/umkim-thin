# 10. First-week security checklist

This is a checklist, not a guarantee. The platform is **alpha** and
single-admin; read [`../security-plan.md`](../security-plan.md) for the full
threat model and the open work.

## Before you invite anyone

- [ ] **Register a TOTP device** for `admin` in the portal. The console is
      behind Authelia; this is the second factor for the admin surface.
- [ ] **Secure the directory UI.** `lldap.<zone>` is behind the portal, but the
      LLDAP admin login is a second password with no MFA of its own. Keep it in
      the password manager and do not reuse it.
- [ ] **Store the generated passwords** in a password manager, and keep
      `/etc/umkim-thin/secrets.yml` off chat and tickets.
- [ ] **Back up** (`sudo ./install.sh --backup`) and copy the backup off the
      host.
- [ ] **Check the certificate** on `auth.<zone>` and `console.<zone>`: a valid
      padlock, no warning. If not, fix DNS/port 80 before inviting people.
- [ ] **Limit the attack surface**: 80/443, 51820/UDP if you use WireGuard, and
      2222/30222 if you use Gitea SSH. Do not expose 6443 or the k3s API.
- [ ] **SSH**: key-only, no root password login, and a firewall allowlist if you
      have a stable address.

## In the first week

- [ ] **Look at what is public.** Every catalog app gets a hostname. Open each
      one in a private window; you should be redirected to the portal, not shown
      the app. Share links are the deliberate exceptions.
- [ ] **Use one account per person.** Invite people; do not share `admin`.
- [ ] **Review app admin accounts.** Some apps create their own admin (for
      example Nextcloud's `nc-admin`); store those passwords in the password
      manager and change weak/default ones.
- [ ] **Test a restore** of one app's data. A backup you have not restored is a
      hypothesis.
- [ ] **Update** the platform monthly, and read the release notes. Run a backup
      before every upgrade.
- [ ] **Watch the logs** for repeated login failures and unexpected pods:
      `sudo kubectl get pods -A` and `sudo ./install.sh --doctor`.

## Defaults you should know

| Area | Default | What to do |
|---|---|---|
| Console | Public host, behind Authelia + its own login | Keep TOTP on; restrict to WireGuard if you can |
| Apps | Public host, Authelia in front (or OIDC/LDAP) | Check each app; remove the ones you do not use |
| Public paths | Declared per app (share links, native clients) | Treat them as public; they are token-authenticated by the app |
| Directory | One LLDAP per cluster, `default-tenant` group | One group is enough for a household; add groups deliberately |
| WireGuard | Optional; UDP 51820 | Do not expose it without a firewall rule; revoke unused clients |
| Secrets | Generated once, `/etc/umkim-thin/secrets.yml` (0600) | Back up; never regenerate on reinstall |
| Backups | etcd + `/etc/umkim-thin` + console PVC | Add VM/volume snapshots for app data |

## Known limits (do not assume otherwise)

- The console is a **cluster-admin-adjacent** component: it installs charts and
  can read the cluster. Do not give it to untrusted users.
- The console is **single-admin** today; invitations create directory users, not
  console users.
- Gitea's OIDC auto-registration and Nextcloud's `user_oidc` are not complete;
  see [`../auth.md`](../auth.md).
- The media stack's bootstrap is optional and specific; it is not a general
  app-setup engine.
- `local-path` has no snapshots. App data protection is your VM/storage
  snapshot.

## If you suspect a compromise

1. Take the host off the network (or close 80/443 at the firewall).
2. Preserve a copy of the VM/disk for analysis.
3. Rotate the directory admin, the console breakglass, app admin passwords, the
   Authelia session/storage keys, OIDC client secrets, WireGuard keys, and any
   API keys. Regenerating `secrets.yml` alone is not enough; rotate at the
   source.
4. Rebuild from a known-good backup rather than cleaning in place.
5. If the issue is in this project, report it privately before publishing
   details. See [`../security-plan.md`](../security-plan.md).
