import { APIRequestContext, Browser, BrowserContext, Page, request } from '@playwright/test';
import zlib from 'node:zlib';

// Shared helpers for the acceptance gate and functional user journeys.
//
// Env (set by ci/e2e.sh):
//   UMKIM_THIN_ZONE, UMKIM_THIN_AUTH_HOST, UMKIM_THIN_APPS
//   UMKIM_THIN_USER / UMKIM_THIN_PASSWORD   an Authelia/directory admin
//   UMKIM_THIN_<APP>_ADMIN_USER/PASSWORD    the app's own admin (where it has one)

export const ZONE = process.env.UMKIM_THIN_ZONE || '';
export const USER = process.env.UMKIM_THIN_USER || 'admin';
export const PASS = process.env.UMKIM_THIN_PASSWORD || '';
// The regular tenant used for the gating portal/app-login checks. The console
// and the invite API keep using the directory admin (USER/PASS).
export const PORTAL_USER = process.env.UMKIM_THIN_PORTAL_USER || USER;
export const PORTAL_PASS = process.env.UMKIM_THIN_PORTAL_PASSWORD || PASS;
export const AUTH_HOST = process.env.UMKIM_THIN_AUTH_HOST || (ZONE ? `auth.${ZONE}` : '');
export const CONSOLE_HOST = ZONE ? `console.${ZONE}` : '';

export type App = {
  name: string;
  host: string;
  auth: string;
  public?: string[];
  // Which per-app token adapter can mint an API token (see appToken). Declared
  // in ci/apps.json so the harness stays data-driven: adding an app that needs a
  // token never means editing this file, only registering an adapter below.
  token?: { adapter?: string };
};
export const APPS: App[] = JSON.parse(process.env.UMKIM_THIN_APPS || '[]');

export function installed(name: string): boolean {
  return APPS.some((a) => a.name === name);
}

export function hostOf(name: string): string {
  return APPS.find((a) => a.name === name)?.host ?? '';
}

export function basic(user: string, pass: string): string {
  return 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64');
}

