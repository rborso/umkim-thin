# Full build

`ci/e2e.sh` has two profiles and is run manually. The **default** one is:
it provisions the target, installs the catalog apps, and verifies them, but it
accepts the lab CA as the fallback certificate and tolerates HTTPS errors in the
browser.

A **full** run is manually triggered and does the things that are too slow or
too network-dependent for a PR gate:

- **real ACME issuance** (Let's Encrypt *staging*, never production) with a
  certificate per host, instead of the lab-CA wildcard fallback;
- **strict TLS**: `ignoreHTTPSErrors` is off and the staging roots are trusted,
  so a bad certificate fails the run;
- the **Servarr bootstrap**, driven through the **console**: the credentials are
  stored in the console, which builds the image on the pinned node and runs the
  chart's Job (nothing prebuilt, no registry);
- a **preflight** that refuses to start if the zone does not point at the
  cluster.

```sh
# on the runner (or any machine that reaches the lab), manually:
./ci/e2e.sh single --full
./ci/e2e.sh ha --full
# or by passing --full to ci/e2e.sh
```

## What has to be true first

1. **The zone points at this cluster.** Set `public_ip` in
   `deploy/provision-<target>.env` to the address the public A records resolve
   to. The preflight compares public DNS (queried over DNS-over-HTTPS, i.e. what
   Let's Encrypt sees) against it and against the cluster answering that `Host`.
2. **Port 80 reaches the ingress from the internet.** The preflight run from the
   runner cannot prove this: the lab NAT does not hairpin, so from inside the
   public address terminates on the router. Run the external half from a host
   outside the lab:

   ```sh
   ./ci/preflight.sh --external test.example.com auth.test.example.com ...
   ```

   It prints whether port 80 reaches the ingress or the router, and who signed
   the certificate it sees.
3. **Indexers (optional).** The Servarr bootstrap has no tracker dependency:
   indexers can be added in Prowlarr after the stack is up, or through the
   console's per-app **Indexers** action (see `docs/servarr-indexers.md`).
   Nothing fails when none is configured — the media stack is simply
   unconfigured for downloads until you add one.

## What the full profile changes

| | default | full |
|---|---|---|
| Traefik default certificate | lab-CA wildcard | none (so ACME runs) |
| ACME directory | staging | staging (unchanged) |
| TLS verification | tolerant | strict, staging roots trusted |
| `tls.spec` | rejects Traefik's default cert | requires a **STAGING** issuer |
| Servarr bootstrap | off | configured through the console (build + run) |

The wildcard default certificate is the reason ACME never ran before: a default
certificate that matches the hostname makes Traefik treat the router as already
covered. A full run leaves it out, and every route gets its own certificate.

The Playwright gate (`apps.spec.ts auth.spec.ts tls.spec.ts console.spec.ts`)
signs in as a regular invited tenant (`ci-portal`) and requires **every
installed app** to accept a directory login: Gitea, Jellyfin and Nextcloud over
LDAP, Immich and Paperless over the real OIDC flow, and Gitea's SSH clone/push
on the dedicated port 2222. See `docs/auth.md` for the OIDC status (Gitea's
source is configured but its new-user auto-registration is not gated;
Nextcloud's `user_oidc` is disabled). HA and single install the same `ci_apps`
(including Nextcloud); the functional user journeys stay informational.

## The bootstrap image

The normal path is the console (Applications → the media-stack row → Run setup):
it stores the credentials, probes the pinned node, builds the image there and
imports it into that node's containerd. The full-profile run drives that same
API through `ci/console-servarr.sh`.

The direct fallback is still useful when the console is unavailable:

```sh
./ci/enable-servarr-bootstrap.sh single   # or ha
```

- source: `images/servarr-bootstrap` (vendored; also baked into the console
  image as the only build context)
- build: `podman build`, tagged
  `localhost/umkim-thin/servarr-bootstrap:<VERSION>-<commit>`; the console
  computes the same tag from `VERSION` and the commit baked into its image
- delivery: `podman save` piped into the target's containerd
  (`k3s ctr -n k8s.io images import`); the console's on-demand builder uses
  kaniko in a dedicated namespace and the same import
- Secret: created by the console at install (app passwords generated once, the
  chart's *arr API keys copied in); the script preserves an existing Secret
- enable: `bootstrap.enabled=true`, `imagePullPolicy=Never` and a
  `nodeSelector` for the node that has the image

The image is based on `mcr.microsoft.com/playwright` (about 1.5 GB, pinned by
digest) because the bootstrap drives parts of Jellyfin and Seerr with
Playwright. Expect the first build to pull it; the console skips the build when
the tag is already on the node.

## Known limits

- **No production Let's Encrypt.** Staging certificates are real ACME
  certificates, but browsers do not trust them; the run trusts the staging roots
  explicitly. Production issuance would need the rate-limit care described in
  `AGENTS.md` and is deliberately out of scope.
- **Hairpin.** Anything that must observe the public path has to run from
  outside the lab. The in-lab preflight checks DNS and that the cluster serves
  the name; the external mode checks the public path.
- The bootstrap's local Jellyfin admin is `jellyfin-admin`, deliberately distinct
  from the directory's `admin`: Jellyfin resolves a local account before an LDAP
  one, so a local `admin` shadows the directory user.
- The bootstrap's Jellyfin wizard step cannot be re-run on an already-configured
  Jellyfin. The console records the credential hash and skips a repeat run with
  the same image and credentials; a changed image or credential set does re-run
  the Job and may hit this. A fresh install is the supported path.
