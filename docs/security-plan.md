# Security hardening plan

Status: **in progress — see Implementation progress**. The single and HA targets
are green; the external tracker-account rotation and the deferred P1/P2 items
are listed there. Last reviewed 2026-10-27. All test runs are manual; there is
no hosted workflow.
Companions: `docs/auth.md` (current auth matrix, Gitea SSH/OIDC status),
`docs/console.md`, `docs/console-plan.md`.

The scope is the whole platform: single and HA targets, the console, the
installer, the on-demand builder, the identity stack (Authelia/LLDAP), the
manual test runner, the catalog apps and their charts. The lab is a test
environment, not production, but the public edge, the runner keys, the app data
and the app credentials are real.

## Threat model (condensed)

**Assets.** The cluster and its etcd, the nodes, the app data (Nextcloud, Immich,
Paperless, Gitea, Servarr/Jellyfin), the directory, the console, the test-runner keys,
the app credentials, the TLS keys and the backups.

**Adversaries.** The public internet (80/443, **2222**, 51820/UDP), a
compromised app or chart, a malicious pull request, a compromised dependency or
image, a stolen credential or laptop, and a lateral move from one app to
another.

**Trust boundaries.**

| Boundary | Current state |
|---|---|
| Internet ↔ Traefik | 80/443 and **2222** public; no WAF/CrowdSec or edge rate limit; only whatever the app enforces |
| Traefik ↔ apps | forward-auth gates the route; the app still has its own login |
| Directory admin UI | `lldap.<zone>` is public but behind Authelia; LLDAP itself has no MFA, so the portal session is the only second factor |
| Authelia ↔ apps | OIDC clients with **plaintext** secrets; `accessRules` empty; no MFA |
| Console ↔ cluster | the console SA can read Secrets, create pods/exec and install arbitrary charts |
| Builder ↔ node | the builder mounts the node's containerd socket and runs as root |
| Manual runner ↔ lab | the runner has the chart/jumphost/node keys; the e2e is run manually, so the exposure is a compromised maintainer or runner account |
| Apps ↔ apps | default-deny ingress, but the node network is allowed for WireGuard; egress is mostly open |
| Host ↔ console | PVC state and ServiceAccount kubeconfig; single mode adds only the storage-agent socket hostPath, no hostNetwork |

## Priorities

| Tier | Meaning | Target |
|---|---|---|
| **P0** | containment: a known credential or a public path is exploitable now | today |
| **P1** | identity/edge and the two privileged components (console, builder) | this week |
| **P2** | secrets at rest, backups, build/test supply chain, network/egress, observability | this month |
| **P3** | app-specific hardening and the SSO/OIDC reworks | later |

## Implementation progress (reviewed 2026-10-27)

| Item | State |
|---|---|
| SEC-001 rotate private-development values | **Done for the live single cluster (2026-10-27).** Directory admin, console breakglass, Nextcloud/Paperless/Immich admins, Servarr forms passwords, qBittorrent, Jellyfin and the *arr/Prowlarr API keys are rotated; the nCore indexer was removed. The external nCore account password/passkey must still be changed by its owner; the HA target was not running. |
| SEC-002 purge local copies | **Partial.** The known `/tmp` credential file is shredded; the test scripts delete scratch keys/repos/files. Copies or transcripts outside this repository are outside our control; rotate anything that appeared there. |
| SEC-003 delete test users/keys | **Done.** `ci/cleanup-ci.sh` enumerates LLDAP/`ci-*` and deletes Gitea keys/repos/users; verified live. |
| SEC-004 random test passwords | **Done.** Per-run tenant/app users; the cleanup runs from the EXIT trap. |
| SEC-005 runner isolation | **Verified.** No automated or hosted test workflow; e2e is manual. |
| SEC-006 Authelia MFA/access rules | **Partial.** Regulation (5 failures/2 min, 5 min ban) is enabled; MFA enrolment, per-tenant `accessRules` and the test-runner service identity are not. |
| SEC-007 Gitea/2222 | **Partial.** Registration is off; sign-in view required; repository creation capped; the 2222 source allowlist/rate limit is still an operator/router task. |
| SEC-008 console headers/XSS/session | **Done.** HSTS/CSP/frame/type/referrer/permissions headers; escaped dynamic HTML; 5-failure/5-minute login limiter; `/metrics` requires a session. |
| SEC-009 Nextcloud | **Done.** Public host is the only trusted domain; the readiness probe is TCP; Collabora certificate verification is back on. |
| SEC-010 Gitea CA | **Done.** Combined public + platform CA in the workload and bootstrap. |
| SEC-011 OIDC secret hash | **Done.** PBKDF2-SHA512 in Authelia's adapted base64; validated with Authelia 4.38.19. |
| SEC-012 console RBAC | **Partial.** `pods/exec` is now a Role in `identity`; the ClusterRole's broad Secret/Helm permissions are unchanged. |
| SEC-013 builder split | **Deferred.** Needs the full-profile live build; the on-demand console path is unchanged. |
| SEC-014 bootstrap Job | **Chart done.** Non-root, read-only rootfs, no SA token and a stack+DNS egress policy; the full-profile live bootstrap is not run in the default gate. |
| SEC-015 secrets at rest | **Partial.** Single node has etcd encryption and 0600 modes. HA is deliberately off: k3s generated a per-server key and the hashes diverged, breaking Secret sync. Automate the key distribution before enabling it on HA. |
| SEC-016…025 | Not started. |

