import { test, expect } from '@playwright/test';
import { ZONE, PORTAL_USER, PORTAL_PASS, installed, portalSession } from './lib/session';

// The home page is a read-only, user-facing view of installed catalog apps.
// It is deliberately separate from the admin console: Authelia is the only
// gate and the page never exposes namespaces, Helm phases or mutating actions.
const HOME_HOST = process.env.UMKIM_THIN_HOME_HOST || (ZONE ? `home.${ZONE}` : '');

type HomeApp = {
  name: string;
  route?: string;
  displayName?: string;
  description?: string;
  url?: string;
  status?: string;
};

test.describe('home', () => {
  test.skip(!HOME_HOST, 'UMKIM_THIN_HOME_HOST is not set');

  test('route is challenged by the portal', async ({ request }) => {
    const res = await request.get(`https://${HOME_HOST}/`, { maxRedirects: 0 });
    expect(res.status()).not.toBe(200);
    expect([301, 302, 303, 307, 308, 401, 403]).toContain(res.status());
  });

  test('an authenticated directory user sees the installed app list', async () => {
    const ctx = await portalSession(
      PORTAL_USER && PORTAL_PASS ? { username: PORTAL_USER, password: PORTAL_PASS } : undefined,
    );
    const page = await ctx.get(`https://${HOME_HOST}/`);
    expect(page.status(), 'home page').toBe(200);
    expect(await page.text(), 'home page title').toContain('Home');

    const api = await ctx.get(`https://${HOME_HOST}/api/home`);
    expect(api.status(), 'home API').toBe(200);
    const body = await api.json();
    expect(Array.isArray(body.apps), 'apps is an array').toBeTruthy();
    expect(body.user, 'the forwarded directory user is present').toBeTruthy();
    for (const app of body.apps) {
      expect(app.name, 'app name').toBeTruthy();
      expect(app.url, `url for ${app.name}`).toMatch(/^https:\/\//);
      expect(['available', 'starting', 'unavailable']).toContain(app.status);
      expect(app).not.toHaveProperty('namespace');
      expect(app).not.toHaveProperty('components');
    }
    if (installed('servarr')) {
      const servarr = (body.apps as HomeApp[]).filter((app) => app.name === 'servarr');
      expect(servarr, 'Servarr is split into watch and request cards').toHaveLength(2);
      const watch = servarr.find((app) => app.route === 'jellyfin');
      const requestApp = servarr.find((app) => app.route === 'seerr');
      expect(watch?.displayName, 'Jellyfin card').toBe('Jellyfin');
      expect(requestApp?.displayName, 'Seerr card').toBe('Seerr');
      expect(watch?.url, 'Jellyfin URL').toContain('jellyfin.');
      expect(requestApp?.url, 'Seerr URL').toContain('servarr.');
      expect(watch?.description, 'Jellyfin description').toContain('Watch');
      expect(requestApp?.description, 'Seerr description').toContain('Request');
    }
    await ctx.dispose();
  });

  test('the page follows the browser language', async ({ browser }) => {
    test.skip(!installed('servarr'), 'servarr is not installed');
    const ctx = await portalSession(
      PORTAL_USER && PORTAL_PASS ? { username: PORTAL_USER, password: PORTAL_PASS } : undefined,
    );
    const state = await ctx.storageState();
    await ctx.dispose();
    const bctx = await browser.newContext({
      locale: 'hu-HU',
      storageState: state,
      ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
    });
    const page = await bctx.newPage();
    await page.goto(`https://${HOME_HOST}/`);
    await expect(page.getByRole('heading', { name: 'Otthon' })).toBeVisible();
    await expect(page.getByText('Filmek és sorozatok nézése.')).toBeVisible();
    await expect(page.getByText('Filmek és sorozatok kérése.')).toBeVisible();
    await expect(page.getByText('Megnyitás →').first()).toBeVisible();
    await bctx.close();
  });
});
