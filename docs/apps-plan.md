# App expansion plan: Calibre-Web, BookStack, Stirling PDF, Vikunja

Status: **proposal — nothing implemented.** Last reviewed 2026-11-20.
Companions: [`catalog.md`](catalog.md) (entry format), [`testing.md`](testing.md)
(gate taxonomy), [`versioning-plan.md`](versioning-plan.md) (chart packaging),
[`apps/testing.md`](apps/testing.md) (generated per-app status).

This document plans four apps and one self-hosted AI stack. For each app it
records what has to exist before it can be added, how it is bootstrapped, how it
is verified, and what could go wrong. It is deliberately specific about the parts
that have bitten us before (see "Lessons applied").

## Scope assumption: single-tenant

**This plan assumes one household/one organisation per install.** That changes
several things from the current catalog's assumptions:

- No per-app group restriction is needed to keep tenants apart, because there is
  only one tenant. Access control is therefore "signed in" vs "not signed in"
  (the existing Authelia `one_factor` on a wildcard domain), plus the app's own
  roles.
- The default `default-tenant` group stays, but it is no longer a security
  boundary — it is just where invited users land.
- The verification suites should stop pretending to prove tenant isolation. They
  prove **a signed-in user can do the thing**, which is what a single-tenant
  install actually needs.
- One consequence worth stating: `journeys/tenant.spec.ts` (added in 0.3.21) was
  written to prove an *invited* user works in every app. It is still useful — it
  catches "authenticates but has no rights" — but its name overpromises. In a
  single-tenant install it should be read as "a regular user, not an admin, can
  use the app".

## Lessons applied

Three defects from the Miniflux attempt (0.3.22–0.3.26) shape this plan:

1. **A chart value the console overwrites is a bug waiting to happen.**
   `applyOIDCValues` replaced the app's `oidc` map and dropped Miniflux's
   `redirectUrl`, which produced an empty `OAUTH2_REDIRECT_URL` and a hard OIDC
   failure. Fixed in 0.3.23 by merging. **Every app below must be checked for
   chart-specific OIDC fields the console does not know about**, and any new
   field must be preserved by that merge.
2. **"The API answered" is not proof a user can use the app.**
   Miniflux's `/v1/me` accepts an API token or Basic auth and *never* a session
   cookie, so the browser was logged in while the API returned 401. **Every app
   below states explicitly which credential the gate uses and why that proves
   the claim.**
3. **A claim in the catalog must resolve to a real test.** `catalog verify
   --strict` fails on an unresolved check. Every check named below must be a
   `@check:` tag on a gating test, or the entry cannot be published.

## Summary

| App | Why | Chart | Memory | Auth | Gate credential | Effort |
|---|---|---|---|---|---|---|
| **Calibre-Web** | ebooks + OPDS; the catalog has no reading app | new | ~384 Mi | forward-auth (reverse-proxy header) | trusted header | low |
| **BookStack** | real wiki; Markdown docs for the household | new | ~256 Mi | OIDC (PKCE, auto-register) | OIDC session + API token | medium |
| **Stirling PDF** | PDF toolkit; the most-used utility of the four | new | ~1024 Mi | OIDC (free plan, 5-user cap) | OIDC session + API key | medium |
| **Vikunja** | shared task list; cheap and popular | new | ~100–200 Mi | OIDC | OIDC session + API token | low |

Order matters: **Calibre-Web first** (no OIDC plumbing, no token, smallest), then
**Vikunja** (proves OIDC + token in one place), then **BookStack**, then
**Stirling** (five-user cap needs a decision first).

## The prerequisite: a token hook in the test harness

Two of the four (BookStack, Vikunja) and Stirling's API need a **per-user API
token** to let the gate act as a signed-in user. Today the harness has no way to
mint one, which is exactly why Miniflux could not be verified.

Proposed shape, minimal:

```
tests/playwright/lib/session.ts
  appToken(app, { user, pass }) -> string
```

implemented per app by a small declarative step in `ci/apps.json`, e.g.

```json
{ "name": "vikunja", "host": "vikunja.{zone}", "auth": "oidc",
  "token": { "kind": "api", "path": "/api/v1/user/token",
             "method": "PUT", "field": "token" } }
```

Each app's token retrieval differs enough that a single generic implementation
will not work; the honest design is **per-app adapters** with a shared interface,
listed in `apps.json` so the harness stays data-driven. Budget this as its own
change, gated on its own test, before either app is added.

