# Security follow-up: findings outside the hardening plan

Status: **implemented, 2026-10-21.** Companion to
[`security-plan.md`](security-plan.md). It covers issues found in the current
tree that the existing plan did not name. IDs continue from `SEC-025`.

All seven items below are in the tree with unit or chart checks; the live
assertions in `ci/assert.sh` run in the lab e2e. The remaining work is the
operator migration in **SEC-026** (Helm prunes the old ConfigMap on upgrade).

It does not replace or re-litigate the P0/P1 items in the main plan. Where an
item already exists (console RBAC, builder split, secrets at rest), it is named
as a dependency rather than duplicated.

## Why these matter more if the product is "easy"

An experienced operator can compensate for a weak default. A first-time user
cannot, and will not read a threat model before clicking **Install**. If the
quickstart is the product, then **secure defaults and recoverability are
features**, not hardening work:

- a secret that is technically readable is effectively published, because
  nobody will rotate it;
- an error message shown in the UI will be pasted into a GitHub issue;
- a console exposed with a password (no MFA) is the first thing scanned;
- a failed install with no rollback is a support ticket;
- a lost invite store is a "start over" event.

The six findings below are therefore **P0/P1 for a novice-facing release**, even
where the main plan treats the underlying area as P1/P2.

## Summary

| ID | Severity | Finding | Effort | State |
|---|---|---|---|---|---|
| SEC-026 | **High** | Authelia session, storage, LDAP, reset and OIDC secrets are rendered into a **ConfigMap**, not a Secret | S | Done |
| SEC-027 | **High** | Directory passwords and admin tokens go through `kubectl` **argv** and are echoed in error text | M | Done |
| SEC-028 | Low | Local breakglass password is compared with `==` (timing oracle, length leak) | S | Done |
| SEC-029 | Medium | No HTTP server or client timeouts; a hung directory call hangs the console | S | Done |
| SEC-030 | Medium | Invite store is read-modify-write without a lock; one invite can be redeemed twice and writes can be lost | M | Done |
| SEC-031 | Low | Some mutating API routes skip the origin check and the audit line | S | Done |
| SEC-032 | Medium | Console pod runs as root with a writable rootfs and no seccomp/capability baseline | M | Done |

Severity is for a cluster that is reachable from the internet. On a fully
private, single-user lab, SEC-026 and SEC-027 are still the two that leak.

---

## SEC-026 — Authelia secrets in a ConfigMap

### Evidence

`charts/identity/templates/authelia.yaml` renders the whole Authelia
`configuration.yml` into a **ConfigMap** named `authelia-config`. The helper
`identity.autheliaConfig` (`charts/identity/templates/_helpers.tpl`) puts these
values in it:

- `identity_validation.reset_password.jwt_secret`
- `authentication_backend.ldap.password` (the LLDAP admin or bind password)
- `identity_providers.oidc.hmac_secret`
- `identity_providers.oidc.issuer_private_key`
- `identity_providers.oidc.clients[].client_secret` (now a PBKDF2 hash, still a
  credential)
- `session.secret`
- `storage.encryption_key`

A rendered fixture confirms it:

```text
$ helm template identity charts/identity -f ci/test-values/identity.yaml \
    | awk '/kind: ConfigMap/,/^---/' | grep -E 'jwt_secret|password:|encryption_key|secret:'
        jwt_secret: "ci-only-authelia-reset"
        password: "ci-only-lldap-admin"
      secret: "ci-only-authelia-session"
      encryption_key: "ci-only-authelia-storage"
```

ConfigMaps are not encrypted by k3s `secrets-encryption`, are commonly readable
by broader roles than Secrets, and are not what operators expect to rotate.
`SEC-015` (secrets at rest) does not fix the object kind.

### Fix

1. In `charts/identity/templates/authelia.yaml`, change the `authelia-config`
   object from `ConfigMap` to `Secret` (`type: Opaque`) with:

   ```yaml
   stringData:
     configuration.yml: |
       {{- include "identity.autheliaConfig" . | nindent 6 }}
   ```

   Keep the name, the volume name, and the `subPath: configuration.yml` mount.
   Keep the `checksum/authelia-config` annotation; it now hashes the Secret
   content, so rotating a secret rolls Authelia.
