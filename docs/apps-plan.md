# App expansion plan: Calibre-Web, BookStack, Vikunja + a multi-node AI stack

Status: **proposal — nothing implemented.** Last reviewed 2026-11-20.
Companions: [`catalog.md`](catalog.md) (entry format), [`testing.md`](testing.md)
(gate taxonomy), [`versioning-plan.md`](versioning-plan.md) (chart packaging),
[`apps/testing.md`](apps/testing.md) (generated per-app status).

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

Order matters: **Calibre-Web first** (no OIDC plumbing, no token, smallest), then
**Vikunja** (proves OIDC + token in one place), then **BookStack**. The AI stack is
a separate track with its own target (section 4).

**PDF apps are dropped** by decision — Stirling's 5-user free cap, BentoPDF is
client-side only, Gotenberg has no UI. Not in scope.

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

## 1. Calibre-Web

**Why:** the catalog has no reading app. Calibre-Web serves a library over the
web and over **OPDS**, so e-readers (KOReader, Moon+ Reader) can browse and
download without a browser. Cloudron ships `9.18.0 (calibre 8.4.0)` at a 384 Mi
minimum — the smallest of the three.

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

**Sticky sessions should be the default**, and there is prior art for this in
`rborso/umkim@devel` (`roles/fleet_llm`), which already runs a hub LiteLLM in
front of per-node llama.cpp backends. Copy that design rather than reinvent it:

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

2. **`session_affinity`, not `deployment_affinity`.** The umkim role documents this
   explicitly: *"Do not enable `deployment_affinity` on the hub proxy: it pins by
   API key hash and prevents load balancing across spokes for one key."* That is a
   real trap — `deployment_affinity` sounds like what you want and quietly turns
   your pool back into one backend per key.

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

There is also a stale-key hazard the umkim role handles: if `deployment_affinity`
was ever enabled, its keys linger in Redis and keep pinning after you switch it
off. That role deletes `deployment_affinity:*` on reconcile. Do the same, or a
config change will not take effect.

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

Each llama.cpp node is launched with its own model directory:

```sh
llama-server --host 0.0.0.0 --port 8080 \
  --models-dir /models --models-max 2 --models-memory-margin 512
```

`--models-max 2` and the memory margin matter on a single-GPU node: without them
the router can try to hold more models in VRAM than the card has, and the OOM
kills the server rather than the model.

The umkim role also pins a **PEP 440 tag** (it uses `v1.102.2`, chosen because
"session_affinity for sticky chats" landed in the 1.100 line) and caps memory
(512Mi request / 1Gi limit) because a floating `:main` OOM-killed the hub at
3–4 GiB. Both are worth keeping: pin the tag, cap the memory.

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

- **A — LiteLLM + Open WebUI against one backend.** Proves the entrypoint, OIDC,
  the model list and the gateway. Works on the current cluster today (with an
  external or single local backend).
- **B — second and third llama.cpp backends** joining the same model pool. This is
  where the multi-node goal is actually met, and it needs no exotic networking.
- **C — optional**: a large model split across nodes (vLLM/Ray) *only* if a model
  must be served that no single node can hold, and the interconnect is fast.

### Platform prerequisites this exposes

1. **No GPU support exists in the platform today** — no `nvidia.com/gpu` resource,
   no NVIDIA device plugin, no runtimeClass. Phase B needs the device plugin on the
   GPU nodes and `resources.limits."nvidia.com/gpu"` in the charts. Platform change,
   not a chart change.
2. **Cross-node traffic**: LiteLLM → llama.cpp is plain HTTP on the pod/service
   network, which the existing NetworkPolicies handle. (vLLM/Ray in Phase C is the
   exception — NCCL needs its own ports and the policies assume a single node.)
3. **Model storage**: each node serving models needs the GGUFs locally — a PVC per
   node, tens of GB. On Longhorn that is replicated, so budget accordingly. Shares
   nothing with the split-model case, which needs the *same* weights everywhere.
4. **Memory**: not on dev-single. It needs its own target.

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

The balancing and sticky checks are deliberately a pair: with only one of them you
cannot tell whether affinity is configured or merely absent. Both are cheap (a
handful of short completions) and both are deterministic — no text assertion.
- Time-box all of it; CPU inference must not make the gate flaky. If it exceeds the
  budget, mark the AI checks informational.

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
| Stirling's free plan caps users at **5** (reason it was dropped) | upstream plans table |
| llama.cpp serves **many models from one process** in router mode (`--models-dir`, `--models-max`, `--models-memory-margin`) | `tools/server/README.md` |
| LiteLLM pools deployments that share a `model_name`; `simple-shuffle` default; cooldowns on 429/>50% fails/401/404/408; `x-litellm-model-id` names the serving deployment | upstream load-balancing docs |
| **Sticky sessions via `optional_pre_call_checks: [session_affinity]`, keyed on `x-litellm-session-id` / `metadata.session_id`, and requiring Redis to share pins across replicas** | LiteLLM load-balancing docs, session-affinity section |
| **Do not enable `deployment_affinity` on a shared proxy** — it pins per API key and defeats balancing; stale `deployment_affinity:*` Redis keys keep pinning after the setting is turned off | `rborso/umkim@devel` `roles/fleet_llm` (comments + reconcile task) |
| Prior art: umkim's hub runs LiteLLM pinned at `v1.102.2` with `simple-shuffle` + `session_affinity` + Redis in front of per-node `llama-cpp-helm` backends | `rborso/umkim@devel` charts + roles |
| llama.cpp RPC exists for multi-host inference but is **"proof-of-concept … fragile and insecure"** per its own README | `tools/rpc/README.md` |
| **exo is MLX/Apple-Silicon only** — no NVIDIA support | upstream README (all benchmarks M3 Ultra; MLX backend) |
| vLLM multi-node = TP within a node, PP across nodes, via Ray | vLLM docs (parallelism and scaling) |
| Multi-node over plain Ethernet measured ~33 tok/s (PP) / single-digit RPM (TP); **one model per box was ~10x faster** | published two-node vLLM benchmark |
| Cloudron memory minimums quoted (384/256/1024 Mi) | Cloudron app API manifests |

Assumptions **not** yet verified, and flagged as such:

- Whether Calibre-Web's OPDS endpoint is `/opds` on the pinned version.
- BookStack's API-token creation flow being scriptable without the UI (the token
  hook's feasibility).
- Actual memory use of each on this platform (Cloudron's numbers are their
  minimums, not ours).

## Open questions to settle before implementation

1. **Token hook design** — per-app adapters (recommended, honest) vs one generic
   implementation (will not fit both).
2. **AI target sizing** — Phase B needs at least one GPU node, and the platform has
   no NVIDIA device plugin yet. Decide whether a GPU node is available before
   designing the charts.
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
