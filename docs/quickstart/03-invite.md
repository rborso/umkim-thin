# 3. Invite someone

**Goal:** give another person their own directory account without sharing yours.

## Create the invite

1. Open the console → **Invitations**.
2. **Label**: a name for you (for example `Sam`). It is shown on the invite and
   becomes the default display name.
3. **Group**: the directory group the new account joins. `default-tenant` is
   created for you.
4. Click **Create invite**.
5. Copy the link (`https://auth.<zone>/invite?token=…`) and send it over a
   private channel.

The link is single-use and expires after 7 days. It is a credential: anyone who
has it can create the account. Do not post it in a public channel.

## What the invitee does

1. Opens the link.
2. Chooses a **username**, a **display name**, and a password (at least 8
   characters).
3. The account is created in LLDAP and added to the group. The invite is marked
   used.
4. They sign in at `https://auth.<zone>` and can open the apps that allow their
   group.

An invitation **never takes over an existing account**: if the username already
exists, the redemption is refused. That is intentional.

## Check and revoke

The Invitations table shows the label, group, status (`unused`, `used`,
`expired`), and the token. Use **Revoke** to make the link stop working before
it is used. Every create/revoke is written to the console's audit log with the
actor.

## Add the person to an app

Creating the directory account is not the same as creating an app account:

- Apps with **forward-auth** (the default) only need the portal session.
- Apps with **OIDC** create their own user on first login (Immich, Paperless).
- Apps with **LDAP** (Gitea, Nextcloud, Jellyfin) read the directory directly.
  Gitea's OIDC source is configured, but new-user auto-registration is not
  reliable yet, so use LDAP there.
- Apps with local accounts only (some *arr tools, qBittorrent) have their own
  admin user; see the app's page in the catalog.

The catalog entry tells you which model an app uses.

## Reset a password, or remove someone

The directory UI is at `https://lldap.<zone>`, behind the portal. Sign in with
the directory admin account (`admin`; the password was printed at install).

- **Forgot password:** Users → the person → Change password. Send them the
  temporary password; they can change it themselves in the same UI after
  signing in. LLDAP has no email reset and no admin “send link”, so this is
  deliberately manual.
- **Remove someone:** delete the user. LDAP-backed apps (Nextcloud, Jellyfin,
  Gitea) stop authenticating immediately. OIDC apps (Immich, Paperless) keep
  the account row but cannot log in through the portal again; delete the
  app-side account too if the data must go.

Both actions are audited by LLDAP. The directory is the one place to do this;
do not hand-edit app users.

## If the link does not work

| Symptom | Cause |
|---|---|
| “unknown invitation” | The token is wrong, or the invite was revoked |
| “already been used” | The link is single-use; create a new one |
| “expired” | The 7-day window passed |
| The signup page loads but the form fails | The console cannot reach LLDAP; check `sudo ./install.sh --doctor` |

## Next

[Install your first app →](04-first-app.md)
