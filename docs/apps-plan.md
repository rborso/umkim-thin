# App expansion plan: Calibre-Web, BookStack, Vikunja + a multi-node AI stack

Status: **proposal — nothing implemented.** Last reviewed 2026-11-20.
Companions: [`catalog.md`](catalog.md) (entry format), [`testing.md`](testing.md)
(gate taxonomy), [`versioning-plan.md`](versioning-plan.md) (chart packaging),
[`apps/testing.md`](apps/testing.md) (generated per-app status), and
[`gpu-agent.md`](gpu-agent.md) — the Option B design chosen in §4.

This document plans three apps and one self-hosted AI stack. For each app it
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
| **Vikunja** | shared task list; cheap and popular | new | ~100–200 Mi | OIDC | OIDC session + API token | low |

Order changed during implementation, for a reason worth keeping: **Calibre-Web
went first as planned and was dropped** on reading its source - it cannot admit a
freshly invited directory user (section 1). **Vikunja** was next and is done: it
proves OIDC, tenant auto-registration and the token hook in one place. **BookStack**
is the remaining app. The AI stack is a separate track with its own target
(section 4).

**Declined, with reasons (section 5):** PDF toolkits (Stirling's 5-user cap),
inventory (HomeBox already covers it), ticketing (nothing inbound-mail based can
work without a mail transport).

## The prerequisite: a token hook in the test harness

Both OIDC apps (BookStack, Vikunja) need a **per-user API token** to let the gate
act as a signed-in user. Today the harness has no way to
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

## 1. Calibre-Web — dropped, with the reason

**Decided against after reading the source, not the marketing copy.** The plan
above assumed it was the easy one because it "has no OIDC client, no token, no
per-user provisioning". That last clause is false, and it is disqualifying:

- **Header login never creates an account.**
  `load_user_from_reverse_proxy_header` looks the name up in Calibre-Web's own
  `user` table and returns `None` if it is not there. Forward-auth supplies
  `Remote-User`, the lookup misses, and the request falls through to the login
  form.
- **LDAP does not either.** `login_post` resolves `user` *first* and only then
  calls `services.ldap.bind_user`, so an unknown directory account cannot get in
  by LDAP either. There is a bulk "import LDAP users" admin button, but it pulls
  a whole group and is not a per-login hook.
- **There is no generic OIDC.** `oauth_bb.py` imports exactly the GitHub and
  Google blueprints.

So an invited Authelia user - the gate this platform now requires - would reach a
login form and stop. The app also needs a pre-existing `metadata.db` to point at,
so a bootstrap has to seed a library before anything works.

The remaining options were (a) ship it admin-only and document that a real tenant
cannot get in, (b) add a per-user provisioning path, or (c) drop it. It was
dropped: (a) ships a user-visible lie for a reading app whose point is that
people read, and (b) is a new platform capability, not a cheap app.

**Recorded so it is not re-proposed** without an answer to "how does a freshly
invited directory user get an account?" If that answer appears - a provisioning
Job, or a Calibre-Web fork with real OIDC - the rest of the original analysis
(chart shape, OPDS, the default `admin123` password that must be changed) still
stands:

- Serves a library over the web and over **OPDS**, so e-readers (KOReader,
  Moon+ Reader) work without a browser. Cloudron ships `9.18.0 (calibre 8.4.0)`
  at a 384 Mi minimum.

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

## 3. Vikunja

**Why:** shared task/project list. Cheap (Cloudron lists no minimum; expect
100–200 Mi), OIDC-capable, per-user API tokens, postgres. Popular and on-brand for
a household.

**Chart:** `charts/apps/vikunja` 0.1.0. Single container + Postgres. Vikunja
migrates on start.

**Bootstrap — corrected against 2.8.0, which is the version actually shipped.**

Vikunja does ship a non-interactive user command, and the chart runs it directly
(the image is `FROM scratch`, so there is no shell for a script):

```
vikunja user create --username <u> --email <e> --password <p>
```

`--username` and `--email` are required and `--password` prompts if omitted.
This part was right.

**`--make-admin` does not exist, and no free install can have an instance admin.**
Two separate facts, both verified in a container:

- `user create` never sets `is_admin` (there is no first-user-becomes-admin
  logic anywhere in `CreateUser`).
- The command that *would* set it, `user set-admin <u> --admin`, refuses on a
  free instance: `IsFeatureEnabled(FeatureAdminPanel)` is false without a licence,
  so it calls `log.Fatalf("The admin-panel license feature is not active")`.
  **It also exits 0 while refusing**, so a chart that checked only the exit
  status would silently ship a non-admin.

That is a product decision, not a bug to work around: the vendor's pricing page
lists the admin panel, audit log and time tracking as paid "Pro" features, and
self-hosting is otherwise free for any number of users. So the chart creates the
breakglass *account* and documents that there is no instance admin. A self-hosted
Vikunja is fully usable that way — projects, tasks, sharing and OIDC all work,
because `Project.CanCreate` returns true for an ordinary user. What is missing is
the admin panel itself.

**Auth:** `auth.openid.enabled=true` with `auth.local.enabled=true` for
breakglass. The provider entry is a nested map, so the chart renders `config.yml`
rather than env vars (both work; the file keeps the secret out of the
environment). The secret itself goes in through upstream's `.file` indirection —
`VIKUNJA_AUTH_OPENID_PROVIDERS_<KEY>_CLIENTSECRET_FILE` — verified both ways: with
it the provider loads and `/api/v1/info` advertises it, without it the app logs
`required key 'clientsecret' is missing` and drops the provider. Registration must
be **disabled** (`service.enableregistration=false`) or anyone can self-serve an
account.

**Gating checks:**

- standard trio
- `oidc_sso_completes`
- `tenant_api_vikunja` — an invited tenant's first use: OIDC auto-registration,
  then create a project and see it in their own list.
- `vikunja_api_token_creates_task` — the token hook's first real exercise. The
  endpoint in the first draft of this document was **wrong**: it is
  `PUT /api/v1/tokens` (with a title, an expiry and explicit permissions, and the
  cleartext returned once), not `PUT /api/v1/user/token`, which only refreshes a
  JWT. Then `PUT /api/v1/projects` + `PUT /api/v1/projects/{id}/tasks`, and read
  the task back.

---

## 4. Self-hosted AI: one entrypoint, many backends (multi-node)

Goal, restated to match what is actually wanted: **a single URL that pools several
inference servers**, so a fleet of machines shares the load — and can serve several
different models at once. Not "split one model across machines".

That distinction is the whole design, and it is also the better-performing one.

### The architecture

```
Open WebUI
    |   one base URL, OpenAI format
LiteLLM proxy            <- the single entrypoint: load balancing, model list,
    |                       fallbacks, virtual keys, spend
    +-- llama.cpp @ node-a   gpu-a, models: qwen3-8b, llama-3.2-3b
    +-- llama.cpp @ node-b   gpu-b, models: qwen3-8b, mistral-7b
    +-- llama.cpp @ node-c   gpu-c, models: qwen3-8b
```

Two capabilities do the work, both verified:

**1. llama.cpp serves *many* models from one process.** Router mode:
`--models-dir /models`, `--models-max N` (default 4; 0 = unlimited),
`--models-memory-margin` (unload LRU models until a new one fits),
`--models-autoload`. It exposes each GGUF at `/v1/models` and loads/unloads on
demand. So "one llama.cpp per *node*", not per model — which changes the earlier
plan and removes most of the container sprawl.

**2. LiteLLM load-balances by model name.** Multiple `model_list` entries sharing
the same `model_name` form a pool; a request for that name is distributed across
them. Verified from upstream's load-balancing docs:

- default strategy is **`simple-shuffle`** (recommended for production), with
  `weight`, `rpm`, `tpm`, `rpd`, `tpd` per deployment;
- other strategies: `least-busy`, `latency-based-routing`,
  `usage-based-routing-v2`, `cost-based-routing`, priority via `order`;
- **cooldowns**: a failing deployment is removed from the pool automatically
  (429 → 5 s; >50% failures in a minute; 401/404/408), then re-introduced;
- **retries + fallbacks** across deployments, with `allowed_fails_policy` per
  deployment;
- **`x-litellm-model-id`** response header names which deployment served a
  request — this is what makes multi-node *testable*;
- session affinity exists if a conversation must stay on one backend
  (`optional_pre_call_checks: ["session_affinity"]`).

So load balancing across nodes costs no custom code: it is a `model_list` with
repeated `model_name`s.

### Sticky sessions: default on, and the trap to avoid

**Sticky sessions should be the default** — a conversation that keeps landing on a
different backend loses its prompt cache and feels erratic. The configuration below
is the one to use:

```yaml
# litellm config
general_settings:
  redis_host: redis.<ns>.svc.cluster.local      # required: pins live in Redis
  redis_port: "6379"

router_settings:
  routing_strategy: simple-shuffle
  redis_host: redis.<ns>.svc.cluster.local
  redis_port: "6379"
  optional_pre_call_checks:
    - session_affinity                            # the sticky switch
  deployment_affinity_ttl_seconds: 3600
```

Four things that matter, three of which are easy to get wrong:

1. **Session affinity needs Redis.** The pin map lives in the router cache; with
   Redis configured it is shared across LiteLLM replicas, without it each replica
   has its own pins and stickiness silently breaks under >1 replica. So the
   deployable unit is **LiteLLM + Redis**, not LiteLLM alone.

2. **`session_affinity`, not `deployment_affinity`.** Both exist and the wrong one
   looks right. `deployment_affinity` pins **per API key**: every request bearing
   that key — every user, every conversation — goes to the same backend, which
   silently turns the pool back into one backend per key and defeats the point.
   `session_affinity` pins per session id, which is what you want. Do not enable
   `deployment_affinity` on a shared proxy.

3. **The client must send a session id.** Stickiness is keyed on
   `x-litellm-session-id` (or `x-<vendor>-session-id`, or `metadata.session_id`).
   Without one, every request is independently shuffled and "sticky" does nothing.
   Open WebUI must pass a conversation id; if it does not, add a tiny shim or set
   the header in the proxy in front. **This is the piece to verify first** — a
   sticky config with no session id is indistinguishable from no stickiness.

4. **Affinity applies to async router paths** (`/v1/chat/completions`,
   `/v1/responses`, `/v1/embeddings`). The sync SDK methods skip it, and the
   `usage-based-routing` (v1) strategy skips it on every call.

**Open WebUI sends no session id** — verified in its `routers/openai.py`, which sets
Azure `api-key`/`api-version` headers and nothing session-related. So as shipped, the
sticky config above would be inert. Two ways to fix it, and this is a decision to
make before building:

- **Cheapest:** set `ENABLE_FORWARD_USER_INFO_HEADERS=true` in Open WebUI; it then
  sends `X-OpenWebUI-User-Id` per request. That makes chat sticky *per user* rather
  than *per conversation* — usually the right granularity for prompt-cache reuse on a
  single-GPU-per-node fleet, and it needs no code.
- **Precise:** a tiny shim in front of LiteLLM that maps a conversation id to
  `x-litellm-session-id`. More moving parts; only worth it if per-conversation
  pinning is genuinely needed.

**An alternative worth knowing:** LiteLLM's *cache-aware routing* with
`kubernetes_pod_discovery` targets a headless Service and keeps requests from the
same session on one pod for prompt-cache reuse; requests without a session id fall
back to round-robin. That is a good fit if the llama.cpp backends are behind one
headless Service rather than listed individually — and it means one llama.cpp
Deployment scaled to N instead of N separate per-node Deployments. Pick one topology
and stay consistent.

There is also a stale-key hazard: if `deployment_affinity` was ever enabled, its
pins **linger in Redis** and keep pinning after the setting is turned off. A config
change then appears not to take effect. On every reconcile, clear them:

```sh
kubectl -n <ns> exec deploy/redis -- sh -c \
  'redis-cli --scan --pattern "deployment_affinity:*" | xargs -r redis-cli DEL'
```

Run it only while `deployment_affinity` is off — deleting pins while the feature is
in use just moves traffic around.

### The llama.cpp backend: what each node runs

Each node is one `llama-server` process, not one per model. Two shapes, pick per
node:

**A. One model, pinned** — simplest, and the right default for a pool:

```sh
llama-server -hf Qwen/Qwen3-8B-GGUF:Q4_K_M \
  --alias qwen3-8b \
  --host 0.0.0.0 --port 8080 \
  -c 8192        # context size; drives KV-cache memory
  -ngl 99        # layers on GPU; 99 = all
  -np 1          # parallel request slots; each costs KV memory
  --jinja
```

- `--alias` sets the model id returned by `/v1/models`. **LiteLLM's `model_name`
  must match it**, or requests 404 on a name the backend does not answer to.
  Leaving `--alias` off makes llama.cpp derive the id from the GGUF filename, which
  then drifts whenever you change the file — always set it.
- `-c` (context) and `-np` (slots) multiply into KV-cache memory. `-c 8192 -np 1`
  is a safe start on a 24 GB card; raising either is the usual cause of a server
  that starts and then OOMs under load.
- `-ngl 99` offloads every layer. Lower it only if the model does not fit.

**B. Several models, router mode** — for a node that should offer more than one:

```sh
llama-server --host 0.0.0.0 --port 8080 \
  --models-dir /models \
  --models-max 2 \
  --models-memory-margin 512
```

Each GGUF in `/models` becomes a model id. Multi-file models (multimodal, sharded)
go in a **subdirectory**, with the projector file named `mmproj*`.

`--models-max 2` and `--models-memory-margin 512` (MiB) are the ones that matter on
a single GPU: without them the router will try to hold more models in VRAM than the
card has, and the OOM kills the server rather than evicting a model. Set
`--models-max` to what actually fits, and let the margin keep headroom.

The upstream image is `ghcr.io/ggml-org/llama.cpp`, tag `server-cuda` for NVIDIA (a
plain `server` tag for CPU). Each node needs the GGUF files on a volume — see the
storage prerequisite below.

### What the config looks like

One `model_name` repeated per node = a pool. Distinct names = separate models in
Open WebUI's picker.

```yaml
# litellm config.yaml
model_list:
  # One pool: the same model on every node, requests spread across them.
  - model_name: qwen3-8b
    litellm_params:
      model: openai/qwen3-8b            # llama.cpp speaks the OpenAI API
      api_base: http://llama-a:8080/v1
      api_key: "none"
      weight: 1
  - model_name: qwen3-8b
    litellm_params:
      model: openai/qwen3-8b
      api_base: http://llama-b:8080/v1
      api_key: "none"
      weight: 1
  - model_name: qwen3-8b
    litellm_params:
      model: openai/qwen3-8b
      api_base: http://llama-c:8080/v1
      api_key: "none"
      weight: 1

  # A different model, only on the node that can hold it.
  - model_name: mistral-7b
    litellm_params:
      model: openai/mistral-7b
      api_base: http://llama-b:8080/v1
      api_key: "none"

router_settings:
  routing_strategy: simple-shuffle
  redis_host: redis.<ns>.svc.cluster.local
  redis_port: "6379"
  optional_pre_call_checks: [session_affinity]
  deployment_affinity_ttl_seconds: 3600
  num_retries: 2
  allowed_fails: 2
  cooldown_time: 30
```

The `api_base` values must be reachable from LiteLLM, and each `model_name` must
equal the `--alias` that backend advertises (see the previous section), or the
request 404s against a name the backend does not answer to.

Pin a **PEP 440 tag** for LiteLLM, not `:main` or `:latest` — `session_affinity`
landed in the 1.100 line, so anything from about `v1.100` onward has it, and a
pinned tag is what makes "the sticky behaviour you tested" the behaviour you get.

### Where tensor/pipeline parallelism fits (and why it is not the default)

Splitting one model across nodes is possible (vLLM + Ray: TP within a node, PP
across nodes) and is *not* what is wanted here. It is worth keeping in the
document only as a caveat, because a published two-node test over ordinary
Ethernet measured ~33 tok/s with pipeline parallel and single-digit requests/min
with tensor parallel, while **one model per box was ~10× faster**. Pooling
machines over gigabit to serve one model is a capacity trick, not a speed-up.

Also relevant: **exo** (the popular "pool your devices" project) is MLX-based and
**Apple Silicon only** — it will not drive NVIDIA PCs. **llama.cpp RPC**
(`ggml-rpc-server`) does split across hosts but its own README calls it
"proof-of-concept … fragile and insecure. Never run the RPC server on an open
network."

None of that is needed for the load-balanced design, which is why it is the plan.

### Same model everywhere, or different models per node?

Both work, and they compose:

- **Same `model_name` on several nodes** → LiteLLM spreads requests across them.
  This is the "pool the machines" answer: one model name, N× the throughput, and
  if one node dies the others keep serving.
- **Different models per node** → each is its own `model_name` and Open WebUI
  shows them all in its model picker. This is the "serve many models" answer.
- **Mix**: pool the popular model across all nodes, and pin a large model to the
  one node with enough VRAM.

The one constraint worth stating: a *pooled* model name must be served by
backends that actually produce comparable output — same model, same quantisation.
LiteLLM will happily route `qwen3-8b` to a Q4 on one node and a Q8 on another, and
the user will notice. Name them `qwen3-8b` and `qwen3-8b-hq` if the quants differ.

### Phases

- **A — LiteLLM + Open WebUI + one CPU-only llama.cpp.** Ships on the current
  cluster with no GPU, no driver and no device plugin. Proves the entrypoint, OIDC,
  the model list and the gateway. This is the phase that unblocks everything else.
- **B — second and third llama.cpp backends** joining the same model pool — CPU or
  GPU, either works; the pool does not care. This is where the multi-node goal is
  actually met, and it needs no exotic networking.
- **C — GPU nodes, optional and independent.** The driver work in
  [`gpu-agent.md`](gpu-agent.md). Improves speed; changes no app contract.
- **D — optional**: a large model split across nodes (vLLM/Ray) *only* if a model
  must be served that no single node can hold, and the interconnect is fast.

### GPU: not a prerequisite for this plan

**CPU-only inference is first-class.** llama.cpp's plain `server` image runs on CPU
with `-ngl 0` and no GPU resource at all, so the whole AI stack in this document
works on today's GPU-less cluster:

| Node | Image | `-ngl` | Resource |
|---|---|---|---|
| no GPU | `llama.cpp:server` | `0` | none |
| GPU | `llama.cpp:server-cuda` | `all` | `nvidia.com/gpu: N` |

That means the phases below ship **before** any driver work. GPU support is an
enhancement to the same app contract, not a gate on it — which is a much better
place for it in the roadmap, and it means the AI track and the driver track can
proceed independently.

The chosen design for adding GPU support is Option B — a root host `gpu-agent`
mirroring the storage agent — and it is specified in full in
[`gpu-agent.md`](gpu-agent.md): Rocky 10 only, DKMS, cordon/drain before the
required reboot, multi-GPU per host, and fixture-based tests. It is deliberately not
repeated here.

Two facts from that document worth knowing while reading this one:

- A reboot **is** required after a driver install (NVIDIA's requirement; DNF can
  report it with `needs-restarting -r`), so the driver step must happen **before any
  app is installed** on that node.
- Installing a driver and toolkit does **not** make `nvidia.com/gpu` schedulable —
  a device plugin must run.

### Platform prerequisites this exposes

1. **No GPU support exists in the platform today** — but per the previous section
   that only affects Phase C. Phases A and B are CPU-only and need none of it.
2. **Cross-node traffic**: LiteLLM → llama.cpp is plain HTTP on the pod/service
   network, which the existing NetworkPolicies handle. (vLLM/Ray in Phase C is the
   exception — NCCL needs its own ports and the policies assume a single node.)
3. **Model storage**: each node serving models needs the GGUFs locally — a PVC per
   node, tens of GB. On Longhorn that is replicated, so budget accordingly. Shares
   nothing with the split-model case, which needs the *same* weights everywhere.
4. **Memory**: CPU inference is viable on dev-single only for small models (~1–3B)
   and will be slow; a node with a GPU is required for anything bigger. Do not size
   the AI target on the assumption GPUs exist.
5. **Registry**: the CUDA image is several GB, the CPU `server` image is far
   smaller, and the model weights are larger than either. Neither image belongs in
   the chart mirror (which is seeded into a ConfigMap, and whose budget the API
   already enforces). Weights come from a PVC the operator populates, or from an
   init container with a bounded timeout — decide which, and do not make an install
   depend on a multi-GB download succeeding.

### Gate strategy

Non-deterministic output, so assert shape and plumbing, never text:

- `llm_models_listed` — `/v1/models` through LiteLLM contains the expected names.
- `llm_completion_answers` — 200, non-empty `choices[0].message.content`, token
  count > 0.
- `llm_load_balances_across_backends` — send N requests **without** a session id and
  assert **more than one distinct `x-litellm-model-id`** came back. Without this,
  a single-backend setup would pass everything else.
- `llm_session_sticks_to_one_backend` — the sticky half: send N requests **with the
  same `x-litellm-session-id`** and assert exactly **one** distinct model-id. This
  is the check that catches the classic failure: sticky enabled but the client never
  sends an id, which looks identical to stickiness that works until you read the
  headers.
- `llm_failover_keeps_serving` — stop one backend and assert requests still
  succeed (cooldown/fallback), which is the reliability half of "pool the machines".
- `llm_cpu_mode_requests_no_gpu` — the CPU-only contract: the pod spec must contain
  **no** `nvidia.com/gpu` limit, and the deployment must reach Ready on a cluster
  with no device plugin. This is the check that keeps CPU mode honest and stops a
  GPU requirement creeping into the default install.

The balancing and sticky checks are deliberately a pair: with only one of them you
cannot tell whether affinity is configured or merely absent. Both are cheap (a
handful of short completions) and both are deterministic — no text assertion.

Time-box all of it; CPU inference must not make the gate flaky. If it exceeds the
budget, mark the AI checks informational.

There is also a **pre-flight** check that belongs before any of that, because it
turns a wedge into a message: `assert.sh` should verify that a GPU node advertises
`nvidia.com/gpu` and that `nvidia-smi` inside a trivial pod lists at least one
device. Without it, "the AI app will not start" is indistinguishable from a broken
image, a bad model path, or a missing driver.

### Per-user API keys, for clients outside the chat UI

Question that comes up once the stack works: can a user mint their own key and
use it from a script, an editor plugin, or a phone app? **Yes, but it needs a
database, which the shipped chart does not have.**

All of this was verified against `main-v1.83.7-stable` in a container, not read
off the docs.

**What does not work.** With no `DATABASE_URL`, `POST /key/generate` answers:

```
500 {'error': 'DB not connected. See .../proxy/virtual_keys'}
```

Virtual keys are stored rows, so there is nothing to store them in. That is the
whole reason the chart has no key management today.

**What works once a database exists.**

| Role | Can generate its own key |
|---|---|
| `proxy_admin` | yes |
| `internal_user` | **yes** |
| `internal_user_viewer` | no — 401 "Only proxy admin can be used to generate..." |

The last one is the default trap: a user created as a viewer cannot self-serve,
and the error names the requirement rather than the setting. `internal_user` is
the role that grants key management without admin.

**Verified behaviour with a database:**

- `internal_user` → `POST /key/generate` → 200, and the returned key completes a
  chat request for real (`200`, model answered).
- `GET /key/list` as that user returned **only their own** keys — the admin's were
  not in the list.
- `GET /key/info?key=<someone else's>` as that user → **403** "You are not allowed
  to access this key's info. Your role=internal_user". Scoping is enforced, not
  just hidden in the UI.

**Two ways to expose it, and they differ in what the user gets:**

1. **LiteLLM virtual keys** (what the checks above exercise). Needs Postgres. The
   user's key works against `https://<chat host>/v1/...` — the gateway speaks the
   OpenAI API — and carries per-key budget, rate limits and spend tracking.
2. **Open WebUI API keys** (`ENABLE_API_KEYS=true`). Needs nothing new: WebUI
   already has a database. But the key is scoped to *Open WebUI's* API
   (`/api/chat/completions`), not the gateway's OpenAI surface, so an editor
   plugin pointed at `/v1` would not work. It is the cheaper option if the goal is
   "let my phone app talk to my chat instance" rather than "let me point any
   OpenAI client at my house".

**What implementing option 1 costs:** a Postgres StatefulSet/Deployment plus a
PVC in the `ai` chart, `DATABASE_URL` and `STORE_MODEL_IN_DB` on LiteLLM, a first
boot that runs migrations (it took ~30 s here), and the users themselves — today
nobody exists in LiteLLM's table, because WebUI authenticates its users and hands
the gateway one master key. Granting SSO users their own virtual keys means
provisioning a LiteLLM user per directory user, which is a real piece of work, not
a chart flag.

That last point is the honest blocker, and it is why this is written down rather
than shipped: the gateway currently has **one** identity. Giving each household
member their own key is a provisioning problem across two user databases that
know nothing about each other.

## 5. Considered and declined

Recorded so the same ground is not re-covered later. Each of these was evaluated
and explicitly dropped.

### Inventory / asset management — already covered

The domestic case ("what do I own, when does the warranty end") is **HomeBox**,
which is already in the catalog. Nothing else is needed:

- **Snipe-IT** is IT-asset tracking for a small estate (assigned laptops, licence
  seats). Its main value over a spreadsheet is expiry alerts **by email**, which
  this platform cannot send, so it would arrive crippled.
- **NetBox** models racks, IPAM and circuits — a datacenter tool with no data to
  hold here.
- **Grocy** is household stock, a different question again.

If a real IT-asset need ever appears, the order is: notification transport first,
then Snipe-IT.

### Ticketing / helpdesk — nothing exists, and nothing is needed

There is **no ticketing or helpdesk app** in the platform today, and nothing in the
catalog references one (the word "ticket" appears only in security notes).

What already covers "track this work":

| Already here | Covers |
|---|---|
| **Gitea issues** (installed) | per-repo issues with labels, milestones, assignees |
| **Nextcloud Deck** (if enabled) | kanban boards inside Nextcloud |
| **Vikunja** (this plan, §3) | shared task lists — the lightest option |

A *real* helpdesk (Zammad, FreeScout, osTicket) exists to turn **inbound email**
into tickets. With no mail transport here, all three would be crippled on arrival —
the same blocker as Snipe-IT. **Recommendation: no ticketing app.** If one is ever
wanted: mail transport first, then Zammad, and re-check whether Gitea issues would
have done.

### PDF toolkits — dropped

- **Stirling PDF** — free plan caps **5 users**; the limit was rejected.
- **BentoPDF** — no user limit and a better privacy story (client-side), but **no
  API**, so it can only be `auto-checked`, never `auto-tested`; also loads its WASM
  from jsDelivr by default and carries a separately-licensed compression component.
- **Gotenberg** — MIT, real API, no UI.

Dropped by decision. If PDF returns, the honest split is BentoPDF for the UI and
Gotenberg for the automatable half.

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

## Verified facts vs assumptions

Every load-bearing claim above was checked against upstream source or docs. Listed
so a reader can tell which is which.

| Claim | Verified how |
|---|---|
| Calibre-Web has reverse-proxy header auth (`config_reverse_proxy_login_header_name`) | present in `cps/config_sql.py` |
| Calibre-Web ships `admin` / `admin123` | `cps/constants.py`: `DEFAULT_PASSWORD = "admin123"` |
| BookStack OIDC: RS256 only, discovery supported, PKCE since v24.02, callback `/oidc/callback` | upstream OIDC docs |
| BookStack `bookstack:create-admin --initial --generate-password` | upstream commands docs |
| Vikunja CLI is `user create --username --email --password`; there is **no** `--make-admin` | `pkg/cmd/user.go` v2.8.0 |
| `user set-admin` refuses on a free instance (`admin_panel` is a paid feature) **and exits 0** while refusing | `pkg/cmd/user.go`, `pkg/license/license.go` |
| `user create` never sets `is_admin`; an ordinary user can still create projects | `pkg/user/user_create.go`, `pkg/models/project_permissions.go` |
| Vikunja reads provider secrets from `..._CLIENTSECRET_FILE` (verified: provider loads with it, is dropped without it) | container test, v2.8.0 image |
| Vikunja's image is `FROM scratch` — no shell | `Dockerfile` |
| postgres:18 needs `/var/lib/postgresql`, not `/var/lib/postgresql/data`; the app needs a writable `files/` | container test |
| Vikunja OIDC: `auth.openid.enabled`, registration off via `service.enableregistration` | upstream config docs |
| llama.cpp server is OpenAI-compatible **and** has a model router (`--models-dir`, `--models-max`) | `tools/server/README.md` |
| LiteLLM is an OpenAI-format gateway with virtual keys/spend tracking; MIT except `litellm/enterprise/` | upstream README + LICENSE |
| Stirling's free plan caps users at **5** (reason it was dropped) | upstream plans table |
| llama.cpp serves **many models from one process** in router mode (`--models-dir`, `--models-max`, `--models-memory-margin`) | `tools/server/README.md` |
| LiteLLM pools deployments that share a `model_name`; `simple-shuffle` default; cooldowns on 429/>50% fails/401/404/408; `x-litellm-model-id` names the serving deployment | upstream load-balancing docs |
| **Sticky sessions via `optional_pre_call_checks: [session_affinity]`, keyed on `x-litellm-session-id` / `metadata.session_id`, and requiring Redis to share pins across replicas** | LiteLLM load-balancing docs, session-affinity section |
| `deployment_affinity` pins **per caller/API key** (defeats balancing) while `session_affinity` pins per session id; stale `deployment_affinity:*` keys in Redis keep pinning after the setting is disabled | LiteLLM load-balancing docs (session-affinity settings table, "How it works") |
| llama.cpp RPC exists for multi-host inference but is **"proof-of-concept … fragile and insecure"** per its own README | `tools/rpc/README.md` |
| **exo is MLX/Apple-Silicon only** — no NVIDIA support | upstream README (all benchmarks M3 Ultra; MLX backend) |
| vLLM multi-node = TP within a node, PP across nodes, via Ray | vLLM docs (parallelism and scaling) |
| Multi-node over plain Ethernet measured ~33 tok/s (PP) / single-digit RPM (TP); **one model per box was ~10x faster** | published two-node vLLM benchmark |
| Cloudron memory minimums quoted (384/256/1024 Mi) | Cloudron app API manifests |
| `cuda-rhel10.repo` is live (200, gpgcheck on) and carries `nvidia-driver*`, `nvidia-driver-selinux`, `nvidia-container-toolkit`, `kmod-nvidia-open-dkms` and `kmod-nvidia-latest-dkms`; driver 620.70.06, open DKMS to 620.70.06, **proprietary capped at 610.57.04** | live repo listing |
| EL10 has **no DNF `nvidia-driver` module stream** (unlike EL9); packages are installed directly from `cuda-rhel10` | live repo + NVIDIA gpu-operator docs |
| GPU Operator with `driver.enabled=true` on a node that already has a driver: the init container detects it, labels the node `nvidia.com/gpu.deploy.driver=pre-installed`, and skips the install | NVIDIA gpu-operator docs, "Pre-installed NVIDIA GPU drivers" |
| Requiring one driver DaemonSet **per kernel version** (precompiled) or one per OS (compiled) — `driver.enabled=true` is why all GPU nodes must match | NVIDIA gpu-operator docs / NVIDIA Driver CRD |
| Installing a driver + toolkit does **not** make `nvidia.com/gpu` schedulable; a device plugin (or NFD + plugin) must run | NVIDIA device-plugin and gpu-operator architecture docs |

Assumptions **not** yet verified, and flagged as such:

- Whether Calibre-Web's OPDS endpoint is `/opds` on the pinned version.
- BookStack's API-token creation flow being scriptable without the UI (the token
  hook's feasibility).
- Actual memory use of each on this platform (Cloudron's numbers are their
  minimums, not ours).
- **LiteLLM sizing.** Budget ~1 GiB under load (512Mi request / 1Gi limit) and
  expect it to need ~45 s before its readiness probe passes. These come from a
  comparable LiteLLM + llama.cpp deployment rather than from this platform, and
  they are the numbers most likely to be wrong here — measure before pinning.

## Open questions to settle before implementation

1. **Token hook design** — per-app adapters (recommended, honest) vs one generic
   implementation (will not fit both).
2. **AI target and GPU prerequisite** — Phase B needs at least one GPU node, and the
   platform has neither a driver-install path nor a device plugin (see "GPU discovery
   and driver install"). Decide which of the two designs to build — host `gpu-agent`
   + device plugin, or GPU Operator — before designing any chart, because the chart's
   GPU stanza depends on which one advertises the resource.
3. **Calibre-Web fork** — upstream `janeczku/calibre-web` moves slowly;
   `Calibre-Web-Automated` is the active fork. Pick one deliberately and pin it.
4. **Where does the AI stack live** — a normal catalog app with big PVCs, or a
   separate "infrastructure" concept alongside the wireguard gateway? It is the first
   thing in this plan that is not a household web app.
5. **Which model pool to start with** — one small model pooled across all nodes
   (proves balancing), or distinct models per node (more useful day one)?
6. **Sticky granularity** — Open WebUI sends no session id (verified), so pick one:
   `ENABLE_FORWARD_USER_INFO_HEADERS` for per-user stickiness (cheap, no code), or a
   shim for per-conversation. Also decide the topology: explicit per-node backends
   in `model_list`, or one headless Service with `kubernetes_pod_discovery`.