export async function anon(): Promise<APIRequestContext> {
  return request.newContext({ ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1' });
}

// The app's own admin account. ci/e2e.sh exports these from the `admin` entry
// in ci/apps.json (UMKIM_THIN_<APP>_ADMIN_USER / _ADMIN_PASSWORD).
export function appAdmin(app: string): { user: string; pass: string } {
  const key = app.toUpperCase().replace(/[^A-Z0-9]/g, '_');
  return {
    user: process.env[`UMKIM_THIN_${key}_ADMIN_USER`] ?? '',
    pass: process.env[`UMKIM_THIN_${key}_ADMIN_PASSWORD`] ?? '',
  };
}

// appApiKey is the app's own *shared* API key (the *arr apps keep theirs in a
// chart Secret). It is not a per-user credential: see appToken for that.
export function appApiKey(app: string): string {
  const key = app.toUpperCase().replace(/[^A-Z0-9]/g, '_');
  return process.env[`UMKIM_THIN_${key}_API_KEY`] ?? '';
}

// ---------------------------------------------------------------------------
// Per-user API tokens
// ---------------------------------------------------------------------------
//
// Some apps have no session API a test can call: every request needs a token
// the user mints in their own profile (BookStack, Vikunja). Without one, the
// gate can only prove that a login page loads - which is what "could not be
// verified" meant for Miniflux, and why it was dropped.
//
// The adapters below are deliberately per-app rather than one generic
// implementation, because the retrieval differs in every case:
//
//   vikunja   PUT  /api/v1/tokens        JWT -> scoped API token, one call
//   bookstack GET  /api-tokens/<id>/...  HTML form + CSRF, no JSON API for it
//
// A shared interface keeps the call sites identical, and ci/apps.json decides
// which adapter an app uses so this file does not grow an app list.

type TokenRequest = { user: string; pass: string };

// anonTokenContext is a request context for an app host. `post`/`get` carry a
// token if the adapter has one, and the caller's credentials are passed
// explicitly because these are the app's own accounts (not the portal's).
type TokenAdapter = (host: string, cred: TokenRequest) => Promise<string>;

// loginJWT posts to an app's local-login endpoint and returns the bearer token
// it issues. Only used by adapters whose app accepts that for the next step.
//
// The call goes through the app's PUBLIC host, which sits behind Authelia for
// every catalog app, so the request needs the portal session's cookie *and* the
// app's own credentials - two different accounts. Without the cookie Traefik
// answers 303 to the portal and the reply is a redirect, not a token. (curl on
// the node against the Service works, but that is not the path a client uses.)
async function loginJWT(
  host: string,
  path: string,
  cred: TokenRequest,
  field = 'token',
): Promise<{ token: string; ctx: Awaited<ReturnType<typeof anon>> }> {
  // The portal session belongs to a directory account; the app login belongs to
  // the app's own local admin. PORTAL_USER/USER are the directory identities the
  // rest of the suite uses.
  const ctx = await portalSession();
  const r = await ctx.post(`https://${host}${path}`, {
    data: { username: cred.user, password: cred.pass },
    headers: { 'Content-Type': 'application/json' },
  });
  if (!r.ok()) {
    const body = (await r.text()).slice(0, 200);
    await ctx.dispose();
    throw new Error(`login at ${path} for ${cred.user} -> ${r.status()} ${body}`);
  }
  const body = await r.json().catch(() => ({}));
  const token: string = body?.[field] ?? '';
  if (!token) {
    await ctx.dispose();
    throw new Error(`login at ${path} returned no ${field}: ${JSON.stringify(body).slice(0, 200)}`);
  }
  return { token, ctx };
}

// vikunjaToken mints a scoped API token from a username/password login.
//
// Verified against the upstream router (pkg/routes/routes.go): the endpoint is
// PUT /api/v1/tokens, NOT /api/v1/user/token (that path only refreshes a JWT).
// A token needs a title, an expiry and explicit permissions, and the cleartext
// is returned exactly once, in the creation response.
const vikunjaToken: TokenAdapter = async (host, cred) => {
  const { token: jwt, ctx } = await loginJWT(host, '/api/v1/login', cred);
  try {
    const expires = new Date(Date.now() + 24 * 3600 * 1000).toISOString();
    const r = await ctx.put(`https://${host}/api/v1/tokens`, {
      headers: { Authorization: `Bearer ${jwt}`, 'Content-Type': 'application/json' },
      data: {
        title: 'umkim-thin-ci',
        expires_at: expires,
        // Explicit rather than "everything": a token that cannot write would
        // make the gating check pass for the wrong reason.
        //
        // read_one matters and is easy to miss: Vikunja derives the required
        // permission from the route's method and path, so a single-item
        // GET /api/v1/tasks/<id> is "read_one" while the collection is
        // "read_all". Without it the read-back 401s even though the create
        // worked. Verified against a running instance.
        permissions: {
          projects: ['read_all', 'read_one', 'create', 'update', 'delete'],
          tasks: ['read_all', 'read_one', 'create', 'update', 'delete'],
        },
      },
    });
    if (!r.ok()) {
      throw new Error(`vikunja token -> ${r.status()} ${(await r.text()).slice(0, 300)}`);
    }
    const body = await r.json().catch(() => ({}));
    const token: string = body?.token ?? '';
    if (!token) throw new Error(`vikunja token response carried no token: ${JSON.stringify(body).slice(0, 300)}`);
    return token;
  } finally {
    await ctx.dispose();
  }
};

// bookstackToken mints an API token through BookStack's own account form.
//
// There is no JSON endpoint for this: a token is created by POSTing a form, and
// the secret is flashed to the session and rendered once on the redirect target.
// Verified end to end against a real container, which is where all of these
// details came from:
//
//   * login is a form POST with a CSRF `_token` scraped from /login;
//   * the session cookie is `Secure`, so it is sent explicitly rather than left
//     to cookie-jar host matching (the CI talks to https anyway, but the same
//     code runs against a plain-http dev URL);
//   * the user id comes from /my-account/auth;
//   * POST /api-tokens/<userId>/create returns 302 to /api-tokens/<userId>/<id>;
//     the page there carries the secret in the `token_id` field;
//   * the API wants `Authorization: Token <id>:<secret>` - id alone is 401.
const bookstackToken: TokenAdapter = async (host, cred) => {
  const base = `https://${host}`;
  const ctx = await portalSession();
  try {
    const loginPage = await ctx.get(`${base}/login`);
    if (!loginPage.ok()) throw new Error(`bookstack /login -> ${loginPage.status()}`);
    const csrf = ((await loginPage.text()).match(/name="_token"\s+value="([^"]+)"/) ?? [])[1];
    if (!csrf) throw new Error('bookstack /login had no CSRF token');

    const login = await ctx.post(`${base}/login`, {
      form: { _token: csrf, email: cred.user, password: cred.pass },
      maxRedirects: 0,
    });
    // A successful login is a 302; a failed one re-renders the form (200).
    if (login.status() !== 302) {
      throw new Error(`bookstack login for ${cred.user} -> ${login.status()}`);
    }

    const account = await ctx.get(`${base}/my-account/auth`);
    const userId = ((await account.text()).match(/\/api-tokens\/(\d+)\/create/) ?? [])[1];
    if (!userId) throw new Error('bookstack: no user id on /my-account/auth (session not established?)');

    const form = await ctx.get(`${base}/api-tokens/${userId}/create`);
    const formCsrf = ((await form.text()).match(/name="_token"\s+value="([^"]+)"/) ?? [])[1];
    if (!formCsrf) throw new Error('bookstack token form had no CSRF token');

    // maxRedirects: 0 so the redirect target is known; the secret is rendered
    // there in the same session, so fetch it immediately.
    const created = await ctx.post(`${base}/api-tokens/${userId}/create`, {
      form: { _token: formCsrf, name: `umkim-thin-ci-${Date.now()}`, expires_at: '2030-01-01' },
      maxRedirects: 0,
    });
    if (created.status() !== 302) {
      throw new Error(`bookstack token create -> ${created.status()}`);
    }
    const location = created.headers()['location'] ?? '';
    if (!location) throw new Error('bookstack token create returned no redirect target');

    const page = await ctx.get(location.startsWith('http') ? location : `${base}${location}`);
    const html = await page.text();
    // The id is a form field; the secret is a bare readonly input with NO name
    // attribute (see resources/views/users/api-tokens/edit.blade.php). Excluding
    // named inputs is what tells the two readonly fields apart.
    const id = (html.match(/name="token_id"[^>]*value="([^"]+)"/) ?? [])[1];
    const secret = (html.match(/<input(?![^>]*\bname=)[^>]*\breadonly[^>]*\bvalue="([^"]+)"/) ?? [])[1];
    if (!id || !secret) {
      throw new Error('bookstack token page did not expose both halves of the token');
    }
    return `${id}:${secret}`;
  } finally {
    await ctx.dispose();
  }
};

const ADAPTERS: Record<string, TokenAdapter> = {
  vikunja: vikunjaToken,
  bookstack: bookstackToken,
};

// appToken returns a per-user API token for `app`, minted as `cred`.
//
// The adapter is chosen by the app's entry in ci/apps.json, so an app that
// needs a token declares it there instead of this file knowing app names. An
// app with no adapter is an error rather than an empty string: a test that
// silently got '' would assert against an unauthenticated app and pass.
export async function appToken(
  app: string,
  cred?: Partial<TokenRequest>,
): Promise<string> {
  const entry = APPS.find((a) => a.name === app);
  const adapterName = entry?.token?.adapter ?? app;
  const adapter = ADAPTERS[adapterName];
  if (!adapter) {
    throw new Error(
      `no token adapter for ${app} (declared: ${adapterName}); add one to lib/session.ts and set token.adapter in ci/apps.json`,
    );
  }
  const host = hostOf(app);
  if (!host) throw new Error(`${app} is not in UMKIM_THIN_APPS`);
  const fallback = appAdmin(app);
  const user = cred?.user ?? fallback.user;
  const pass = cred?.pass ?? fallback.pass;
  if (!user || !pass) {
    throw new Error(`appToken(${app}) needs credentials; set the app admin or pass them explicitly`);
  }
  return adapter(host, { user, pass });
}


// portalSession logs in through the Authelia portal and returns a context whose
// cookie jar carries the session (the cookie is scoped to the zone, so it is
// sent to every app subdomain).
export async function portalSession(
  credentials?: { username: string; password: string },
): Promise<APIRequestContext> {
  const u = credentials?.username ?? USER;
  const p = credentials?.password ?? PASS;
  const ctx = await anon();
  await ctx.get(`https://${AUTH_HOST}/`);
  const r = await ctx.post(`https://${AUTH_HOST}/api/firstfactor`, {
    data: { username: u, password: p, targetURL: `https://${AUTH_HOST}/` },
  });
  if (r.status() !== 200) {
    throw new Error(`authelia firstfactor for ${u} -> ${r.status()}`);
  }
  return ctx;
}

// consoleSession additionally establishes the console's own session (for the
// invite/app APIs, which require it).
export async function consoleSession(): Promise<APIRequestContext> {
  const ctx = await portalSession();
  await ctx.post(`https://${CONSOLE_HOST}/login`, { form: { username: USER, password: PASS } });
  return ctx;
}

// createUser onboards a directory user through an invite (the same path a real
// invitee takes): create the invite in the console, then redeem it publicly.
export async function createUser(
  username: string,
  password: string,
  opts?: { displayName?: string; email?: string },
): Promise<void> {
  let lastError = '';
  for (let attempt = 1; attempt <= 5; attempt++) {
    let ctx: APIRequestContext;
    try {
      ctx = await consoleSession();
    } catch (err) {
      lastError = `console login failed: ${String(err)}`;
      await new Promise((r) => setTimeout(r, 5000));
      continue;
    }
    try {
      const created = await ctx.post(`https://${CONSOLE_HOST}/api/invites`, {
        data: {
          displayName: opts?.displayName ?? username,
          group: 'default-tenant',
          email: opts?.email ?? `${username}@ci.local`,
        },
      });
      const body = await created.json().catch(() => ({}));
      const token: string | undefined = body?.invite?.token;
      if (!token) {
        // The console can briefly return an empty body while a restarted pod is
        // settling; retry with a fresh console session/token.
        lastError = `invite create failed: ${created.status()} ${JSON.stringify(body)}`;
        await new Promise((r) => setTimeout(r, 5000));
        continue;
      }
      const redeemed = await ctx.post(`https://${CONSOLE_HOST}/api/invites/redeem`, {
        data: {
          token,
          username,
          password,
          displayName: opts?.displayName ?? username,
          email: opts?.email ?? `${username}@ci.local`,
        },
      });
      if (redeemed.ok()) return;
      const text = await redeemed.text();
      // An invitation never takes over an existing account, which is expected when
      // a journey is retried or two tests share a user. Returning quietly is wrong
      // though: the caller then holds a password that was never set, and every
      // later login fails with what looks like a bad credential.
      if (/already exists|never takes over/i.test(text)) {
        // The account predates this run with an unknown password. Delete it and
        // invite again, so the credentials returned here are the ones that work.
        // (ci/portal-user.sh does the same for its own tenant.)
        if (await deleteDirectoryUser(ctx, username)) continue;
        return;
      }
      lastError = `invite redeem failed: ${redeemed.status()} ${text}`;
      await new Promise((r) => setTimeout(r, 5000));
    } finally {
      await ctx.dispose().catch(() => {});
    }
  }
  throw new Error(lastError || `could not invite ${username}`);
}


// deleteDirectoryUser removes a directory account through the console API. It is
// only used for the ci-* test accounts this suite creates.
async function deleteDirectoryUser(ctx: APIRequestContext, username: string): Promise<boolean> {
  if (!/^ci-/.test(username)) return false;
  const r = await ctx.delete(`https://${CONSOLE_HOST}/api/directory/users/${username}`);
  return r.ok() || r.status() === 404;
}

// oidcLogin drives the real browser SSO flow of an OIDC app: open the app, get
// past the forward-auth portal if there is one, sign in at the app's own SSO
// button (or let it redirect on its own), and land back inside the app. This is
// the path a user takes, and it exercises the app's client -> issuer -> token ->
// JWKS chain, including TLS trust of the issuer.
//
// `ready` lets a caller say what "signed in" means for its app (a token in
// storage, a cookie, a URL); it is polled until it holds.
export async function oidcLogin(
  browser: Browser,
  appHost: string,
  opts?: {
    path?: string;
    credentials?: { username: string; password: string };
    ready?: (page: Page) => Promise<boolean>;
    onPage?: (page: Page) => void;
  },
): Promise<{ context: BrowserContext; page: Page }> {
  const user = opts?.credentials?.username ?? USER;
  const pass = opts?.credentials?.password ?? PASS;
  const context = await browser.newContext({
    ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
  });
  const page = await context.newPage();
  opts?.onPage?.(page);
  const onAuth = () => page.url().includes(AUTH_HOST);
  // An OAuth round trip is in flight: the app is exchanging a code, or an
  // oidc/oauth route is doing the redirect. Clicking SSO again would start over.
  //
  // Do not match a bare "oidc"/"oauth" in the app's path: an app that serves its
  // SSO link under /oauth2/oidc/... makes its own login page look like a flow in
  // progress, and the helper then never clicks the button. Only a completed
  // authorization code on the URL, or the provider's own authorize endpoint,
  // means a round trip is actually running.
  const inFlight = () => {
    if (/[?&]code=/.test(page.url())) return true;
    try {
      const u = new URL(page.url());
      return /\/api\/oidc\/authorization|\.well-known\/openid-configuration/.test(u.pathname + '?' + u.search);
    } catch {
      return false;
    }
  };

  // The portal SPA: fill and submit when the form is shown. A browser that
  // already holds a session just gets redirected, so an absent form is fine.
  const portalLogin = async () => {
    if (!onAuth()) return;
    // Authelia renders its form after the SPA mounts. Typing before that loses
    // the keystrokes, so wait for the field to be attached *and* visible before
    // clicking into it - the visible check alone passes on a hidden duplicate.
    await page.waitForLoadState('domcontentloaded').catch(() => {});
    const u = page.locator('#username-textfield, input[autocomplete="username"]').first();
    try {
      await u.waitFor({ state: 'visible', timeout: 20_000 });
    } catch {
      return;
    }
    // A brief settle: the form can be visible while React is still attaching
    // its handlers, and a submit in that window is silently dropped.
    await page.waitForTimeout(1500);
    await u.click();
    await u.fill('');
    await u.pressSequentially(user, { delay: 20 });
    const p = page.locator('#password-textfield, input[autocomplete="current-password"]').first();
    await p.click();
    await p.fill('');
    await p.pressSequentially(pass, { delay: 20 });
    await p.press('Enter');
    await page
      .waitForURL((x) => !x.host.startsWith('auth.'), { timeout: 45_000 })
      .catch(() => {});
  };

  await page.goto(`https://${appHost}${opts?.path ?? '/'}`);

  const isReady = async () => (opts?.ready ? opts.ready(page) : Promise.resolve(!onAuth()));
  for (let pass = 0; pass < 8; pass++) {
    if (await isReady()) break;
    await portalLogin();
    await page.waitForLoadState('domcontentloaded').catch(() => {});
    if (await isReady()) break;
    if (inFlight()) {
      await page.waitForTimeout(2000);
      continue;
    }

    // Provider-specific labels first: SparkyFitness also has a "Sign in with
    // Passkey" button, which a generic "Sign in with" selector would click
    // instead of the OIDC provider.
    //
    // BookStack's SSO control is a bare <button id="oidc-login"> with an icon
    // and no text at all, so it has to be matched by id - but ONLY on the app's
    // own page. Matching it on the portal would click the portal's own form
    // instead of completing the password step.
    let sso = onAuth()
      ? page.locator('button:has-text("Authelia"), a:has-text("Authelia")')
      : page.locator('#oidc-login, button:has-text("Authelia"), a:has-text("Authelia")');
    if ((await sso.count()) === 0) {
      sso = page.locator(
        // "Continue with <provider>" is Open WebUI's wording.
        'button:has-text("Continue with"), a:has-text("Continue with"), button:has-text("SSO"), a:has-text("SSO"), button:has-text("OpenID"), a:has-text("OpenID"), button:has-text("Sign in with"):not(:has-text("Passkey")), a:has-text("Sign in with")',
      );
    }
    if (await sso.count()) {
      await sso.first().click();
      await page.waitForTimeout(2000);
      continue;
    }
    await page.waitForTimeout(1500);
  }

  // give a late redirect a moment, then stop
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline && !(await isReady())) await page.waitForTimeout(1000);
  return { context, page };
}

// tokenInStorage reports whether a Vikunja page holds a signed-in session, and
// doubles as a generic "the app stored its own token" readiness probe.
//
// Vikunja does NOT use a cookie for API auth: the SPA keeps a JWT in
// localStorage under `token` and sends it as a bearer header. So a
// `fetch(..., {credentials:'same-origin'})` from the page is anonymous even
// after a successful SSO round trip, which is what made the first version of
// this check fail while the browser was visibly logged in.
export async function tokenInStorage(page: Page, key = 'token'): Promise<boolean> {
  return page
    .evaluate((k) => Boolean(localStorage.getItem(k)), key)
    .catch(() => false);
}

// png builds a valid w*h RGB PNG. Image pipelines (thumbnailers, OCR previews)
// reject degenerate 1x1 images, so the journeys use a real picture.
export function png(width = 64, height = 64): Buffer {
  const raw = Buffer.alloc((width * 3 + 1) * height);
  for (let y = 0; y < height; y++) {
    raw[y * (width * 3 + 1)] = 0; // filter: none
    for (let x = 0; x < width; x++) {
      const o = y * (width * 3 + 1) + 1 + x * 3;
      raw[o] = x * 4;
      raw[o + 1] = y * 4;
      raw[o + 2] = 128;
    }
  }
  const table = Array.from({ length: 256 }, (_, n) => {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    return c >>> 0;
  });
  const crc = (b: Buffer) => {
    let c = 0xffffffff;
    for (const x of b) c = table[(c ^ x) & 0xff] ^ (c >>> 8);
    return (c ^ 0xffffffff) >>> 0;
  };
  const chunk = (type: string, data: Buffer) => {
    const len = Buffer.alloc(4);
    len.writeUInt32BE(data.length);
    const td = Buffer.concat([Buffer.from(type), data]);
    const c = Buffer.alloc(4);
    c.writeUInt32BE(crc(td));
    return Buffer.concat([len, td, c]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // colour type: RGB
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

// waitFor polls fn until it returns true (or throws on the deadline).
export async function waitFor(
  fn: () => Promise<boolean>,
  opts?: { timeoutMs?: number; intervalMs?: number; what?: string },
): Promise<void> {
  const deadline = Date.now() + (opts?.timeoutMs ?? 60_000);
  const interval = opts?.intervalMs ?? 1000;
  while (Date.now() < deadline) {
    if (await fn()) return;
    await new Promise((r) => setTimeout(r, interval));
  }
  throw new Error(`timed out waiting for ${opts?.what ?? 'the condition'}`);
}
