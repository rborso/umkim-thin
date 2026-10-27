import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, anon } from '../lib/session';

// SparkyFitness is OIDC-only for directory users; the route is also behind
// Authelia. The real browser login and the mealie recipe provider are covered
// by auth.spec.ts and the chart's bootstrap Job.
test.describe('sparkyfitness', () => {
  test.skip(!installed('sparkyfitness'), 'sparkyfitness is not installed');

  const host = hostOf('sparkyfitness');
  const url = (path = '/') => `https://${host}${path}`;

  test('route is challenged by the portal', async ({ request }) => {
    const res = await request.get(url(), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    expect([301, 302, 303, 307, 308, 401, 403]).toContain(res.status());
  });

  test('authenticated request can reach the app and its API', async () => {
    const ctx = await portalSession();
    const page = await ctx.get(url('/'));
    expect(page.status(), 'the app shell').toBeLessThan(500);
    const health = await ctx.get(url('/api/health'));
    expect(health.status(), 'the server health endpoint').toBe(200);
    await ctx.dispose();
  });

  test('the OIDC provider is registered', async () => {
    const ctx = await portalSession();
    const res = await ctx.get(url('/api/auth/settings'));
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body?.oidc?.enabled, 'OIDC is enabled').toBeTruthy();
    expect(
      (body?.oidc?.providers ?? []).map((p: { id: string }) => p.id),
      'the authelia provider is registered',
    ).toContain('authelia');
    await ctx.dispose();
  });

  test('anonymous API request is not a direct app response', async () => {
    const ctx = await anon();
    const res = await ctx.get(url('/api/health'), { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    await ctx.dispose();
  });
});
