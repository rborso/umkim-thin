{{/*
Refuse an install that relies on placeholder or empty credentials. The identity
chart is a platform component: an empty secret is never a safe default, so the
render fails here rather than deploying a login nobody can trust.
*/}}
{{- define "identity.validate" -}}
{{- $_ := required "lldap.baseDN is required (e.g. dc=example,dc=com)" .Values.lldap.baseDN -}}
{{- $_ := required "authelia.portalHost is required" .Values.authelia.portalHost -}}
{{- $_ := required "authelia.cookieDomain is required" .Values.authelia.cookieDomain -}}
{{- $_ := required "authelia.sessionSecret is required" .Values.authelia.sessionSecret -}}
{{- $_ := required "authelia.storageEncryptionKey is required" .Values.authelia.storageEncryptionKey -}}
{{- $_ := required "authelia.resetPasswordJWTSecret is required" .Values.authelia.resetPasswordJWTSecret -}}
{{- if not .Values.authelia.accessDomains -}}
{{- fail "authelia.accessDomains must list the hosts that may use the portal" -}}
{{- end -}}
{{- if .Values.lldap.deploy -}}
{{- $_ := required "lldap.adminPassword is required" .Values.lldap.adminPassword -}}
{{- $_ := required "lldap.jwtSecret is required" .Values.lldap.jwtSecret -}}
{{- else -}}
{{- $_ := required "lldap.externalUrl is required when lldap.deploy is false" .Values.lldap.externalUrl -}}
{{- $_ := required "authelia.ldapPassword is required when lldap.deploy is false" .Values.authelia.ldapPassword -}}
{{- end -}}
{{- if .Values.signup.enabled -}}
{{- $_ := required "signup.redeemURL is required when signup.enabled is true" .Values.signup.redeemURL -}}
{{- end -}}
{{- if .Values.authelia.oidc.enabled -}}
{{- $_ := required "authelia.oidc.hmacSecret is required when OIDC is enabled" .Values.authelia.oidc.hmacSecret -}}
{{- $_ := required "authelia.oidc.issuerPrivateKey is required when OIDC is enabled" .Values.authelia.oidc.issuerPrivateKey -}}
{{- end -}}
{{- end -}}

{{- define "identity.namespace" -}}
{{- .Values.namespace | default "identity" -}}
{{- end -}}

{{- define "identity.protectedIngressMiddlewares" -}}
{{- $t := .Values.traefik -}}
{{- printf "%s@file,%s@file" ($t.middlewareName | default "identity-authelia") ($t.stripAuthorizationMiddlewareName | default "strip-authorization") -}}
{{- end -}}

{{- define "identity.image" -}}
{{- $repo := .repository -}}
{{- if .Values.registry.prefix -}}
{{- printf "%s/%s:%s" .Values.registry.prefix $repo .tag -}}
{{- else -}}
{{- printf "%s:%s" $repo .tag -}}
{{- end -}}
{{- end -}}

{{- define "identity.inviteSignupNginx" -}}
server {
  listen 8080;
  {{- /*
    Resolve the console Service at request time, not at startup: during a fresh
    install the identity release comes up before the console exists, and a
    literal upstream name makes nginx exit (and CrashLoop).
  */}}
  resolver {{ .Values.cluster.dnsServiceIP | default "10.43.0.10" }} valid=10s ipv6=off;
  set $redeem "{{ .Values.signup.redeemURL }}";
  location = /invite/redeem {
    proxy_pass $redeem;
    proxy_http_version 1.1;
    proxy_set_header Content-Type application/json;
  }
  location /invite {
    root /usr/share/nginx/html;
    try_files /index.html =404;
  }
  location / {
    return 302 /invite;
  }
}
{{- end -}}

{{- define "identity.inviteSignupIndex" -}}
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Accept invitation</title>
<style>
body{font-family:system-ui,sans-serif;max-width:28rem;margin:3rem auto;padding:0 1rem;color:#151515}
label{display:block;margin:.75rem 0 .25rem;font-weight:600}
input{width:100%;padding:.5rem;box-sizing:border-box}
button{margin-top:1rem;padding:.6rem 1rem;font-weight:600}
button:disabled{opacity:.6;cursor:not-allowed}
.msg{margin-top:1rem;padding:.75rem;border-radius:.25rem}
.err{background:#faeae8;color:#a30000}
.ok{background:#e7f5e8;color:#1e7b34}
</style>
</head>
<body>
<h1>Accept invitation</h1>
<p>Create your account for this directory.</p>
<form id="f">
  <label>Username</label><input id="username" required pattern="[a-z0-9-]+">
  <label>Display name</label><input id="displayName">
  <label>Password (min 8 characters)</label><input id="password" type="password" required>
  <label>Confirm password</label><input id="confirm" type="password" required>
  <button id="submit" type="submit">Create account</button>
</form>
<div id="msg"></div>
<script>
const params = new URLSearchParams(location.search);
const token = params.get('token') || '';
const msg = document.getElementById('msg');
const btn = document.getElementById('submit');
let busy = false;
document.getElementById('f').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (busy) return;
  msg.className = 'msg'; msg.textContent = '';
  const username = document.getElementById('username').value.trim().toLowerCase();
  const password = document.getElementById('password').value;
  const confirm = document.getElementById('confirm').value;
  if (password.length < 8) { msg.className='msg err'; msg.textContent='Password must be at least 8 characters.'; return; }
  if (password !== confirm) { msg.className='msg err'; msg.textContent='Passwords do not match.'; return; }
  busy = true; btn.disabled = true; btn.textContent = 'Creating account…';
  msg.className='msg'; msg.textContent='Creating your account, this can take a few seconds…';
  try {
    const r = await fetch('/invite/redeem', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({token,username,password,displayName:document.getElementById('displayName').value})});
    const text = await r.text();
    if (!r.ok) throw new Error(text || ('HTTP '+r.status));
    msg.className='msg ok'; msg.textContent='Account created: '+username+'. You can now sign in.';
  } catch (err) {
    msg.className='msg err'; msg.textContent=String(err.message||err);
  } finally {
    busy = false; btn.disabled = false; btn.textContent = 'Create account';
  }
});
if (!token) { msg.className='msg err'; msg.textContent='This link is missing its invite token.'; }
</script>
</body>
</html>
{{- end -}}

