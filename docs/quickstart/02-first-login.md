# 2. First login

**Goal:** sign in to the portal, register a second factor, and open the console.

## The two logins

There are two layers, deliberately:

1. **Authelia** at `https://auth.<zone>` is the portal. It protects the console
   and every app that uses forward-auth. This is where you register a second
   factor.
2. **The console** at `https://console.<zone>` has its own login. It sits behind
   Authelia, so you sign in to the portal first.

## Sign in to the portal

1. Open `https://auth.<zone>`.
2. Username: `admin`.
3. Password: the **Admin password** printed by the installer. It is also in
   `/etc/umkim-thin/secrets.yml` (root only):

   ```bash
   sudo sed -n 's/^secret_lldap_admin_password: "\(.*\)"/\1/p' \
     /etc/umkim-thin/secrets.yml
   ```

## Register a second factor

Do this now, before you invite anyone.

1. In the portal, open **Two-factor authentication** (or **Register device**).
2. Choose **One-time password** (TOTP) and scan the QR code with an
   authenticator app (Aegis, 1Password, Google Authenticator, …).
3. Enter the six-digit code to confirm.
4. Keep the recovery codes somewhere safe.

Authelia's regulation layer already limits repeated failures. TOTP is the
second layer; the console is behind it, so this protects the admin surface too.

## Open the console

1. Open `https://console.<zone>`.
2. If the portal asks again, sign in; then the console's own login appears.
3. Username: `admin`, password: the same directory admin password.

The console is the place to install apps, invite people, and create WireGuard
clients. The directory UI at `https://lldap.<zone>` is behind the same portal;
it is where you reset a forgotten password or remove a person (see
[Invite someone](03-invite.md#reset-a-password-or-remove-someone)).

## The breakglass account

The console also has a local account, `localadmin`, for when the directory is
unavailable. Its password is the **Breakglass password** printed by the
installer:

```bash
sudo sed -n 's/^secret_console_localadmin_password: "\(.*\)"/\1/p' \
  /etc/umkim-thin/secrets.yml
```

Because the console is published behind Authelia, a broken portal also blocks
the console ingress. In that case, reach the console directly from the node:

```bash
sudo kubectl -n umkim-thin port-forward deploy/umkim-thin-console 8080:8080
```

Then open `http://localhost:8080` on the node (use an SSH tunnel if the node is
remote). `localadmin` works there without Authelia. This is an advanced recovery
path; keep the password offline.

## Keep the secrets safe

`/etc/umkim-thin/secrets.yml` contains the directory admin, the breakglass
password, the Authelia session and storage keys, and the OIDC signing material.
On the host it is root-owned, mode 0600; the console keeps a copy on its PVC.
Back it up with the rest of the state (see [Backup](06-backup.md)); do not paste
it into a chat or an issue.

## Next

[Invite someone →](03-invite.md)