2. Change the `wait-lldap-ldap` init container's `LLDAP_BIND_PASSWORD` from a
   literal `value:` to a reference:

   ```yaml
   - name: LLDAP_BIND_PASSWORD
     valueFrom:
       secretKeyRef:
         name: lldap-credentials
         key: LLDAP_LDAP_USER_PASS
   ```

   That Secret already exists when `lldap.deploy` is true. When it is false
   (external directory), add a `ldap-password` key to the new `authelia-config`
   Secret and reference that key instead. Do not put the value back in the pod
   spec.
3. Audit the other templates for the same mistake. `lldap-bind-account.yaml`
   already uses a Secret. `traefik-middleware.yaml` must contain only hosts and
   regexes; assert that.
4. Update `ci/test-values/identity.yaml` only if the fixture names change; the
   values themselves stay public fixtures.

### Migration

- Helm 3 prunes a resource removed from the manifest on upgrade, so the old
  `ConfigMap` should disappear. Verify with
  `kubectl get cm -n identity`; if an old release leaves it behind, delete it
  once with `kubectl delete cm -n identity authelia-config`.
- Authelia reads the file at startup; the checksum annotation causes one
  rollout. Expect a short portal outage during the upgrade.
- The new Secret is part of the same release; Helm creates it before the
  Deployment.

### Acceptance

- `ci/static.sh` fails if any rendered `ConfigMap` contains a fixture secret
  (see **Tests**).
- `helm template` shows the secrets only under `kind: Secret`.
- Live: `kubectl get cm -n identity -o yaml` has no `session:`, `encryption_key`,
  `hmac_secret`, `issuer_private_key`, `jwt_secret`, `client_secret`, or
  `password:`; `kubectl get secret -n identity authelia-config` has the keys.
- Authelia `/api/health` is healthy, portal login works, an OIDC app still
  signs in, and changing `authelia.sessionSecret` rolls the pod.

---

## SEC-027 — Directory password and token in `kubectl` argv

### Evidence

`internal/identity/identity.go` (invite redemption) runs:

```go
d.kubectl("exec", "deploy/lldap", "--",
    "env", "LLDAP_USER_PASSWORD="+password,
    "/app/lldap_set_password",
    "--base-url", "http://localhost:17170",
    "--token", token,
    "--username", username)
```

Two problems:

1. The password and the admin token are in the `kubectl` process's argv on the
   console host/node, visible in `ps` for the life of the call.
2. `kubectl()` formats the **whole argv** into its error:
   `fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)`. If the exec
   fails, the password is returned to the API handler, rendered in the console,
   and is one copy-paste away from an issue report or a screenshot.

The comment above `setPassword` says "no password in argv"; the `env` assignment
is still an argument to `kubectl`. This is exactly the kind of leak an
inexperienced user will surface without knowing it.

### Fix

1. Add a helper that feeds stdin and never formats argv on error:

   ```go
   func (d *Directory) kubectlInput(stdin string, args ...string) (string, error) {
       cmd := exec.Command(d.cfg.Kubectl, append([]string{
           "--kubeconfig", d.cfg.Kubeconfig, "-n", d.cfg.Namespace,
       }, args...)...)
       cmd.Stdin = strings.NewReader(stdin)
       out, err := cmd.CombinedOutput()
       if err != nil {
           return string(out), fmt.Errorf("kubectl %s: %w", args[0], err)
       }
       return string(out), nil
   }
   ```

   Make `kubectl()` equally terse (`args[0]` or a caller-supplied operation
   name); keep the full command only behind a debug log that redacts values.