**Tests run.** `ci/static.sh` (go build/vet/test, helm lint/template, ansible
syntax) is green. The final clean single and HA reinstalls both passed the
42-test acceptance gate and the state/platform checks; the functional user
journey suite (18 tests, including invitation acceptance, OIDC, Gitea SSH,
Nextcloud editing/sharing, Immich, Paperless OCR and Servarr) passed 18/18 on
both live targets. HA ran with `secrets_encryption=false` (the one assertion
skipped there). The OIDC hash was validated with
`authelia crypto hash validate`. Live checks after the runs:
LLDAP contains only `admin`/`tenant`/`svc-bind` on single and `admin`/`svc-bind`
on HA (no `ci-*`); Gitea has registration off and requires sign-in; Nextcloud
has the public host as its only trusted domain; the console headers and the
single-node etcd status are present.

## Follow-up audit (2026-10-21)

A review of the tree found issues this plan does not name. They are tracked as
`SEC-026`…`SEC-032` in [`security-followup.md`](security-followup.md):

- Authelia's session, storage, LDAP, reset and OIDC secrets are rendered into a
  **ConfigMap** (`SEC-026`, High);
- the directory password and admin token go through `kubectl` **argv** and are
  echoed in error text (`SEC-027`, High);
- a handful of smaller items: breakglass comparison, HTTP timeouts, the invite
  store race, missing origin/audit on some mutations, and the console pod
  baseline (`SEC-028`…`SEC-032`).

They are launch blockers for a novice-facing release and have landed (see the
follow-up document for the implementation and tests). The live assertions run
in the manual e2e; Helm prunes the old ConfigMap on upgrade.

## P0 — containment (today)

| ID | Task | Change | Acceptance |
|---|---|---|---|
| SEC-001 | **Rotate private-development secrets** | Rotate the directory admin, console breakglass, app admin credentials, Servarr passwords and *arr API keys at the source; remove stored tracker credentials. Applied to the live single cluster on 2026-10-27. The external nCore account password/passkey remains an owner action, and the HA target was not running. | The old local values no longer authenticate; the nCore indexer is gone; `ci/credentials.sh` shows the new ones masked. |
| SEC-002 | **Purge local credential copies** | Remove `/tmp/umkim-thin-credentials.txt`, any `~/.config/umkim-thin` copies, the `/tmp/umkim-git-*` test keys/repos, the `/tmp/servarr-*` staging files, the shell history entries, and the test artifacts that may contain requests/cookies. Keep `ci/credentials.sh` print-only (it already is) and make sure `ci/enable-servarr-bootstrap.sh` and the console's setup paths delete their temporary files. | `find`/`grep` for the known values returns nothing outside the cluster; `/tmp` and the runner work dir are clean; no test artifact contains a session cookie. |
| SEC-003 | **Delete test users and keys from both clusters** | The `ci-*` accounts the suite creates (`ci-portal`, `ci-immich`, `ci-paperless`, `ci-nextcloud`), any leftovers such as `ci-oidc-*` from the removed OIDC tests, the Gitea `ci-ssh-*` keys/repositories, and the matching Nextcloud/Immich/Paperless accounts. The clusters are left running and these are known-password test accounts. | The users cannot log in to the portal or an app; the Gitea keys/repos are gone; the LLDAP groups are clean. |
| SEC-004 | **Stop committing fixed test passwords** | `ci/portal-user.sh` and `ci/e2e.sh` used fixed `Ci-Portal-*` values; `auth.spec.ts` embedded fixed `Ci-Oidc-*`/`Ci-Ldap-*` passwords. Generate a random password per run, pass it through the environment, delete the users in a `trap`, and stop uploading Playwright traces/`test-results` that can contain session cookies. Add `gitleaks` so it cannot regress. | The repository contains no fixed login; `gitleaks` is clean; a rerun on a live cluster still passes. |
| SEC-005 | **Keep untrusted PRs off the test runner** | The runner holds the chart, jumphost and node credentials. The e2e is manual and no hosted test workflow runs. Do not add a PR-triggered job that runs `ci/e2e.sh` on the lab runner; that would be a credential-exfiltration path. | A fork PR cannot read the lab keys or reach the cluster; only a trusted manual run reaches the e2e. |

