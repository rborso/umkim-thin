import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, anon } from '../lib/session';

// Mealie is OIDC-only for directory users; the route is also behind Authelia.
// These checks stay small because the app-level OIDC flow is exercised by the
// shared acceptance gate when Mealie is included in ci_apps.
test.describe('mealie', () => {
  test.skip(!installed('mealie'), 'mealie is not installed');

  const host = hostOf('mealie');
  const url = (path = '/') => `https://${host}${path}`;

  test('route is challenged by the portal', async ({ request }) => {
    const res = await request.get(url(), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    expect([301, 302, 303, 307, 308, 401, 403]).toContain(res.status());
  });

  test('authenticated request can reach the app', async () => {
    const ctx = await portalSession();
    const res = await ctx.get(url('/api/app/about/startup-info'));
    expect(res.status()).toBeLessThan(500);
    await ctx.dispose();
  });

  test('anonymous API request is not a direct app response', async () => {
    const ctx = await anon();
    const res = await ctx.get(url('/api/app/about/startup-info'), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    await ctx.dispose();
  });
});