{{- define "identity.autheliaConfig" -}}
{{- $ldapPass := .Values.authelia.ldapPassword | default .Values.lldap.adminPassword -}}
{{- $publicURL := .Values.authelia.publicURL | default (printf "https://%s" .Values.authelia.portalHost) -}}
theme: light
# Some virtualized and freshly booted nodes have no reliable clock yet;
# Authelia's startup clock check would crash the pod in that window.
ntp:
  disable_startup_check: true
server:
  address: 'tcp://0.0.0.0:{{ .Values.authelia.port }}'
log:
  level: info
identity_validation:
  reset_password:
    jwt_secret: {{ .Values.authelia.resetPasswordJWTSecret | quote }}
authentication_backend:
  ldap:
    address: {{ .Values.lldap.externalUrl | default (printf "ldap://lldap.%s.svc.cluster.local:%v" (include "identity.namespace" .) .Values.lldap.ldapPort) | quote }}
    implementation: lldap
    base_dn: {{ .Values.lldap.baseDN | quote }}
    user: {{ .Values.authelia.ldapUser | default (printf "uid=admin,ou=people,%s" .Values.lldap.baseDN) | quote }}
    password: {{ $ldapPass | quote }}
    users_filter: "(&({username_attribute}={input})(objectClass=person))"
    groups_filter: "(&(member={dn})(objectClass=groupOfNames))"
    attributes:
      username: uid
      display_name: displayName
      mail: mail
      group_name: cn
{{- if .Values.authelia.oidc.enabled }}
identity_providers:
  oidc:
    hmac_secret: {{ .Values.authelia.oidc.hmacSecret | quote }}
    issuer_private_key: |
{{ .Values.authelia.oidc.issuerPrivateKey | indent 6 }}
    clients:
{{- range .Values.authelia.oidc.clients }}
      - client_id: {{ .clientId | quote }}
        client_name: {{ .clientName | default .clientId | quote }}
        client_secret: {{ .clientSecret | quote }}
        public: false
        authorization_policy: {{ .authorizationPolicy | default "one_factor" }}
        require_pkce: {{ if hasKey . "requirePkce" }}{{ .requirePkce }}{{ else }}true{{ end }}
{{- if or (not (hasKey . "requirePkce")) .requirePkce }}
        pkce_challenge_method: S256
{{- end }}
        redirect_uris:
{{- range .redirectUris }}
          - {{ . | quote }}
{{- end }}
        scopes:
{{- range (.scopes | default (list "openid" "profile" "email")) }}
          - {{ . | quote }}
{{- end }}
        response_types:
          - code
        grant_types:
          - authorization_code
        consent_mode: {{ .consentMode | default "implicit" }}
        token_endpoint_auth_method: {{ .tokenEndpointAuthMethod | default "client_secret_basic" }}
{{- end }}
{{- end }}
access_control:
  default_policy: deny
  rules:
    - domain: {{ .Values.authelia.portalHost | quote }}
      policy: bypass
{{- range .Values.authelia.publicPathRules }}
    # Paths an app declares reachable without a session (share links, native
    # clients). They must precede the authenticated rule: Authelia takes the
    # first match.
    - domain: {{ .domain | quote }}
      resources:
{{- range .paths }}
        - {{ . | quote }}
{{- end }}
      policy: bypass
{{- end }}
{{- range .Values.authelia.accessRules }}
    - domain: {{ .domain | quote }}
      subject:
{{- range .groups }}
        - {{ printf "group:%s" . | quote }}
{{- end }}
      policy: one_factor
    # Deny authenticated users who are not in the allowed groups; Authelia
    # skips a subject rule when it does not match, so an explicit deny must
    # precede the zone wildcard rules.
    - domain: {{ .domain | quote }}
      policy: deny
{{- end }}
{{- range .Values.authelia.accessDomains }}
    - domain: {{ . | quote }}
      policy: one_factor
{{- end }}
# Brute-force protection: five failures in two minutes ban the source for five
# minutes. This is the regulation layer; MFA is a user enrolment decision.
regulation:
  max_retries: 5
  find_time: 2m
  ban_time: 5m