## P1 — identity, edge, console, builder (this week)

| ID | Task | Change | Acceptance |
|---|---|---|---|
| SEC-006 | **Authelia MFA and access rules** | TOTP/WebAuthn for `admin`, the console route and the OIDC clients; the global `default_policy: deny` is already set, but `accessRules` is empty, so add explicit per-app/tenant group rules; `regulation` (brute-force ban); the `one_factor` OIDC clients become `two_factor` where supported. The manual test runner needs a deliberate service identity (a dedicated account/credential) rather than the admin password. | An admin login without a second factor is refused; a user in the wrong group gets 403; repeated failures are banned. |
| SEC-007 | **Seal the public Gitea SSH 2222** | Keep the Traefik `gitssh` entry point, but restrict the source at the router and the host firewall (WireGuard allowlist, or CrowdSec/fail2ban + rate limits). Gitea: the catalog currently sets `disableRegistration: false` (the chart default is true), so set it to true, add `REQUIRE_SIGNIN_VIEW`, `DEFAULT_USER_IS_RESTRICTED`, `MAX_CREATION_LIMIT`, admin 2FA and key-only SSH; keep `ALLOW_ONLY_EXTERNAL_REGISTRATION=true`. The OIDC auto-registration is only re-enabled through the per-source admin API/UI. | A connection from a non-allowed source is dropped; the public sign-up form cannot create an account (OIDC auto-registration stays directory/domain-restricted); the SSH host-key fingerprint is published. |
| SEC-008 | **Console security headers, XSS and session** | Traefik: HSTS, `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy`, `Permissions-Policy`, `COOP/CORP`, and a CSP (nonce/hash, no `unsafe-inline` ideally). The UI uses `innerHTML` with invite `displayName`/`email`, the host inputs and app names: switch to `textContent`/DOM creation and validate hosts server-side. Shorten the 12 h session, add login rate limiting/lockout, keep the audit line for every mutation, and scrape `/metrics` only from the pod network (the auth whitelist currently leaves it anonymous). | A stored `<script>` in an invite/host does not execute; the headers are present on `/` and `/api/*`; failed logins are throttled. |
| SEC-009 | **Nextcloud trusted domains/proxies and Collabora** | Replace `trustedDomains: ["*"]` with the public host and the readiness address; restrict `trustedProxies` to the ingress/proxy addresses; set `forwarded_for_headers` explicitly. Do not keep `disable_certificate_verification=yes` for richdocuments; trust the platform CA instead. Add 2FA and brute-force protection; keep `user_oidc` disabled until its route/CA issue is fixed. | A request with an unknown Host is rejected; Collabora WOPI uses verified TLS; the admin has 2FA. |
| SEC-010 | **Gitea CA bundle** | When OIDC is enabled the chart sets `SSL_CERT_FILE` to the platform CA only, which drops the public roots (email/webhooks/OIDC to public endpoints). Combine the image's system bundle with the platform CA, as the Nextcloud chart now does. | `openssl`/Go calls to both the portal and a public endpoint succeed in the Gitea pod. |
| SEC-011 | **Hash the OIDC client secrets** | `internal/app/identity.go` writes plaintext `clientSecret` into `identity-values.yml` and the app values; Authelia warns about it. Use Authelia's PBKDF2 hashed form for the Authelia client, keep the plaintext only in the app's Secret, and rotate on change. | The Authelia config has no plaintext client secrets; the apps still authenticate; the warning is gone. |
| SEC-012 | **Split the console's privilege** | Today the console ClusterRole can read every Secret, create pods and `pods/exec`, and install arbitrary charts. Split into: a read-only API identity (status/storage/catalog/apps), a settings identity limited to the servarr Secret, a narrow identity for the LLDAP exec (a Role in `identity`), and an installer identity. Add Kyverno/OPA admission: no privileged pods outside `umkim-thin-build`, no hostPath outside the console/build namespaces, no `cluster-admin` bindings, no hostNetwork outside WireGuard. Add quotas/limits. | The status/catalog/storage APIs work with no Secret read; settings can read only the servarr Secret; a chart that requests a privileged pod or a hostPath is rejected; the console cannot read unrelated Secrets. |
| SEC-013 | **Split the builder from the socket** | `internal/app/servarr.go` builds and imports in one root container with `/run/k3s/containerd/containerd.sock`. Build with kaniko in a container **without** the socket, write the tar to an emptyDir, and let a minimal importer (no network, no SA, read-only rootfs) import it. Keep the fixed context and digest-pinned base; add `npm ci --ignore-scripts`, SBOM, Trivy and cosign. | The build container cannot reach containerd; the importer has no network/SA; an SBOM exists; a vulnerable image fails the scan. |
| SEC-014 | **Harden the bootstrap Job** | The servarr bootstrap runs Playwright and holds the qBittorrent/Jellyfin/Servarr credentials. Make it non-root, no SA token, read-only rootfs, dropped capabilities, seccomp, and a NetworkPolicy limited to the app services + DNS (no internet). Redact the qBittorrent/Jellyfin/Servarr values from logs. | The Job cannot reach the internet; no credential appears in its logs; it still configures the stack. |
| SEC-015 | **Secrets at rest and mode** | Enable k3s `secrets-encryption`; `chmod 600 /etc/rancher/k3s/k3s.yaml`; `chmod 700 /etc/umkim-thin`, 600 for `secrets.yml`, `identity-values.yml`, `apps/*-values.yaml`, the OIDC keys, the session key. Move the values to SealedSecrets/SOPS (age) or an external secret manager. | etcd secrets are encrypted; the kubeconfig and state are not world-readable; a stolen state dir does not yield plaintext client secrets. |