2. Pass the secret values on stdin, not in argv. The simplest form, if the
   image has `base64`, is two base64 lines so arbitrary bytes (including
   newlines) are safe:

   ```go
   script := `IFS= read -r pw_b64; IFS= read -r tok_b64
   pw=$(printf %s "$pw_b64" | base64 -d)
   tok=$(printf %s "$tok_b64" | base64 -d)
   export LLDAP_USER_PASSWORD="$pw"
   exec /app/lldap_set_password --base-url http://localhost:17170 --token "$tok" --username "$1"`
   payload := b64(password) + "\n" + b64(token) + "\n"
   d.kubectlInput(payload, "exec", "-i", "deploy/lldap", "--",
       "sh", "-c", script, "sh", username)
   ```

   Verify the LLDAP image has `base64`; if not, check whether
   `lldap_set_password` supports `--password-stdin` or an interactive prompt and
   use that. Do not fall back to `env VAR=...` as an argument.
3. Reject or explicitly handle control characters in the invite password
   (`\n`, `\r`, NUL) at the API boundary. A clear validation error is better
   than a broken exec.
4. Redact error output before returning it. Wrap the LLDAP stdout/stderr so a
   helper that echoes the input cannot leak it; never include the password in
   the `fmt.Errorf` chain.
5. Check for the same pattern elsewhere. `internal/app/servarr.go` passes
   *arr/qBittorrent credentials to an HTTP client, not argv; confirm. The CI
   scripts are not shipped, but `ci/enable-servarr-bootstrap.sh` should also
   avoid secrets in process arguments.

### Acceptance

- A unit test with a fake `kubectl` script that writes `"$@"` to one file and
  `cat`s stdin to another: assert the password and token are **not** in argv and
  are present in stdin.
- A forced-failure test: the returned error contains the operation and the
  exit status, not the password.
- `grep -R 'kubectl.*exec'` has one implementation, and it uses `-i`.
- E2E: redeem an invite; a `ps` snapshot during the call (or a wrapper that logs
  argv) shows no credential.

---

## SEC-028 — Non-constant-time breakglass password check

### Evidence

`cmd/umkim-thin/consoleauth.go`, `valid()`:

```go
if a.localUser != "" && user == a.localUser && a.localPass != "" && pass == a.localPass {
```

`==` short-circuits on the first differing byte and leaks the password length.
The console is at the public edge (behind Authelia, but still reachable by an
authenticated portal user), and `localadmin` is the breakglass account.

### Fix

- Compare SHA-256 digests with `hmac.Equal` (constant time, no length leak):

  ```go
  func constTimeEqual(a, b string) bool {
      ah, bh := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
      return hmac.Equal(ah[:], bh[:])
  }
  ```

  Use it for the local password. The username comparison is not secret and can
  stay `==`. The session signature already uses `hmac.Equal`.
- Add a unit test for correct/incorrect/empty. Do not attempt a timing test in
  CI.

---

## SEC-029 — No HTTP timeouts

### Evidence

- `cmd/umkim-thin/api.go` ends with `http.ListenAndServe(addr, ...)`: no
  `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`. A
  slowloris client can hold connections; a stuck peer can exhaust the server.
- `internal/identity/identity.go` uses `http.Post` and `http.DefaultClient`
  without a timeout. If LLDAP does not answer, the console handler hangs
  indefinitely (and, with SEC-030, can block invite operations).

### Fix

- Build the server explicitly:

  ```go
  srv := &http.Server{
      Addr:              addr,
      Handler:           securityHeaders(auth.requireAuth(mux)),
      ReadHeaderTimeout: 10 * time.Second,
      ReadTimeout:       30 * time.Second,
      WriteTimeout:      60 * time.Second,
      IdleTimeout:       120 * time.Second,
      MaxHeaderBytes:    1 << 20,
  }
  ```

  Return `http.ErrServerClosed` as clean. A 60 s write timeout is safe because
  installs are asynchronous jobs; the longest synchronous handler is the status
  poll. If a slow `kubectl` can exceed it, raise it to 120 s and document why.
- Use one package-level client:

  ```go
  var directoryClient = &http.Client{Timeout: 20 * time.Second}
  ```

  and use it for `login` and `gql`. Add `context` support later; the client
  timeout is the immediate fix.
- Optional, but recommended for a novice product: handle `SIGTERM` with
  `srv.Shutdown(ctx)` and a 25 s deadline, so a rollout does not cut a job.

### Acceptance

- A unit test constructs the server (via a `newHTTPServer` helper) and asserts
  the timeout fields; a second test asserts `directoryClient.Timeout`.
- `go test -race` stays green.
- Manual: `nc` a partial header and confirm the connection closes; stop LLDAP
  and confirm the console returns an error promptly instead of hanging.

---

## SEC-030 — Invite store race and non-atomic writes

### Evidence

`internal/identity/identity.go`:

- `CreateInvite`, `ListInvites`, `Revoke` each do `load()` → mutate → `save()`
  with no lock.
- `Redeem` loads the store, checks `Used` and expiry, then makes several LLDAP
  calls, then sets `Used` and saves. Two concurrent redemptions of the same
  token can both pass the check and create two accounts (one invite, two
  users). Concurrent invite creation/revocation can lose updates.
- `save()` uses `os.WriteFile`, which truncates in place. A crash or a full
  disk during the write leaves a truncated JSON file; every later invite
  operation fails until an operator repairs it.

The console is `replicas: 1`, but it serves concurrent requests. The host
`/etc/umkim-thin` copy is only a seed; the live invite store is on the PVC, so
operations should run through the console pod/API.

### Fix

Preferred: a **claim** method with a lease, so the lock is not held across
network I/O.

1. Add to `Invite`:

   ```go
   ClaimedAt *time.Time `json:"claimedAt,omitempty"`
   ```

   `Used` remains the terminal state. A claim older than, say, 5 minutes is
   considered abandoned and can be taken over.
2. Add `mu sync.Mutex` to `Directory` (or a small `store` type it owns).
   `Claim(token)` loads, validates expiry, treats `Used` or a fresh
   `ClaimedAt` as "unavailable", sets `ClaimedAt=now`, saves atomically, and
   returns the invite. `Release(token)` clears the claim on failure.
3. `Redeem` calls `Claim` first, then does the LLDAP work, sets `Used=true` on
   success, and `Release`s on any error. This keeps the invariant testable and
   avoids a lock held for the duration of a network call.
4. Make `save` atomic: write to `os.CreateTemp(dir, ".invites-*")`, `Chmod`
   0600, `Sync`, `Close`, `os.Rename`, and remove the temp file on error. The
   rename is atomic on the same filesystem.
5. If CLI and console can both write, take a `flock` on `StateFile+".lock"` in
   `load`/`save`, or document that only the console writes and the CLI is
   read-only. The former is safer; `syscall.Flock` is Linux-only, which is the
   supported platform.
6. Add `go test -race` for the identity package to `ci/static.sh`.

### Acceptance

- A `-race` test with 20 goroutines redeeming one token: exactly one succeeds,
  the rest get "already used"/"being redeemed", and a fake LLDAP records one
  account. (Refactor the LLDAP calls behind a small interface or use a fake
  `kubectl` script for the test.)
- 20 concurrent `CreateInvite`s produce 20 distinct invites, all present.
- A simulated failure during `Redeem` releases the claim; a retry succeeds.
- Killing the process between `Claim` and `save` leaves the old file intact
  (atomic rename), and a stale claim is reclaimable after the lease.
- Existing invite tests still pass.

---

## SEC-031 — Missing origin checks and audit on mutating routes

### Evidence

`cmd/umkim-thin/api.go`:

- `POST /api/invites` (create) has no `sameOrigin` and no `audit`.
- `DELETE /api/invites/{token}` has no `sameOrigin` and no `audit`.
- `POST /api/wg/clients` and `DELETE /api/wg/clients/{name}` have no
  `sameOrigin` and no `audit`.
- `POST /api/invites/redeem` is intentionally open (token-authenticated) and is
  reached server-side from the signup nginx; leave it open, but rate-limit it.
- Mutating handlers that already check (`install`, `uninstall`, settings,
  setup, CI user delete) are the model to copy.

`SameSite=Lax` plus a JSON content type makes most CSRF attempts fail, but the
inconsistency is a finding, and the missing audit means a household/team
operator cannot see who created an invite or a WireGuard client.

### Fix

1. Apply `sameOrigin` to every admin mutating route. Leave `/login`,
   `/logout`, and `/api/invites/redeem` as the documented exceptions (form
   posts and the token-authenticated signup flow).
2. Add audit lines with the actor and the target, never the value:
   `invite-create` (email/group), `invite-revoke` (token prefix or email),
   `wg-create`/`wg-delete` (client name), and `login-failed` (client key, no
   password). Keep the existing `install`/`uninstall`/`settings` lines.
3. Rate-limit `/api/invites/redeem` by client key using the existing login
   limiter, with a higher threshold (tokens are 48 hex chars; the limit is
   insurance, not the primary control).
4. Optional: issue a per-session CSRF token for the login form and the signup
   page. Not required if the origin check and SameSite are in place; document
   the decision.

### Acceptance

- A table-driven test iterates every mutating route with
  `Origin: https://evil.example` and expects 403; with no `Origin` (curl/CLI)
  it succeeds.
- A test captures `log` output and asserts one audit line per mutation, with no
  secret value in it.
- E2E: create and revoke an invite, create and delete a WireGuard client, and
  see the four audit lines in the console log.

---

## SEC-032 — Console pod baseline

This complements `SEC-012` (RBAC split) and `SEC-013` (builder split); it is the
cheap pod-level baseline, not the privilege redesign.

### Evidence

`charts/umkim-thin/templates/deployment.yaml` sets only
`seLinuxOptions: {type: spc_t}`. The image has no `USER`, so the container runs
as root; the rootfs is writable; no `allowPrivilegeEscalation: false`, no
capability drop, no seccomp profile. The pod holds a broad ClusterRole
(`SEC-012`) and a kubeconfig, so a compromise is a cluster problem.

### Fix

1. Add a non-root user to the image (`USER 65532:65532`) and set
   `HOME=/tmp`, `KUBECONFIG=/tmp/kubeconfig`.
2. Container `securityContext`:

   ```yaml
   runAsNonRoot: true
   runAsUser: 65532
   runAsGroup: 65532
   allowPrivilegeEscalation: false
   readOnlyRootFilesystem: true
   capabilities:
     drop: ["ALL"]
   seccompProfile:
     type: RuntimeDefault
   ```

3. Mount `emptyDir` at `/tmp` for the kubeconfig and Helm's cache/config/data
   (`HELM_CACHE_HOME=/tmp/helm/cache`, `HELM_CONFIG_HOME=/tmp/helm/config`,
   `HELM_DATA_HOME=/tmp/helm/data`); create the directories in the entrypoint.
4. Make the state writable by the UID:
   - both targets: `fsGroup: 65532` on the pod and
     `fsGroupChangePolicy: OnRootMismatch`; the console PVC handles ownership;
   - single only: `chown root:65532 /run/umkim-thin` and `chmod 0750` for the
     storage-agent socket. The console no longer reads the host state
     directory; the chart mirror is seeded onto the PVC.
5. `kubectl exec` into LLDAP is an API call, not a local exec, and works without
   capabilities. Verify in e2e.
6. `seLinuxOptions: spc_t` is conditional on a value (a lab/SELinux workaround
   for the single-node storage socket; it is ignored on clusters without
   SELinux and is not hard-coded in the portable chart).

### Acceptance

- `helm template` asserts the fields above.
- E2E single + HA: the console starts, installs an app, redeems an invite (the
  `kubectl exec` path), writes state, and runs Helm (cache works).
- `kubectl exec ... -- id` shows UID 65532; `touch /x` in the container fails.
- Ansible re-run on an existing host fixes ownership without breaking the
  install.

---

## Tests to add to the gate

### `ci/static.sh`

```sh
# No Authelia secret may appear in a ConfigMap.
render=$(helm template identity charts/identity -f ci/test-values/identity.yaml)
for v in ci-only-authelia-session ci-only-authelia-storage \
         ci-only-authelia-reset ci-only-lldap-admin; do
  if printf '%s' "$render" |
     awk '/kind: ConfigMap/,/^---/' | grep -qF "$v"; then
    echo "secret $v is rendered into a ConfigMap" >&2
    exit 1
  fi
done
```

- Assert the console chart renders the `securityContext` fields (SEC-032).
- Run `go test -race ./...` (or at least `./internal/identity`).
- Keep `gitleaks`; add the new fixture strings to the allowlist only if
  necessary.

### `ci/assert.sh`

- `kubectl get cm -n identity authelia-config` is absent;
  `kubectl get secret -n identity authelia-config` exists.
- The console pod has `runAsNonRoot: true` and
  `readOnlyRootFilesystem: true` (read the pod spec, not the chart).
- The console process runs as the expected UID.
- A failed invite redemption returns a redacted error (grep the console log for
  the test password; must be absent).

### Playwright / e2e

- The invite flow still works after the stdin change (this is the regression
  risk).
- The first-run wizard (once it exists) and the novice path on a clean VM.
- A deliberately failed exec (scale LLDAP to zero, redeem) shows an error
  without the password.

---

## Ordering

1. **SEC-026, SEC-027** — the two that leak real credentials. Do these before
   any public or novice-facing release.
2. **SEC-032, SEC-031, SEC-028** — cheap, visible, and reduce the blast radius
   if the console is exposed.
3. **SEC-029, SEC-030** — reliability and data integrity; do them with the
   novice path so "it hangs" and "my invite broke" do not become the first
   issues.
4. Then the product work below, and the existing `SEC-012`/`013`/`015` in the
   main plan.

Estimated total: **2–3 focused days** for SEC-026…032, including tests, plus
the Ansible ownership change for SEC-032.

---

## How the "easy for non-technical users" goal changes the perspective

It changes the product, not just the docs. The security findings are the first
symptom.

### 1. Secure defaults become the feature

A novice will not enable MFA, choose private exposure, rotate a secret, or
notice a public path. So the **defaults** must be the safe answer:

- **Console:** no unauthenticated or single-factor admin surface on the
  internet. Either force MFA enrollment on first login, or make the console
  WireGuard-only and offer a deliberate "expose admin UI" switch. Pick one and
  make it the quickstart. Do not ship "public + password" as the default.
- **Apps:** ask one plain question at install time — *"Who should reach this?"*
  — with two answers:
  - *Anyone with a login* → public route + Authelia;
  - *Only my devices* → WireGuard route (`exposure: vpn`).
  Make the private option the recommended default for admin tools and the
  public option for share-oriented apps. The catalog already has the `exposure`
  field; the wizard must use it.
- **Secrets:** generated, never printed twice without a "copy now" warning,
  stored in Secrets, rotated with the app, and never rendered in an error.
- **Destructive actions:** "uninstall" must say what is deleted, and "purge"
  must require typing the app name. For a novice, "delete my photos" is one
  mis-click away.

### 2. The install path is the product

"Run this Ansible playbook from a jumphost" is not a novice path. A novice
needs:

- one blessed OS (Rocky Linux 9/10) and one command;
- preflight checks with actionable messages (DNS, ports 80/443/51820, disk,
  kernel modules, existing k3s);
- TLS and DNS guidance, not a certificate troubleshooting session;
- a **doctor** command/button that diagnoses the common failures and produces
  a redacted bundle;
- backup/restore and upgrade/rollback from day one, because novices will not
  read release notes.

This is a larger investment than any single security fix, and it is the real
gate for "easy". Keep the existing cluster path as the advanced route; do not
let it define the quickstart.

### 3. The console needs a first-run wizard and plain language

Current UI is a compact single page that assumes the user knows what Helm,
a StorageClass, and an Ingress are. For the new audience:

- a setup checklist that persists until done: domain/TLS → admin + MFA →
  storage check → first app → invite a user → WireGuard (optional);
- **no Kubernetes vocabulary** in the UI: "application", "address", "storage",
  "backup", "access" — the API can keep the technical names;
- a **"What is on the internet?"** page: every host, its auth mode, its public
  paths, and its certificate status. This is the single best safety feature you
  can give a non-expert;
- inline warnings before exposure and deletion, with a plain-language
  consequence;
- a "repair" action for a failed install (rollback, retry, logs);
- a "copy diagnostics" button that redacts secrets.

### 4. The catalog becomes a support contract

Every entry is something you support. For novices, fewer, safer apps are
better:

- no manual post-install steps (automate them, or do not list the app);
- a known auth mode and no public path unless it is token-authenticated;
- resource requests that fit a small VPS, and a documented backup story;
- a per-app page: what it is, what it needs, what is exposed, how to back it
  up, known limitations.

The `verify` suite is the quality gate; the catalog page is the promise.

### 5. Quick guides are task-based, not architecture-based

Keep `docs/design.md`, `docs/auth.md`, and the rest for contributors and
operators. Add a `docs/quickstart/` tree for users:

1. **Install on a fresh server** (15 minutes, one command, expected output).
2. **First login and MFA** (including the breakglass account).
3. **Invite someone** (link, expiry, what they see).
4. **Install your first app** (screenshots, public vs. private choice).
5. **Connect a device with WireGuard** (QR, client apps, revoke).
6. **Back up and restore** (what is backed up, where, how to test it).
7. **Upgrade safely** (backup first, rollback).
8. **Something is broken** (doctor, logs, diagnostics bundle, common errors).
9. **I'm locked out** (breakglass, reset, recovery).
10. **Uninstall** (what is removed, what remains).

Each guide: goal, time, prerequisites, copy-paste steps, "what you should see",
"if it fails", "how to undo", and a one-paragraph security note. The current
docs are excellent reference material; they are not a novice path. Link them at
the end of each guide for depth.

### 6. Support and recovery are part of the product

Novices generate support load. The cheapest way to contain it:

- a diagnostics bundle and a "before you open an issue" page;
- a version/compatibility page and a known-issues page;
- a clear support policy (community-only, best-effort, no production SLA);
- a "reset/repair" path that does not require cluster knowledge;
- telemetry? No — not for this audience and not for this project's privacy
  stance. A local health page is enough.

### 7. Positioning changes — and the competition gets harder

"A platform for a cluster you already have" is an expert pitch. "The easiest
way to run a private app stack on your own server" puts you against Cloudron,
YunoHost, Umbrel, Runtipi, and CasaOS, all of which already own the "easy"
mindshare. Your differentiators remain real — Kubernetes-grade storage and HA,
identity-first access, WireGuard, a verified catalog — but the quickstart must
hide the Kubernetes, and the README must lead with the outcome, not the
architecture.

Decide which audience is primary:

- **Expert/k8s-first:** the current docs and Ansible path are close; open
  source sooner, accept a smaller audience.
- **Novice-first:** invest in installer, wizard, doctor, backup, and guides
  first; open source after the clean-VM path works, or the first issues will be
  "it doesn't install."

Trying to serve both with one quickstart usually serves neither. A reasonable
compromise: one blessed novice path, with the existing cluster path labelled
**Advanced** and kept out of the first ten minutes.

### 8. Open-source timing under the new lens

Novices should not have to assemble images by hand. "Open source" here means a
**one-command install whose internal images are built locally**, not a
repository that expects users to know the build graph. That raises the bar
before launch:

- keep the installer/console local build path working and documented; internal
  images are not prebuilt or published;
- ship app charts through the `charts/apps` submodule and local mirror;
- sign release tags and attach an SBOM for the source release (`SEC-021`);
- keep the lab e2e runner off PRs (`SEC-005`);
- include `SECURITY.md`, a disclosure process, and the current hardening
  status in the repository;
- be explicit that it is alpha and single-admin.

The security findings above are launch blockers precisely because the new
audience will trust the defaults. Fixing them is cheaper than the first
incident.

---

## Open questions

1. **Console exposure default:** MFA-on-public (easiest) or WireGuard-only
   (safest)? Recommendation: MFA-on-public with a guided enrolment, plus a
   one-click "restrict console to WireGuard" for advanced users. Do not ship
   password-only public access.
2. **One blessed OS** or a small matrix? Recommendation: one, with a preflight
   that fails clearly elsewhere.
3. **How much of the Ansible lab path survives?** It is valuable for the HA
   story; keep it as the advanced path and do not let it shape the quickstart.
4. **Backups from day one?** Recommendation: yes, even if it is only
   `velero`/restic to a local directory plus a documented restore drill.
5. **Name and first impression.** "umkim-thin" is not discoverable and needs a
   parent project to explain it. A short name and a one-sentence outcome matter
   more for this audience than for experts.

## Non-goals

- A full Vault/KMS; `SEC-015` (SealedSecrets/SOPS/etcd encryption) is enough.
- A WAF/CDN; edge rate limits and CrowdSec/fail2ban are the intended layer.
- Multi-tenant directories; one directory per cluster remains the model.
- A hosted service; this plan is about the self-hosted product.
