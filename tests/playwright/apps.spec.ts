import { test, expect, Page } from '@playwright/test';

// Data-driven verification for the catalog's checks. The app list is provided
// by CI (see ci/apps.json + ci/e2e.sh):
//
//   UMKIM_THIN_APPS   JSON array of {name, host, auth, public?[]}
//   UMKIM_THIN_USER / UMKIM_THIN_PASSWORD   an Authelia account
//   UMKIM_THIN_AUTH_HOST   the portal host (e.g. auth.test.example.com)

type App = { name: string; host: string; auth: string; public?: string[] };

const apps: App[] = JSON.parse(process.env.UMKIM_THIN_APPS || '[]');
// The portal/app login checks use a regular tenant, not the directory admin;
// the admin remains for the console/invite APIs. Fall back to UMKIM_THIN_USER
// for a manual run that only sets the admin.
const USER = process.env.UMKIM_THIN_PORTAL_USER || process.env.UMKIM_THIN_USER || '';
const PASS = process.env.UMKIM_THIN_PORTAL_PASSWORD || process.env.UMKIM_THIN_PASSWORD || '';
const AUTH_HOST = process.env.UMKIM_THIN_AUTH_HOST || '';

const url = (host: string, path = '/') => `https://${host}${path}`;
const isAuthRedirect = (status: number, location: string) =>
  (status >= 300 && status < 400 && location.includes(AUTH_HOST)) || status === 401;

async function login(page: Page, host: string) {
  await page.goto(url(host));
  // The Authelia portal renders a SPA; MUI inputs need real keystrokes for
  // React to register the values.
  const user = page.locator('#username-textfield, input[autocomplete="username"]').first();
  await user.waitFor({ state: 'visible', timeout: 30_000 });
  await user.click();
  await user.fill('');
  await user.pressSequentially(USER, { delay: 20 });
  const pass = page
    .locator('#password-textfield, input[autocomplete="current-password"]')
    .first();
  await pass.click();
  await pass.fill('');
  await pass.pressSequentially(PASS, { delay: 20 });
  await Promise.all([
    page.waitForURL((u) => !u.host.startsWith('auth.'), { timeout: 30_000 }),
    pass.press('Enter'),
  ]);
}

for (const app of apps) {
  test.describe(`${app.name} (${app.auth})`, () => {
    test('route_reachable + auth_challenge', async ({ request }) => {
      const res = await request.get(url(app.host), { maxRedirects: 0 });
      expect(res.status(), `reachability of ${app.host}`).toBeLessThan(500);
      expect(res.status(), `anonymous request to ${app.host} should be challenged`)
        .not.toBe(200);
      expect(
        isAuthRedirect(res.status(), res.headers()['location'] ?? ''),
        `expected a challenge to ${AUTH_HOST}, got ${res.status()} ${res.headers()['location'] ?? ''}`,
      ).toBe(true);
    });

    for (const p of app.public ?? []) {
      test(`public_path_exempt ${p}`, async ({ request }) => {
        const res = await request.get(url(app.host, p), { maxRedirects: 0 });
        // The path may 404 (no such share), but must NOT be pushed to the portal.
        expect(
          isAuthRedirect(res.status(), res.headers()['location'] ?? ''),
          `${p} on ${app.host} must be reachable without a session`,
        ).toBe(false);
        expect(res.status()).toBeLessThan(500);
      });
    }

    test('authenticated_request_reaches_app', async ({ browser }) => {
      test.skip(!USER || !PASS, 'set UMKIM_THIN_USER / UMKIM_THIN_PASSWORD');
      const ctx = await browser.newContext();
      const page = await ctx.newPage();
      await login(page, app.host);
      const res = await page.goto(url(app.host));
      expect(res?.status(), `after login, ${app.host}`).toBeLessThan(400);
      await ctx.close();
    });
  });
}