**Calibre-Web does not need it** — it uses forward-auth — which is why it goes
first.

---

## 1. Calibre-Web

**Why:** the catalog has no reading app. Calibre-Web serves a library over the
web and over **OPDS**, so e-readers (KOReader, Moon+ Reader) can browse and
download without a browser. Cloudron ships `9.18.0 (calibre 8.4.0)` at a 384 Mi
minimum — the smallest of the four.

**Chart:** `charts/apps/calibre-web` 0.1.0. Single Deployment, SQLite, one PVC for
`/config` and one for the library (or a subPath of the same claim). No Redis, no
external database.

**Bootstrap:** there is no meaningful admin creation step — Calibre-Web ships a
default `admin/admin123`. The chart's bootstrap Job must **change that password**
before the install is reported complete, the way the Mealie chart does, and the
generated password goes into `calibre-web-secrets`, annotated
`helm.sh/resource-policy: keep`. Enable uploads and set the library path in
`app.db` during the job. If the job fails, the install must fail rather than
leave `admin123` on a public host.

**Auth — forward-auth, not OIDC.** Calibre-Web has no OIDC/LDAP in Cloudron's
manifest; it has reverse-proxy header auth
(`config_reverse_proxy_login_header_name`). That is the same model as the servarr
stack: Authelia authenticates, the app trusts a header. Configuration:

- `BEHIND_PROXY=true`
- the reverse-proxy login header name set to Authelia's `Remote-User`
- the proxy's source restricted to the cluster network

This is the reason Calibre-Web goes first: **no OIDC client, no token, no
per-app user provisioning.**

> Note: Calibre-Web is single-library with per-user shelves and read state. In a
> single-tenant install that is fine and worth saying plainly in the catalog
> description. Do not imply per-user libraries.

**Bootstrap + verify, concretely:**

- `verify.playwright: tests/playwright/journeys/calibreweb.spec.ts` (informational)
  and the gating checks below.
- Gating checks (all must be `@check:` tags in gating suites):
  - `route_reachable`, `auth_challenge` — `apps.spec.ts` (free, applies to every
    app)
  - `forward_auth_header_reaches_app` — new, in `apps.spec.ts`: with the portal
    session, the app serves its library page rather than a login form.
  - `calibreweb_default_password_changed` — new, in `auth.spec.ts`: `admin` with
    the original `admin123` is **rejected**, and with the generated password is
    accepted. This is the check that makes the bootstrap claim real.
  - `calibreweb_opds_served` — new: `/opds` answers 200 with an OPDS document.
    Proves the e-reader path, which is the whole point of the app.
- Spot-check in the journey (informational): upload a small `.epub`, confirm it
  appears in the library and downloads back byte-identical.

**Risks:** the default password is a real security issue if the bootstrap Job is
skipped; the verify check above exists specifically to catch that. Calibre-Web
also holds its library in one directory, so a restore is "put the PVC back" —
already covered by the platform backup.

## 2. BookStack

