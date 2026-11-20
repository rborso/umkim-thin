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
| **BentoPDF** | PDF toolkit, **no user limit** | new | tiny (static) | forward-auth | header + page renders (no API) | low |
| **Gotenberg** *(optional, infra)* | PDF API for automation | new | ~200–500 Mi | none (internal) | POST two PDFs, assert merged | low |
| **Vikunja** | shared task list; cheap and popular | new | ~100–200 Mi | OIDC | OIDC session + API token | low |

**Stirling PDF is dropped** — free plan caps users at 5 (see section 3).

Order matters: **Calibre-Web first** (no OIDC plumbing, no token, smallest), then
**BentoPDF** (same forward-auth pattern, no token), then **Vikunja** (proves OIDC +
token in one place), then **BookStack**. The AI stack is a separate track with its
own target.

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

## 3. PDF toolkit: BentoPDF, not Stirling

**Stirling is out.** Its licence caps the free plan at **5 users**, and the user
declined that limit. Verified from upstream's plans table: "all PDF tools and
OAuth2/OIDC SSO" are on Free, but "Users: 5". Raising it needs the Team plan. That
is a licensing constraint on the product, not a technical one, so it is settled:
no Stirling.

Two genuinely unencumbered alternatives, with very different shapes:

| | **BentoPDF** | **Gotenberg** |
|---|---|---|
| Licence | AGPL-3.0 (plus a **commercial add-on** for Hyper Compress/Kura) | MIT |
| User limit | none | none (no user model at all) |
| Shape | **client-side**: PDF work happens in the browser, the server is static files | **server-side API**: multipart in, PDF out |
| UI | yes — 50+ tools | **none** |
| API | **none** (nothing to call) | full REST |
| Memory | tiny (nginx + static assets) | ~200–500 Mi (Chromium + LibreOffice inside) |
| Gate-able | route + auth + page renders only | **real**: POST two PDFs, get a merged one |

**Recommendation: ship both, for different jobs.**