## P2 — supply chain, network, backups, observability (this month)

| ID | Task | Change | Acceptance |
|---|---|---|---|
| SEC-016 | **Local security gate** | Add `gitleaks`, `govulncheck`, `npm audit`, `trivy` (fs and image), `kubeconform`, `kube-linter`, `semgrep`, `shellcheck`, `hadolint`, `cosign verify` to `ci/static.sh`/a new `ci/security.sh`; fail on secrets and critical CVEs. This is a manual gate; no hosted workflow is used. | The gate fails on a planted secret/CVE and passes on the current tree. |
| SEC-017 | **Egress and metadata** | Default-deny egress in the app namespaces; explicit destinations for the servarr indexers and any app that needs the internet (FQDNs via Cilium/Calico FQDN policy or an egress proxy; otherwise the indexer IP ranges); block the cloud metadata endpoint (169.254.169.254) and the node network except where the WireGuard masquerade requires it; the builder egress limited to the registry/npm FQDNs. | An app cannot reach the metadata endpoint or an arbitrary internet host; the servarr indexers still work. |
| SEC-018 | **Audit and observability** | Enable the k3s/API audit log; ship the console audit, Authelia/LLDAP logs and app logs to a small Prometheus/Loki/Alertmanager or equivalent; add Falco for runtime and CrowdSec (or fail2ban) at the edge; alert on cert expiry, backup failure and privileged pods. | A login/mutation/privileged pod produces an alert; the audit is not only stdout on one node. |
| SEC-019 | **Backups and restore** | etcd snapshots, Longhorn/NFS backups (restic/Velero), encrypted and offsite, with a documented and tested restore. Include the console state, the identity values and the app databases. | A restore into a clean environment is exercised; the recovery point/time are known. |
| SEC-020 | **App hardening** | Gitea: admin 2FA, repo/action limits, no public sign-up. Nextcloud: 2FA, app-store integrity, encryption, backups. *arr: `AuthenticationMethod=External` behind a sanitized header or keep forward-auth. qBittorrent: keep the WebUI behind forward-auth; route the download traffic through a VPN if the tracker/provider requires it; no UPnP/DHT as appropriate. FlareSolverr: restricted egress. Jellyfin: LDAP/native clients, admin 2FA where possible. Immich/Paperless: public share paths and API keys reviewed. | The app-specific checklist in `docs/auth.md` is updated and tested. |
| SEC-021 | **Pin and attest** | Pin every upstream app image by digest and the console/builder base images by digest; record a checksum/SBOM for local console/bootstrap builds. Internal images and charts are not published, so there is no registry signature to verify. | `imagePullPolicy`/digests are explicit; the local build emits a verifiable checksum/SBOM; no publication workflow is required. |

## P3 — SSO/OIDC reworks (later)

