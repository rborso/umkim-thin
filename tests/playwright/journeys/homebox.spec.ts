import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, anon } from '../lib/session';

// HomeBox's own login is OIDC; the route also sits behind Authelia.
test.describe('homebox', () => {
  test.skip(!installed('homebox'), 'homebox is not installed');

  const host = hostOf('homebox');
  const url = (path = '/') => `https://${host}${path}`;

  test('route is challenged by the portal', async ({ request }) => {
    const res = await request.get(url(), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    expect([301, 302, 303, 307, 308, 401, 403]).toContain(res.status());
  });

  test('authenticated request reaches the inventory', async () => {
    const ctx = await portalSession();
    const page = await ctx.get(url('/'));
    expect(page.status(), 'the app shell').toBeLessThan(500);
    const status = await ctx.get(url('/api/v1/status'));
    expect(status.status(), 'HomeBox status endpoint').toBe(200);
    expect((await status.json()).health, 'HomeBox health').toBe(true);
    await ctx.dispose();
  });

  test('anonymous inventory API is not publicly writable', async () => {
    const ctx = await anon();
    const res = await ctx.get(url('/api/v1/status'), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    await ctx.dispose();
  });
});
