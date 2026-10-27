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

export type App = { name: string; host: string; auth: string; public?: string[] };
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

// The app's API key, exported by ci/e2e.sh from the `apiKey` entry in
// ci/apps.json (UMKIM_THIN_<APP>_API_KEY). The *arr apps keep theirs in a chart
// Secret, so the CI reads it there rather than from a config file.
export function appApiKey(app: string): string {
  const key = app.toUpperCase().replace(/[^A-Z0-9]/g, '_');
  return process.env[`UMKIM_THIN_${key}_API_KEY`] ?? '';
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
      // An invitation never takes over an existing account. That is fine (and
      // expected when several journeys share a user, or when a run is repeated
      // against a live cluster), so treat it as success.
      if (/already exists|never takes over/i.test(text)) return;
      lastError = `invite redeem failed: ${redeemed.status()} ${text}`;
      await new Promise((r) => setTimeout(r, 5000));
    } finally {
      await ctx.dispose().catch(() => {});
    }
  }
  throw new Error(lastError || `could not invite ${username}`);
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
  const inFlight = () => /[?&]code=|oidc|oauth/i.test(page.url());

  // The portal SPA: fill and submit when the form is shown. A browser that
  // already holds a session just gets redirected, so an absent form is fine.
  const portalLogin = async () => {
    if (!onAuth()) return;
    const u = page.locator('#username-textfield, input[autocomplete="username"]').first();
    try {
      await u.waitFor({ state: 'visible', timeout: 8_000 });
    } catch {
      return;
    }
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

    const sso = page.locator(
      'button:has-text("SSO"), a:has-text("SSO"), button:has-text("Sign in with"), a:has-text("Sign in with"), a:has-text("OpenID"), button:has-text("OpenID")',
    );
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