- **BentoPDF** is the user-facing toolkit. It is the closest replacement for what
  people actually used Stirling's UI for (merge, split, rotate, sign, OCR, convert)
  and it removes the server from the trust path entirely — the file never leaves
  the browser. That is a genuinely better privacy story than Stirling, and worth
  saying in the catalog description.
  - Caveat 1: **its WASM modules load from jsDelivr by default.** For a
    self-hosted install that is an outbound dependency on a third party at runtime.
    The chart must vendor the WASM (upstream documents an air-gapped mode) and the
    catalog description must not claim "fully offline" unless that is done.
  - Caveat 2: AGPL plus a **commercial** component — the README is explicit that
    bundled Hyper Compress is licensed separately ("using the engine on its own, on
    a server, through an API ... needs a separate Hyper Compress license"). Serving
    BentoPDF's own UI to end users is in scope; **do not** repurpose its
    compression engine as a platform API. Read the README's licence section before
    packaging, and record the conclusion in `docs/licenses.md`.
  - Because it has no API, its checks can only be `route_reachable`,
    `auth_challenge`, `authenticated_request_reaches_app` plus a "the tool list
    renders" check. It would be **auto-checked**, not auto-tested — and the entry
    should say so rather than pretend.

- **Gotenberg** is the *automation* piece: no UI, MIT, no user limit, and a real
  API, which makes it trivially gate-able ("POST two PDFs to /forms/libreoffice/convert
  or /merge, assert page count 2"). It is not a replacement for a PDF *app* — nobody
  wants Gotenberg as their merge tool — but it is what makes PDF operations
  available to other apps and to scripts, and it never has a licence surprise.

Decision to make: **BentoPDF alone** (simplest, user-facing, weak verification) or
**BentoPDF + Gotenberg** (both jobs, honest verification available for the half
that has an API). I would ship both, and keep the Gotenberg entry described as an
infrastructure service rather than an app.

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

## 5. Self-hosted AI across multiple nodes

Goal: **one model served across several machines' GPUs** — e.g. three PCs with one
GPU each pool into one model that none of them could hold alone. Plus Open WebUI
as the interface, and LiteLLM as the gateway.

This is outside the platform's current concept and that is fine — but it must be
built on facts, and the facts change the design substantially.

### What actually works, and what does not (verified)

| Option | Multi-node | Works on your 3× NVIDIA PCs | Verdict |
|---|---|---|---|
| **llama.cpp RPC** (`ggml-rpc-server`) | yes, by design | yes (CUDA backend) | **Its own README says "proof-of-concept … fragile and insecure. Never run the RPC server on an open network."** Usable on a trusted LAN, not a product feature. |
| **exo** | yes, automatic discovery + tensor parallel | **no — MLX / Apple Silicon only** | Excellent for Mac Studios, wrong for NVIDIA. Its benchmarks are all M3 Ultra. |
| **vLLM + Ray** | yes: TP within a node, PP across nodes | **yes** | The production answer for NVIDIA. Needs Ray, and inter-GPU networking. |
| **llama.cpp router** (`--models-dir`) | no — one node, many models | yes | Complements the above: picks *which model*, not *which machine*. |

### The finding that matters most

A published two-node vLLM experiment over **ordinary Ethernet** (one GPU per box)
measured the reality:

- tensor parallel: sharded fine, but throughput was single-digit requests/minute —
  TP all-reduces after *every layer*, and each round trip crosses the network;
- pipeline parallel: better, but ~**33 tok/s** and p95 latency ~136 s, because
  every decoded token crosses the wire once and the hop cost ~25–30 ms;
- **"the boring answer wins": one independent model per box, no distributed
  inference, gave ~10× the aggregate throughput.**

So the multi-node story is only good when the interconnect is fast (NVLink,
InfiniBand, RDMA/Thunderbolt). Over gigabit Ethernet, pooling three GPUs to serve
one model can be **slower than running three smaller models side by side** — while
feeling like a bigger number. Any plan that promises "pool your 3 PCs" must say
this out loud, because the user's expectation ("utilize all of them") is met better
by *serving three models* than by *splitting one*.

### The design that fits that finding

One URL, many backends, and the model picked by request — not by splitting one
model across a slow network.

```
Open WebUI
    |  (OpenAI format, one base URL)
LiteLLM  --- model/virtual-key/spend policy
    |            |
llama.cpp #1   llama.cpp #2   llama.cpp #3     (one per node, each on its own GPU)
```

- **Each node runs its own llama.cpp server** with `--models-dir` + `--models-max`,
  so a node can hold a few small models and swap them. This is the "boring answer"
  that measured 10× better.
- **LiteLLM** exposes all of them as one endpoint and decides which backend serves
  which model name. It is also where per-user virtual keys and budgets live, which
  is the right place for that policy.
- **Tensor/pipeline parallelism is an opt-in escape hatch**, not the default: worth
  enabling only when the model genuinely does not fit on one node *and* the link
  is fast. Document the threshold, do not promise the speed.

If the goal is specifically "run a 70B across three 24 GB cards over Ethernet",
that is doable with vLLM TP/PP + Ray, but it will be slow and should be described as
a capacity play, not a performance one.

### Phases

- **Phase A — Open WebUI + LiteLLM, external or single-node backend.** Works on the
  current cluster today. Proves the UI, OIDC, and the gateway.
- **Phase B — one llama.cpp per node**, registered in LiteLLM. This is where the
  multi-node value actually lands, and it needs no exotic networking.
- **Phase C — vLLM + Ray for one large model** across nodes, only if a model must
  be split and the interconnect supports it.

### Platform prerequisites this exposes

1. **No GPU support exists in the platform today** — no `nvidia.com/gpu` resource,
   no NVIDIA device plugin, no runtimeClass. A GPU node needs the device plugin
   installed and the charts need `resources.limits."nvidia.com/gpu"`. That is a
   platform change, not a chart change.
2. **Cross-node traffic**: LiteLLM → llama.cpp is plain HTTP on the pod/service
   network, which is fine. vLLM/Ray needs NCCL ports between nodes — a different
   firewall and NetworkPolicy story, and the platform's NetworkPolicies currently
   assume a single-node flow.
3. **Model storage**: models are tens of GB. Each node that serves a model needs the
   weights locally (a PVC per node, or a shared RWX volume). This is the largest
   storage decision in the whole plan — plan for 50–100 GB per node with a few
   models, and note that on Longhorn that is replicated.
4. **Memory**: the AI stack does not go on dev-single. It needs its own target.

### Gate strategy

Non-deterministic output, so assert shape and plumbing, never text:

- `/v1/models` lists the expected model names (via LiteLLM, so the gateway is proven)
- a completion returns 200, `choices[0].message.content` non-empty, token count > 0
- the same request routed to a **named backend** reaches that backend (LiteLLM
  exposes which deployment served it) — this is what proves multi-node actually works,
  rather than proving one node works
- time-box it: CPU or slow-Ethernet inference must not make the gate flaky; mark the
  AI checks informational if they exceed the budget

## 6. Ticketing: what already exists

Checked the catalog and docs before proposing anything: **there is no ticketing or
helpdesk app in the platform**, and nothing in the catalog references one. The word
"ticket" appears only in security docs (CSRF/incident notes), not as a feature.

What exists that people *could* use for tracking work:

| Already here | Covers |
|---|---|
| **Gitea issues** (installed) | per-repository issues, labels, milestones, assignees, kanban-ish board. Genuinely usable for "track this work" if the work is code-adjacent. |
| **Nextcloud Deck** (if enabled) | kanban boards inside Nextcloud. Personal/team task boards. |
| **Vikunja** (planned, section 4) | shared task lists — the lightest "ticketing" that fits a household. |

So the answer is: **nothing dedicated, and probably nothing needed.** Gitea issues +
Vikunja between them cover "track this task" for a single-tenant install.

**When a real ticketing system would make sense:** when there are *inbound requests
from people who are not you* — a support address, a form, a customer. That is the
**helpdesk** category (Zammad, FreeScout, osTicket), and every one of them is built
around **inbound email** (a request becomes a ticket by mail). This platform has no
mail transport, so all three would be crippled on arrival. That is the same blocker
as Snipe-IT's expiry alerts.

**Recommendation: no ticketing app.** If a dedicated one is ever wanted, the order
is: notification/mail transport first, then Zammad (the most capable of the three;
its OIDC is real), then evaluate whether Gitea issues would have been enough.

| Candidate | Verdict |
|---|---|
| **Zammad** | Best real helpdesk; heavy, needs mail + websockets. Only with mail. |
| **FreeScout** | Lightweight, but PHP and mail-centric. |
| **osTicket** | Dated; OIDC via add-on. Skip. |
| **Redmine** | Issue tracker, not a helpdesk. 600 Mi, joyless UI, and Gitea issues already do this. |
| **Plane / Vikunja / Leantime** | Project/task tools, not ticketing. Vikunja is the cheap useful one and is already in this plan. |

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
| BentoPDF is **client-side only**, AGPL-3.0 with a separately-licensed commercial compression component, and loads WASM from jsDelivr by default | upstream README + Dockerfile |
| Gotenberg is **MIT**, a server-side PDF API with **no UI and no user model** | upstream README + LICENSE |
| llama.cpp RPC exists for multi-host inference but is **"proof-of-concept … fragile and insecure"** per its own README | `tools/rpc/README.md` |
| **exo is MLX/Apple-Silicon only** — no NVIDIA support | upstream README (all benchmarks M3 Ultra; MLX backend) |
| vLLM multi-node = TP within a node, PP across nodes, via Ray | vLLM docs (parallelism and scaling) |
| Multi-node over plain Ethernet measured ~33 tok/s (PP) / single-digit RPM (TP); **one model per box was ~10x faster** | published two-node vLLM benchmark |
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

1. **PDF app choice** — BentoPDF has no user limit but is client-side only, so it
   cannot be API-tested; Gotenberg has no limit and a real API but no UI. Pick one,
   or ship both for different jobs (see the PDF section).
2. **Token hook design** — per-app adapters (recommended, honest) vs one generic
   implementation (will not fit all four).
3. **AI target sizing and networking** — Phase B needs a node with a GPU (and the
   platform has no NVIDIA device plugin yet); Phase C needs a fast interconnect
   between nodes or it is slower than Phase B. Decide which phase is the goal before
   sizing anything.
4. **Calibre-Web fork** — upstream `janeczku/calibre-web` moves slowly;
   `Calibre-Web-Automated` is the active fork. Pick one deliberately and pin it.
5. **Where does the AI stack live** — a normal catalog app with a big PVC, or a
   separate "infrastructure" concept alongside the wireguard gateway? It is the
   first thing in this plan that is not a household web app.
6. **BentoPDF's WASM hosting** — vendor it in the chart (offline, honest) or accept
   the jsDelivr CDN default (simpler, but a runtime third-party dependency)?
7. **BentoPDF licensing** — confirm the AGPL + separately-licensed Hyper Compress
   split is acceptable for how the platform distributes and serves it, and record the
   conclusion in `docs/licenses.md`.