| ID | Task | Change | Caveat |
|---|---|---|---|
| SEC-022 | **Gitea OIDC** | Fix the per-source `AutoRegister` through the Gitea admin auth-source API/UI (the CLI has no flag), restrict the email domain and the `default-tenant` group, and re-add the OIDC login to `auth.spec.ts`. | Git clients still use keys/tokens. |
| SEC-023 | **Nextcloud OIDC** | Fix the Nextcloud 35 `user_oidc` login route/discovery, or use `user_saml`/proxy-header SSO. Keep LDAP for native clients. | Desktop/mobile clients use app passwords. |
| SEC-024 | ***arr / Jellyfin / console** | *arr `AuthenticationMethod=External` behind a Sanitized header; Jellyfin web SSO plugin (native clients stay LDAP); console trusts Authelia's `Remote-User` for the admin, keeping `localadmin` breakglass. | No OIDC for qBittorrent or Jellyfin native clients. |
| SEC-025 | **Policy as code / DR** | CIS/kube-bench, Kyverno policies in the repo, a documented DR runbook and periodic restore drills. | A larger operational change; not a prerequisite for the P0/P1 work. |

## Verification

- **New `ci/security.sh`** (run in Tier 1): `gitleaks`, `govulncheck`, `npm
  audit`, `trivy fs`, `kubeconform`, `kube-linter`, `semgrep`, `shellcheck`,
  `hadolint`, `cosign verify`. Fail closed on secrets/critical CVEs.
- **`ci/assert.sh`** additions: the console security headers; the Nextcloud
  Host/trusted-proxy behaviour; the Gitea 2222 allowlist (a negative probe from
  a disallowed source, or a firewall/NetworkPolicy config assertion); the etcd
  encryption status; the `k3s.yaml`/state modes; the absence of the `ci-*` users
  after the run.
- **`auth.spec.ts`/`ci/e2e.sh`**: random per-run credentials, a `trap` that
  deletes the users/keys/repos, and no test artifacts with cookies/requests.
- **Manual**: a second-factor login for `admin`; a blocked 2222 from a
  non-allowed source; a restore drill; a rotated app credential.

## Operator tasks (environment-specific, not in this repo)

- Router/NAT: restrict the public **2222** source (or keep it WireGuard-only),
  the ports 80/443/51820, the HA VIP, the single-node NAT.
- WireGuard: peer lifecycle, the allowed IPs (pod/service CIDR plus only what
  is needed), and the test/node-network exception.
- Public DNS, NTP, the platform CA/staging trust, and the `local/` notes.
- Rotating the test-runner and jumphost keys.

## Dependencies and risks

- **MFA vs test runner**: the e2e logs in as the admin; decide on a dedicated
  test identity or a TOTP secret before enabling MFA. Do not weaken MFA for the
  runner.
- **Console RBAC split** can break Helm installs; introduce the admission
  policies first, then narrow the roles, and test each chart.
- **Builder split** changes the on-demand bootstrap; keep the fallback
  `ci/enable-servarr-bootstrap.sh` until the new path is green.
- **etcd encryption** on a running cluster needs a key rotation/restart;
  schedule it and take a snapshot first.
- **Nextcloud `trusted_domains`** must keep the readiness probe address or the
  rollout will fail; test the change with the probe.
- **2222 allowlist** may lock out legitimate external users; publish the chosen
  source policy and the host-key fingerprint.
- **Runner PR isolation** remains the default: there is no automated test
  hosted workflow; e2e runs manually.

## Non-goals

- Production Let's Encrypt (staging only; no production rate-limit risk).
- A full Vault/KMS deployment (SealedSecrets/SOPS/etcd encryption instead).
- A WAF/CDN; CrowdSec/fail2ban at the edge is the intended layer.
- Rewriting the app charts' SSO or the *arr/Jellyfin client model; P3 covers
  the realistic pieces.
- qBittorrent OIDC and Jellyfin native-client SSO (not feasible).

## Suggested order

1. **SEC-001…005** (rotate, purge, delete test users, randomize, runner).
2. **SEC-006…011** (MFA, 2222, console headers, Nextcloud, Gitea CA, OIDC hash).
3. **SEC-012…015** (console RBAC, builder split, bootstrap, secrets at rest).
4. **SEC-016…021** (local gate, egress, audit, backups, apps, pin/sign).
5. **SEC-022…025** (the OIDC/SSO reworks and policy/DR).

Each item should land with its acceptance test; do not batch the P0 items into
one release without verifying the old credentials and users are gone.
