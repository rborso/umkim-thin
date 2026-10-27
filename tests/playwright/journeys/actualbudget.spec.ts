import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, anon } from '../lib/session';

// Actual Budget's own login is OIDC; the route also sits behind Authelia by
// default. The real browser flow is in auth.spec.ts.
test.describe('actualbudget', () => {
  test.skip(!installed('actualbudget'), 'actualbudget is not installed');

  const host = hostOf('actualbudget');
  const url = (path = '/') => `https://${host}${path}`;

  test('route is challenged by the portal', async ({ request }) => {
    const res = await request.get(url(), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    expect([301, 302, 303, 307, 308, 401, 403]).toContain(res.status());
  });

  test('authenticated request reaches the sync server', async () => {
    const ctx = await portalSession();
    const page = await ctx.get(url('/'));
    expect(page.status(), 'the app shell').toBeLessThan(500);
    const health = await ctx.get(url('/health'));
    expect(health.status(), 'the sync server health endpoint').toBe(200);
    await ctx.dispose();
  });

  test('the OIDC login entry point is present', async () => {
    const ctx = await portalSession();
    const page = await ctx.get(url('/'));
    expect(page.status()).toBe(200);
    expect(await page.text()).toMatch(/Actual|OpenID|login/i);
    await ctx.dispose();
  });

  test('anonymous health request is not a direct app response', async () => {
    const ctx = await anon();
    const res = await ctx.get(url('/health'), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    await ctx.dispose();
  });
});
