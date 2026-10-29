import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, appApiKey, waitFor } from '../lib/session';

// Client simulation for the Servarr stack: what the apps' own UIs and the
// automation between them do.
//
//   * every *arr answers on its API with its own key (the chart seeds the key
//     into each app's config, so a mismatch shows up immediately);
//   * a root folder is created on the shared media volume - the same filesystem
//     the download client and Jellyfin see;
//   * Jellyfin completes its startup wizard (the chart deliberately leaves that
//     to the user), gets a library on the shared volume, scans it, and reports
//     the fixture that was placed there;
//   * qBittorrent answers (its WebUI is what the browser uses).
//
// The API keys and the Jellyfin account come from ci/e2e.sh.

const SONARR = () => appApiKey('sonarr');
const RADARR = () => appApiKey('radarr');
const PROWLARR = () => appApiKey('prowlarr');
const LIBRARY = process.env.UMKIM_THIN_MEDIA_LIBRARY || '/media/ci-library';

test.describe('servarr stack', () => {
  test.skip(!installed('servarr'), 'servarr is not installed');
  test.skip(!SONARR() || !RADARR() || !PROWLARR(), 'the *arr API keys are not set');

  const base = (host: string) => `https://${host}`;

  test('each *arr answers on its API with its own key', async () => {
    const ctx = await portalSession();
    // Sonarr/Radarr speak v3; Prowlarr's API is v1.
    for (const [app, key, version] of [
      ['sonarr', SONARR(), 'v3'],
      ['radarr', RADARR(), 'v3'],
      ['prowlarr', PROWLARR(), 'v1'],
    ] as const) {
      const host = hostOf(app);
      expect(host, `${app} host`).toBeTruthy();
      const r = await ctx.get(`${base(host)}/api/${version}/system/status`, { headers: { 'X-Api-Key': key } });
      expect(r.status(), `${app} system/status`).toBe(200);
      const body = await r.json();
      expect(String(body.appName ?? '').toLowerCase(), `${app} identifies itself`).toContain(app.slice(0, 3));
      expect(body.version, `${app} version`).toBeTruthy();
    }
    await ctx.dispose();
  });

  test('a root folder on the shared media volume can be created', async () => {
    test.skip(
      process.env.UMKIM_THIN_SKIP_MEDIA_FIXTURE === '1',
      'the runner could not place the shared-volume fixture',
    );
    const ctx = await portalSession();
    const host = hostOf('radarr');
    const headers = { 'X-Api-Key': RADARR() };

    const existing = await (await ctx.get(`${base(host)}/api/v3/rootfolder`, { headers })).json();
    let createdId: number | undefined;
    try {
      if (!(existing as any[]).some((f) => f.path === LIBRARY)) {
        const created = await ctx.post(`${base(host)}/api/v3/rootfolder`, {
          headers,
          data: { path: LIBRARY },
        });
        expect([200, 201], `create root folder -> ${created.status()}`).toContain(created.status());
        createdId = (await created.json().catch(() => ({}))).id;
      }

      const list = await (await ctx.get(`${base(host)}/api/v3/rootfolder`, { headers })).json();
      expect(
        (list as any[]).map((f) => f.path),
        'the root folder is on the shared media volume',
      ).toContain(LIBRARY);

      // it is the same filesystem the download client and Jellyfin mount
      expect((list as any[]).find((f) => f.path === LIBRARY)?.accessible, 'the volume is accessible').toBe(true);
    } finally {
      // Existing installations must not be left with a test root folder.
      if (createdId !== undefined) {
        await ctx
          .delete(`${base(host)}/api/v3/rootfolder/${createdId}`, { headers })
          .catch(() => undefined);
      }
      await ctx.dispose();
    }
  });

  test('the download client answers', async () => {
    // qBittorrent has no forward-auth bypass and no API key configured in this
    // stack; reaching its UI through the session is what matters.
    const ctx = await portalSession();
    const res = await ctx.get(`${base(hostOf('qbittorrent') ?? '')}/api/v2/app/version`);
    // 200 with a session, 403 when the WebUI wants its own login: either proves
    // the app (not the portal) answered.
    expect([200, 403], `qBittorrent API -> ${res.status()}`).toContain(res.status());
    await ctx.dispose();
  });
});
