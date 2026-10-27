# Adding an indexer (media stack)

The Servarr stack ships without tracker integrations. Prowlarr is the only
place that talks to an indexer; Sonarr and Radarr get the indexers from it.

You can add an indexer from **Prowlarr** directly, or from the **Indexers**
action on the installed Servarr row in the console. That manager is scoped to
the specific deployed stack (release + namespace), uses Prowlarr's definitions,
and sends the filled fields straight to that Prowlarr; the console does not
store the credentials. Add definitions with interactive login or unusual
challenge handling in Prowlarr itself.

## In Prowlarr

1. Open `https://prowlarr.<zone>` (behind the portal) and sign in with the
   directory account or the local `admin` account.
2. **Indexers → Add Indexer**, search for the tracker by name. Prowlarr ships
   the definitions; this project does not.
3. Fill the fields the definition asks for: base URL, username, password, and a
   one-time 2FA code where the tracker requires one. Prowlarr stores the session;
   the code itself is not persisted by this project.
4. If the tracker is behind Cloudflare, assign the **FlareSolverr** proxy (the
   bootstrap created it) or the `flaresolverr` tag.
5. **Test**, then **Save**.

## Sync to Sonarr and Radarr

**Settings → Apps → Sync App Indexers** (or wait for the scheduled sync). The
two apps should list the indexer within a minute. If they do not, check
**System → Tasks** in Prowlarr for a failed `ApplicationIndexerSync`.

## Media language profiles

The install form can create additional language variants of the target quality
(for example `HD-720p · English`) without changing the default. The installed
stack's **Profiles** action can add any resolution/language variant later.
Users choose one through Seerr's **Advanced → Quality Profile**; the default
remains the configured default. Radarr has no separate movie-language request
field, so the profile name is the language choice.

Additional profiles can be deleted from the same manager, but only when no
library item or active Seerr request uses them. The default and built-in
profiles cannot be deleted.

## Request quality and language in Seerr

The bootstrap gives regular Seerr users the **Request Advanced** permission, so
they can open the request dialog's **Advanced** section and choose the quality
profile. The default is the profile configured by the console.

Language profiles are only exposed when the *arr provides them; Radarr selects
the language through its quality profile, so movie requests do not get a
separate language picker. The console's default language is what that profile
uses.

## If downloads do not start

- The qBittorrent download client is created by the bootstrap. Verify it under
  **Settings → Download Clients**; the category is `prowlarr` (plus `tv` and
  `movies`).
- The bootstrap's NetworkPolicy keeps the Job off the internet. That is
  deliberate; the *arr apps, not the Job, reach the tracker.
- A tracker that needs a VPN for downloads is out of scope for this project:
  route qBittorrent's traffic yourself if the provider requires it.

## Why this is opt-in

Indexer choice is a user decision and tracker credentials carry terms-of-service
and legal questions that do not belong in the platform. The catalog installs and
configures the software; the operator chooses the sources. The console's
generic manager is optional and stores nothing itself; Prowlarr remains the
source of truth.
