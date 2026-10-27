import { test, expect, Page } from '@playwright/test';

// Functional console test: a real browser signs in through Authelia, then the
// console's own login, and uses the page exactly as an operator would.
//
// This replaces the old curl-based cookie assertion: that test was sensitive to
// cookie-jar/session details and did not exercise the UI. Run manually with the
// same UMKIM_THIN_* variables used by ci/e2e.sh:
//
//   UMKIM_THIN_ZONE=example.com \
//   UMKIM_THIN_CONSOLE_HOST=console.example.com \
//   UMKIM_THIN_AUTH_HOST=auth.example.com \
//   UMKIM_THIN_USER=admin UMKIM_THIN_PASSWORD=... \
//   npx playwright test console.spec.ts
//
// The console login itself is public (/login); the rest of the console is
// behind Authelia, so the browser visits the portal first.

const ZONE = process.env.UMKIM_THIN_ZONE || '';
const CONSOLE_HOST = process.env.UMKIM_THIN_CONSOLE_HOST || (ZONE ? `console.${ZONE}` : '');
const AUTH_HOST = process.env.UMKIM_THIN_AUTH_HOST || (ZONE ? `auth.${ZONE}` : '');
const USER = process.env.UMKIM_THIN_USER || 'admin';
const PASS = process.env.UMKIM_THIN_PASSWORD || '';
// The ordinary tenant is the realistic Authelia user; the console's own
// breakglass/admin login is separate.
const PORTAL_USER = process.env.UMKIM_THIN_PORTAL_USER || '';
const PORTAL_PASS = process.env.UMKIM_THIN_PORTAL_PASSWORD || '';

async function portalLogin(page: Page) {
  await page.goto(`https://${CONSOLE_HOST}/`);
  if (!page.url().includes(AUTH_HOST)) return;

  const user = page.locator('#username-textfield, input[autocomplete="username"]').first();
  await user.waitFor({ state: 'visible', timeout: 30_000 });
  await user.click();
  await user.fill('');
  await user.pressSequentially(PORTAL_USER, { delay: 20 });

  const pass = page.locator('#password-textfield, input[autocomplete="current-password"]').first();
  await pass.click();
  await pass.fill('');
  await pass.pressSequentially(PORTAL_PASS, { delay: 20 });
  await Promise.all([
    page.waitForURL((u) => !u.host.startsWith('auth.'), { timeout: 30_000 }),
    pass.press('Enter'),
  ]);
}

test.describe('console', () => {
  test.skip(
    !CONSOLE_HOST || !PASS || !PORTAL_USER || !PORTAL_PASS,
    'set UMKIM_THIN_CONSOLE_HOST, UMKIM_THIN_PASSWORD, UMKIM_THIN_PORTAL_USER and UMKIM_THIN_PORTAL_PASSWORD',
  );

  test('a client can sign in and use the Applications page', async ({ browser, request }) => {
    test.setTimeout(120_000);
    const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
    const page = await ctx.newPage();

    // The login page itself is public and must carry the console security
    // headers. Capture those before authenticating.
    const loginPage = await page.goto(`https://${CONSOLE_HOST}/login`);
    expect(loginPage?.status(), 'console login page').toBe(200);
    const loginHeaders = loginPage?.headers() ?? {};
    expect(loginHeaders['content-security-policy'], 'console CSP').toBeTruthy();

    // Authelia protects the console root; the console then has its own login.
    await portalLogin(page);
    if (!new URL(page.url()).pathname.startsWith('/login')) {
      await page.goto(`https://${CONSOLE_HOST}/login`);
    }

    await expect(page.locator('#username')).toBeVisible({ timeout: 15_000 });
    await page.fill('#username', USER);
    await page.fill('#password', PASS);
    await Promise.all([
      page.waitForURL(
        (u) => u.host === CONSOLE_HOST && !u.pathname.startsWith('/login'),
        { timeout: 30_000 },
      ),
      page.press('#password', 'Enter'),
    ]);

    await expect(page.getByRole('heading', { name: 'Applications' })).toBeVisible({
      timeout: 15_000,
    });

    // The Catalog table must show both the application licence and the chart
    // licence. SparkyFitness is the deliberate non-commercial entry, so it is
    // the useful canary for the warning text as well.
    await page.locator('#tab-catalog-label').click();
    await expect(page.getByRole('columnheader', { name: 'License' }).first()).toBeVisible();
    await expect(page.locator('#catalog')).toContainText('Source-available (non-commercial)');
    await expect(page.locator('#catalog')).toContainText('chart Apache-2.0');
    await expect(page.locator('#catalog')).toContainText('website');
    await expect(page.locator('#catalog')).toContainText('GitHub');

    const result = await page.evaluate(async () => {
      const r = await fetch('/api/status', { credentials: 'same-origin' });
      return { status: r.status, body: await r.json() };
    });
    expect(result.status, 'console /api/status').toBe(200);
    expect(Array.isArray(result.body), 'status is an array').toBeTruthy();
    expect(result.body.length, 'at least five installed apps').toBeGreaterThanOrEqual(5);

    // The authenticated page must carry the edge/console security headers.
    const root = await page.goto(`https://${CONSOLE_HOST}/`);
    expect(root?.status(), 'console authenticated root').toBe(200);
    const rootHeaders = root?.headers() ?? {};
    expect(rootHeaders['content-security-policy'], 'console CSP').toBeTruthy();
    expect(rootHeaders['strict-transport-security'], 'console HSTS').toBeTruthy();

    // The console API must not be reachable without its session. Use a fresh
    // request context so the browser cookies are not attached.
    const anon = await request.get(`https://${CONSOLE_HOST}/api/status`, { maxRedirects: 0 });
    expect(anon.status(), 'anonymous /api/status').not.toBe(200);

    // A running install must appear in Applications before its Helm release
    // exists, with the current progress and an "installing" chip.
    await page.route('**/api/status', route =>
      route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }));
    await page.route(/\/api\/jobs$/, route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify([{
          id: 'install-test', kind: 'install', name: 'paperless', status: 'running',
          log: ['pulling chart'], started: new Date().toISOString(),
        }]),
      }));
    await page.evaluate(() => loadApplications());
    await expect(page.locator('#applications')).toContainText('paperless');
    await expect(page.locator('#applications')).toContainText('installing');
    await expect(page.locator('#applications')).toContainText('pulling chart');

    // An app install can roll Authelia, which briefly makes the console's
    // forward-auth return an empty 5xx. The job keeps running server-side, so
    // the UI's poller must retry rather than fail with "Unexpected end of
    // JSON input".
    let pollCalls = 0;
    await page.route('**/api/jobs/transient-test', async route => {
      pollCalls++;
      if (pollCalls === 1) {
        return route.fulfill({ status: 500, body: '' });
      }
      if (pollCalls === 2) {
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ id: 'transient-test', name: 'paperless', status: 'running', log: [] }),
        });
      }
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ id: 'transient-test', name: 'paperless', status: 'done', log: [], result: { Name: 'paperless' } }),
      });
    });
    const polled = await page
      .evaluate(() => pollJob('transient-test', 'installing').then(j => j.status).catch(e => 'error: ' + String(e)));
    expect(polled, 'the job poller tolerates a transient 5xx').toBe('done');
    expect(pollCalls, 'the poller retried').toBeGreaterThanOrEqual(3);

    await ctx.close();
  });
});