**Why:** markdown-ish wiki with books/chapters/pages, very widely used, cheap
(~256 Mi in Cloudron's manifest), and it has first-class OIDC with
auto-registration and optional group→role mapping.

**Chart:** `charts/apps/bookstack` 0.1.0. Needs MySQL (existing pattern from
mealie/paperless) plus one PVC for uploads. No Redis required.

**Bootstrap:** BookStack's first admin comes from `php artisan
bookstack:create-admin`. Upstream documents a mode built for exactly this case:

```sh
php artisan bookstack:create-admin --initial \
  --email="admin@<zone>" --name="Admin" --generate-password
```

`--initial` updates the default `admin@admin.com` user if it exists, creates an
admin if none exists, and **exits 2 if an admin already exists** (so it is safe
to re-run). `--generate-password` prints the password on stdout as the only
output, which is what a bootstrap Job needs: capture it, write it into
`bookstack-secrets` (annotated `helm.sh/resource-policy: keep`), and never
regenerate on reinstall. There is also `--external-auth-id` for creating a user
whose login is OIDC-only, which is the cleaner end state — the breakglass admin
exists, and OIDC users auto-register alongside it.

**Auth:** OIDC. Confirmed requirements from upstream docs:

- `AUTH_METHOD=oidc`, `OIDC_ISSUER` (**must** be `https://`, which our issuer is)
- `OIDC_ISSUER_DISCOVER=true` (Authelia publishes `.well-known`)
- `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET`
- `OIDC_DISPLAY_NAME_CLAIMS=name`
- callback URI `https://<host>/oidc/callback`
- **RS256 only** — Authelia's default, so no change needed
- **PKCE** is used since v24.02; Authelia supports it (and already requires
  `require_pkce` for some clients)
- `OIDC_USER_TO_GROUPS` / `OIDC_GROUPS_CLAIM=groups` are optional — **not needed
  single-tenant**, and worth leaving off so role drift cannot lock people out

**Watch for the 0.3.23 class of bug:** BookStack's chart will have `oidc.*`
fields the console does not know (`oidc.displayNameClaims`, the group-sync flags).
The console's `applyOIDCValues` `default:` branch must keep merging rather than
replacing; the regression test from 0.3.23 already guards this, and BookStack is
the first app to exercise it with more than one extra field.

**Gating checks:**

- `route_reachable`, `auth_challenge`, `authenticated_request_reaches_app`
- `oidc_sso_completes` — real browser round trip (`auth.spec.ts` pattern)
- `bookstack_api_token_writes_page` — **needs the token hook**: create a page via
  `POST /api/pages` with a token from the user's profile, then read it back.
  BookStack issues tokens per user in the UI (`/api/docs`), so the adapter is
  "sign in, open Preferences → API Tokens, create, copy". If that proves fragile,
  fall back to the browser journey + `bookstack_page_created_in_ui` and label the
  entry `auto-checked` rather than `auto-tested` — do not fake it.

## 3. Stirling PDF

**Why:** the most useful utility of the four — merge/split/rotate/OCR/sign/convert
with no data leaving the host. It has a documented REST API, which makes it
genuinely verifiable.

**One decision needed before starting:** Stirling's licensing changed. Per
upstream's plans table, **the free plan includes "all PDF tools" and "OAuth2 /
OIDC single sign-on"**, but caps **users at 5**. The Team plan raises that. For a
single-household install, 5 users is likely enough; for anything larger it is not,
and that is a product decision, not a technical one. The catalog entry must state
the cap in its description.

Also note: the login-capable images (`latest`, `latest-fat`,
`Stirling-PDF-with-login.jar`) are required for plans/admin settings;
`latest-ultra-lite` has neither. Pin a login-capable tag.

**Chart:** `charts/apps/stirling-pdf` 0.1.0. Java, ~1024 Mi. One PVC for
`/configs` (and optionally `/tessdata` for OCR languages — pre-install the
languages the platform already offers, or OCR fails at runtime).

**Bootstrap:** Stirling creates its first admin through the UI on first visit.
Options: bootstrap Job is not provided upstream, so use
**OIDC with the first OIDC user promoted**, or seed `settings.yml` with
`security.enableLogin` and an initial admin. Validate which exists in the pinned
version before writing the chart — do not assume.

**Auth:** OIDC on the free plan. Callback goes through the login build's
`/login/oauth2/code/...`; confirm the exact path for the pinned version.

**Gating checks:**

- standard route/auth trio
- `oidc_sso_completes`
- `stirling_merge_pdfs` — **the valuable one**: upload two small PDFs to the merge
  endpoint and download the result, asserting it has two pages. This is a real
  write that proves the app works, not just that it renders. Needs an API key
  (per-user) → the token hook again.

## 4. Vikunja

**Why:** shared task/project list. Cheap (Cloudron lists no minimum; expect
100–200 Mi), OIDC-capable, per-user API tokens, postgres. Popular and on-brand for
a household.

**Chart:** `charts/apps/vikunja` 0.1.0. Single container + Postgres. Vikunja
migrates on start.

**Bootstrap:** verified — Vikunja ships a CLI with a non-interactive user
command:

```
vikunja user create --username <u> --email <e> --password <p>   # --make-admin
```

(`--username` and `--email` are required; `--password` prompts if omitted, so a
Job must pass it. `--make-admin` promotes the account.) A bootstrap Job creates
the admin from a generated password in `vikunja-secrets`, matching BookStack.

**Auth:** `auth.openid.enabled=true`, with `VIKUNJA_AUTH_LOCAL_ENABLED` left on
for breakglass. Registration must be **disabled**
(`service.enableregistration=false`) or anyone can self-serve an account — worth
a gating check of its own if you want it enforced.

**Gating checks:**

- standard trio
- `oidc_sso_completes`
- `vikunja_api_token_creates_task` — **needs the token hook**: `PUT
  /api/v1/user/token` (or the UI's API-token page), then `PUT /api/v1/projects`
  + `PUT /api/v1/projects/{id}/tasks`, and read the task back. Clean REST
  surface, so this is the best first exercise of the token hook.

---

## 5. Self-hosted AI: Open WebUI + llama.cpp + LiteLLM

**This is out of the platform's current concept, and that is fine** — but it must
be built with its own sizing and its own failure modes, not squeezed in.

### What the pieces actually are (verified)

- **llama.cpp's server is OpenAI-compatible** (`/v1/chat/completions`,
  `/v1/responses`, `/v1/embeddings`) *and* now ships a **built-in model router**:
  `--models-dir`, `--models-preset`, `--models-max N` (default 4, 0 = unlimited).
  **So "multiple llama.cpp containers" is probably unnecessary** — one container
  with `--models-dir` serves many models and loads/unloads them. Start there;
  only add containers if you need them on different nodes (e.g. one CPU node, one
  GPU node).
- **LiteLLM proxy** is an OpenAI-format gateway over 100+ providers, with
  *virtual keys, spend tracking, load balancing*. That is a different job from
  llama.cpp's router: **router = which local model; LiteLLM = which backend, for
  whom, at what budget.** Both are worth having, in that order.
- **Mixing licences:** LiteLLM is MIT except `litellm/enterprise/`. n8n and
  Stirling's newer features show the pattern to watch for — check the pinned
  version's licence directory before shipping.

### Memory: the binding constraint

| Model | Weights (Q4) | Realistic total with KV | Fits dev-single (~2 GB free)? |
|---|---|---|---|
| 1–3B | 0.7–2 GB | 1.5–3 GB | borderline; 3B will not |
| 7–8B | 4–5 GB | 6–7 GB | **no** |
| 14B+ | 9 GB+ | 12 GB+ | no |

ha-dev nodes are 8 GiB each and already run Longhorn. **There is no GPU support
in the platform today** (no `nvidia.com/gpu`, no runtimeClass), so this is CPU
inference at a few tokens/second.

**Therefore the AI track needs a target of its own**, and the plan is:

- **Phase A (works today):** Open WebUI pointed at an **external**
  OpenAI-compatible endpoint. No local weights. Proves the UI, OIDC and the
  integration without touching node memory. Fully gate-able.
- **Phase B (needs a bigger node):** llama.cpp server with `--models-dir` and a
  1–3B model for "local, private, slow". One container. Verify with a real
  completion request, not just a health check.
- **Phase C (optional):** LiteLLM in front, so Open WebUI points at one URL and
  the routing/limits live in LiteLLM. Only worth it once there is more than one
  backend.

**Do not** add a `llama.cpp` catalog entry as if it were a web app: it has no UI,
no user model, and its model cache needs a PVC of tens of GB. If it ships, it is
an *infrastructure* app (like the wireguard gateway), documented as such.

### Gate strategy for AI

A completion is non-deterministic, so the gate must not assert text. It should
assert **shape and plumbing**:

- the backend answers `/v1/models` with at least one model
- a completion request returns 200 with `choices[0].message.content` non-empty
  and a token count > 0
- Open WebUI can list models *through* LiteLLM (when Phase C exists)
- the request goes through the portal session (auth), not around it

Time-box it: CPU inference for a short prompt should be seconds, not minutes. If
it exceeds the gate's budget, mark the AI checks informational rather than
letting a slow node make the whole gate flaky.

## What each app still needs before it is "added"

Per `docs/catalog.md` and the release trains, an app is not done when it installs:

1. Chart in `umkim-charts` (version bump, licence/NOTICE, packaged into the
   mirror; first release needs no N-1 — `ci/check-mirror-versions.py` allows it).
2. Catalog entry with route, exposure, auth, `values`, and `verify`.
3. Every `verify.checks` name carried by an `@check:` tag in a **gating** suite,
   so `catalog verify --strict` passes.
4. `docs/apps/testing.md` regenerated (`ci/gen-app-docs.py`), which `static.sh`
   checks.
5. Both targets installed and green: `ci/test-existing.sh single|ha --pin
   --journeys`, plus `ci/assert.sh`.
6. A human verification log (`verify.human`) for the chart version tested, or the
   entry shows no "verified" badge — which is the honest outcome if nobody has
   used it yet.

---

## 6. Inventory / asset management: the recommendation

You asked what makes sense here. The category splits in two, and only one half
fits this platform.

| Tool | What it actually tracks | Fit |
|---|---|---|
| **Snipe-IT** | IT **assets**: "laptop 12 is assigned to Alice, warranty ends 2027-03, licences used 40/50" | Best candidate. LDAP + OIDC, mature, the de-facto self-hosted choice. |
| **NetBox** | **Network** source of truth: racks, IPAM, VLANs, circuits, cable runs | Wrong shape. It is DCIM for a datacenter, not for a household. |
| **Grocy** | Household **stock**: groceries, consumables, expiry | On-brand for this product, but it is a different question ("what's in the fridge"). |
| **HomeBox** | Household **possessions**, with photos and warranties | **Already installed** — covers the domestic case. |
| **Part-DB / InvenTree** | Electronics components / parts inventory | Niche; wrong audience. |

**Recommendation: Snipe-IT, and only if you have a real IT-asset need.**

The reasoning matters more than the pick:

- The domestic "what do I own, when does the warranty expire" case is **already
  covered by HomeBox**, which is in the catalog. Adding Snipe-IT for that would
  be duplication.
- NetBox is genuinely excellent and genuinely wrong here — it models racks and
  IP allocations, and you manage the network with WireGuard, Calico and a
  single VIP. It would be a tool with no data to hold.
- Snipe-IT earns its place only in the "this household also runs a small IT
  estate" scenario: assigned laptops, licence seats, warranty expiries. That is a
  real need for some users and zero need for others.

So: **Snipe-IT is a candidate, not a default.** It needs an LDAP or OIDC client,
MySQL, a scheduler sidecar for its expiry alerts — and that scheduler sends
**email**, which this platform does not have. Without mail, Snipe-IT's alerting
(its main value over a spreadsheet) is dead. That is the same blocker as the
helpdesk apps, and it is more important than the app choice.

**Therefore:** the honest order is *notification transport first* (ntfy or a
SMTP smart host), then Snipe-IT. Until then, HomeBox covers inventory.

---

## Verified facts vs assumptions

Every load-bearing claim above was checked against upstream source or docs. Listed
so a reader can tell which is which.

| Claim | Verified how |
|---|---|
| Calibre-Web has reverse-proxy header auth (`config_reverse_proxy_login_header_name`) | present in `cps/config_sql.py` |
| Calibre-Web ships `admin` / `admin123` | `cps/constants.py`: `DEFAULT_PASSWORD = "admin123"` |
| BookStack OIDC: RS256 only, discovery supported, PKCE since v24.02, callback `/oidc/callback` | upstream OIDC docs |
| BookStack `bookstack:create-admin --initial --generate-password` | upstream commands docs |
| Vikunja CLI `user create --username --email --password [--make-admin]` | `pkg/cmd/user.go` |
| Vikunja OIDC: `auth.openid.enabled`, registration off via `service.enableregistration` | upstream config docs |
| llama.cpp server is OpenAI-compatible **and** has a model router (`--models-dir`, `--models-max`) | `tools/server/README.md` |
| LiteLLM is an OpenAI-format gateway with virtual keys/spend tracking; MIT except `litellm/enterprise/` | upstream README + LICENSE |
| Stirling: **OIDC is in the free plan, capped at 5 users**; login builds required | upstream plans table |
| Cloudron memory minimums quoted (384/256/1024 Mi) | Cloudron app API manifests |

Assumptions **not** yet verified, and flagged as such:

- Stirling's exact OIDC callback path and whether an admin can be seeded without
  the UI — must be checked against the pinned version before writing the chart.
- Whether Calibre-Web's OPDS endpoint is `/opds` on the pinned version.
- BookStack's API-token creation flow being scriptable without the UI (the token
  hook's feasibility).
- Actual memory use of each on this platform (Cloudron's numbers are their
  minimums, not ours).

## Open questions to settle before implementation

1. **Stirling's 5-user free cap** — acceptable, or does this need the Team plan?
   This is a licensing/product decision, and it decides whether Stirling ships at
   all.
2. **Token hook design** — per-app adapters (recommended, honest) vs one generic
   implementation (will not fit all four).
3. **AI target sizing** — is there a node with enough RAM/GPU for Phase B, or does
   the AI track stop at Phase A?
4. **Calibre-Web fork** — upstream `janeczku/calibre-web` moves slowly;
   `Calibre-Web-Automated` is the active fork. Pick one deliberately and pin it.
5. **Where does the AI stack live** — a normal catalog app with a big PVC, or a
   separate "infrastructure" concept alongside the wireguard gateway?