session:
  secret: {{ .Values.authelia.sessionSecret | quote }}
{{- if .Values.valkey.enabled }}
  redis:
    host: {{ printf "valkey.%s.svc.cluster.local" (include "identity.namespace" .) | quote }}
    port: 6379
    timeout: 5s
    max_retries: 3
    database_index: 0
    maximum_active_connections: 8
    minimum_idle_connections: 1
{{- if .Values.valkey.password }}
    password: {{ .Values.valkey.password | quote }}
{{- end }}
{{- end }}
  cookies:
    - domain: {{ .Values.authelia.cookieDomain | quote }}
      name: authelia_session
      same_site: lax
      expiration: 8h
      inactivity: 30m
      authelia_url: {{ $publicURL | quote }}
      default_redirection_url: {{ printf "https://%s" .Values.authelia.cookieDomain | quote }}
storage:
  encryption_key: {{ .Values.authelia.storageEncryptionKey | quote }}
  local:
    path: /data/db.sqlite3
notifier:
  filesystem:
    filename: /data/notification.txt
{{- end -}}

{{/*
Ensure every group the cluster's routes declare exists in this cluster's own
directory. Route access rules (route.requireAuthGroups) are the declaration:
without this, an operator has to create each tenant group by hand before it can
be used, and a group deleted in the directory is never restored.
*/}}
{{- define "identity.ensureManagedGroupsScript" -}}
set -uo pipefail
url="${LLDAP_URL}"
for i in $(seq 1 40); do
  curl -fsS "${url}/health" >/dev/null 2>&1 && break
  sleep 3
done
token=""
for i in $(seq 1 10); do
  token=$(curl -fsS -X POST "${url}/auth/simple/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"${LLDAP_ADMIN_PASSWORD}\"}" 2>/dev/null \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "${token}" ] && break
  sleep 3
done
if [ -z "${token}" ]; then echo "LLDAP admin login failed"; exit 1; fi
gql() { curl -fsS -X POST "${url}/api/graphql" \
  -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' --data "$1"; }

if [ -z "${MANAGED_GROUPS:-}" ]; then
  echo "==> no declared groups; nothing to reconcile"
  exit 0
fi
groups=$(gql '{"query":"{ groups { id displayName } }"}') || { echo "listing directory groups failed"; exit 1; }
created=0
for g in ${MANAGED_GROUPS}; do
  case "${g}" in
    *[!A-Za-z0-9_-]*|'') echo "skipping unsafe group name ${g}"; continue;;
  esac
  if printf '%s' "${groups}" | tr '{' '\n' | grep -qF "\"displayName\":\"${g}\""; then
    echo "==> group ${g} present"
    continue
  fi
  if gql "{\"query\":\"mutation{createGroup(name:\\\"${g}\\\"){id displayName}}\"}" >/dev/null; then
    echo "==> created group ${g}"
    created=$((created + 1))
  else
    echo "creating group ${g} failed"; exit 1
  fi
done
echo "==> declared groups reconciled (${created} created)"
{{- end -}}

{{/*
The effective set of directory groups this cluster owns: the route-declared
groups plus the default tenant. Space-joined so it can feed the Job env and,
through its hash, the Job name (which must change when the set does).
*/}}
{{- define "identity.managedGroups" -}}
{{- $groups := .Values.lldap.managedGroups | default list -}}
{{- $default := .Values.lldap.defaultTenant | default "" -}}
{{- if and $default (not (has $default $groups)) -}}
{{- $groups = append $groups $default -}}
{{- end -}}
{{- join " " $groups -}}
{{- end -}}

{{/*
Pod spec for reconciling declared directory groups. Shared by the hook Job and
the drift CronJob so both run exactly the same script.
*/}}
{{- define "identity.managedGroupsJobSpec" -}}
backoffLimit: 4
template:
  metadata:
    labels:
      app.kubernetes.io/name: identity
      app.kubernetes.io/component: lldap-managed-groups
  spec:
    restartPolicy: OnFailure
    containers:
      - name: managed-groups
        image: {{ .Values.lldap.image | quote }}
        command: ["/bin/bash", "-c"]
        args:
          - |
{{ include "identity.ensureManagedGroupsScript" . | indent 12 }}
        env:
          - name: LLDAP_URL
            value: "http://lldap:{{ .Values.lldap.httpPort }}"
          - name: LLDAP_ADMIN_PASSWORD
            valueFrom:
              secretKeyRef:
                name: lldap-credentials
                key: LLDAP_LDAP_USER_PASS
          - name: MANAGED_GROUPS
            value: {{ include "identity.managedGroups" . | quote }}
        resources:
          requests: { cpu: 5m, memory: 32Mi }
          limits: { memory: 128Mi }
{{- end -}}
