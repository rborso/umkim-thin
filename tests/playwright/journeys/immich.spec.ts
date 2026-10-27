import { test, expect, request } from '@playwright/test';
import { installed, hostOf, portalSession, appAdmin, png, waitFor } from '../lib/session';

// Client simulation for Immich: what the web UI, the mobile app and an
// automation script actually do.
//
//   * SSO sign-in through the real browser flow (Immich -> Authelia -> back).
//     This is the chain that breaks when the app cannot trust the issuer's TLS
//     certificate, so it is worth a browser and not just an API call.
//   * upload several photos, see the thumbnailer process them, and download the
//     originals back byte for byte.
//   * an album containing them, and a public link that an anonymous client can
//     open through the route's exempt /share/ path.
//   * an API key, the way an automation client authenticates.

const { user: ADMIN_EMAIL, pass: ADMIN_PASS } = appAdmin('immich');

test.describe('immich client', () => {
  test.skip(!installed('immich'), 'immich is not installed');
  test.skip(!ADMIN_PASS, 'UMKIM_THIN_IMMICH_ADMIN_PASSWORD not set');

  const host = hostOf('immich');
  const base = () => `https://${host}`;

  // api() is an Authelia session carrying Immich's own bearer token, which is
  // what the web UI does.
  async function api() {
    const ctx = await portalSession();
    const login = await ctx.post(`${base()}/api/auth/login`, {
      data: { email: ADMIN_EMAIL, password: ADMIN_PASS },
    });
    expect([200, 201], `immich login for ${ADMIN_EMAIL}`).toContain(login.status());
    const { accessToken } = await login.json();
    expect(accessToken, 'access token').toBeTruthy();
    return { ctx, headers: { Authorization: `Bearer ${accessToken}` } };
  }

  async function upload(ctx: any, headers: any, name: string, buffer: Buffer) {
    const now = new Date().toISOString();
    const r = await ctx.post(`${base()}/api/assets`, {
      headers,
      multipart: {
        assetData: { name, mimeType: 'image/png', buffer },
        deviceAssetId: `ci-${name}-${Date.now()}`,
        deviceId: 'ci-playwright',
        fileCreatedAt: now,
        fileModifiedAt: now,
      },
    });
    expect([200, 201], `upload ${name} -> ${r.status()}`).toContain(r.status());
    const body = await r.json();
    expect(body.id, `asset id for ${name}`).toBeTruthy();
    return body.id as string;
  }

  test('uploads photos, the thumbnailer processes them, originals download back', async () => {
    const { ctx, headers } = await api();
    const photo = png(64, 64);
    const second = png(48, 32);
    const ids = [await upload(ctx, headers, 'ci-a.png', photo), await upload(ctx, headers, 'ci-b.png', second)];

    // the thumbnailer is asynchronous: the first request can 404 while the job
    // is queued, which is exactly what the app's own UI polls for
    await waitFor(
      async () => {
        const r = await ctx.get(`${base()}/api/assets/${ids[0]}/thumbnail?size=thumbnail`, { headers });
        return r.status() === 200;
      },
      { what: 'the thumbnail job', timeoutMs: 90_000 },
    );

    for (const [i, original] of [photo, second].entries()) {
      const dl = await ctx.get(`${base()}/api/assets/${ids[i]}/original`, { headers });
      expect(dl.status(), 'download original').toBe(200);
      expect(Buffer.from(await dl.body()).equals(original), `original ${i} matches`).toBeTruthy();
    }

    await ctx.delete(`${base()}/api/assets`, { headers, data: { ids, force: true } });
    await ctx.dispose();
  });

  test('an album holds the photos and a public link opens without a session', async () => {
    const { ctx, headers } = await api();
    const id = await upload(ctx, headers, 'ci-album.png', png(32, 32));

    const created = await ctx.post(`${base()}/api/albums`, {
      headers,
      data: { albumName: `ci-album-${Date.now()}` },
    });
    expect(created.status()).toBe(201);
    const album = await created.json();

    const added = await ctx.put(`${base()}/api/albums/${album.id}/assets`, { headers, data: { ids: [id] } });
    expect(added.status()).toBe(200);

    const read = await ctx.get(`${base()}/api/albums/${album.id}`, { headers });
    expect(read.status()).toBe(200);
    expect((await read.json()).assetCount, 'the album holds the photo').toBe(1);

    // share it publicly; Immich mints a shared link with its own key
    const shared = await ctx.post(`${base()}/api/shared-links`, {
      headers,
      data: { type: 'ALBUM', albumId: album.id },
    });
    expect([200, 201], `share album -> ${shared.status()}`).toContain(shared.status());
    const linkKey: string = (await shared.json()).key;
    expect(linkKey, 'shared link key').toBeTruthy();

    const anon = await request.newContext({ ignoreHTTPSErrors: true });
    const link = `${base()}/share/${linkKey}`;
    const page = await anon.get(link);
    // the /share/ path is exempt from the forward-auth, so the app answers
    expect([200, 401], `public album link -> ${page.status()}`).toContain(page.status());
    expect(page.headers()['location'] ?? '', 'must not be pushed to the portal').not.toContain('auth.');

    // The share page is a Svelte app: its immutable assets and the share API
    // must be reachable without a portal session too, or the page only spins.
    const html = await page.text();
    const assetPath = html.match(/(?:src|href)="(\/_app\/[^"]+\.js)"/)?.[1];
    expect(assetPath, 'share page has an _app asset').toBeTruthy();
    const asset = await anon.get(`${base()}${assetPath}`);
    expect(asset.status(), `public share asset ${assetPath}`).toBe(200);
    expect(asset.headers()['content-type'] ?? '', 'asset is JavaScript').toContain('javascript');

    const sharedLink = await anon.get(`${base()}/api/shared-links/me?key=${encodeURIComponent(linkKey)}`);
    expect(sharedLink.status(), 'public shared-link API').toBe(200);
    expect(sharedLink.headers()['content-type'] ?? '').toContain('application/json');

    const buckets = await anon.get(`${base()}/api/timeline/buckets?albumId=${album.id}&key=${encodeURIComponent(linkKey)}`);
    expect(buckets.status(), 'public timeline buckets').toBe(200);
    expect(buckets.headers()['content-type'] ?? '').toContain('application/json');

    for (const asset of ['/api/server/config', '/api/public/config']) {
      const r = await anon.get(`${base()}${asset}`, { maxRedirects: 0 });
      expect(r.status(), `public Immich asset ${asset}`).toBe(200);
      expect(r.headers()['content-type'] ?? '').toContain('application/json');
    }

    // A future change must not turn a private API into a public one.
    const privateApi = await anon.get(`${base()}/api/server/storage`, { maxRedirects: 0 });
    expect([301, 302, 303, 307, 308], 'Immich private API remains behind the portal').toContain(privateApi.status());
    expect(privateApi.headers()['location'] ?? '').toContain('auth.');
    await anon.dispose();

    await ctx.delete(`${base()}/api/assets`, { headers, data: { ids: [id], force: true } });
    await ctx.delete(`${base()}/api/albums/${album.id}`, { headers });
    await ctx.dispose();
  });

  test('an API key authenticates an automation client', async () => {
    const { ctx, headers } = await api();
    const created = await ctx.post(`${base()}/api/api-keys`, {
      headers,
      data: { name: `ci-key-${Date.now()}`, permissions: ['all'] },
    });
    expect(created.status(), `api key create -> ${created.status()}`).toBe(201);
    const key = await created.json();
    expect(key.secret, 'api key secret').toBeTruthy();

    const me = await ctx.get(`${base()}/api/users/me`, { headers: { 'x-api-key': key.secret } });
    expect(me.status(), 'the key authenticates').toBe(200);
    expect((await me.json()).email).toBe(ADMIN_EMAIL);

    // and it can write
    const id = await upload(ctx, { 'x-api-key': key.secret }, 'ci-key.png', png(16, 16));
    await ctx.delete(`${base()}/api/assets`, { headers, data: { ids: [id], force: true } });
    await ctx.delete(`${base()}/api/api-keys/${key.id}`, { headers });
    await ctx.dispose();
  });
});
