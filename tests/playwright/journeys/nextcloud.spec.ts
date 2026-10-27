import { test, expect, request } from '@playwright/test';
import { installed, hostOf, anon, basic, appAdmin, waitFor } from '../lib/session';

// Client simulation for Nextcloud, going past "a file exists": the desktop
// client's sync semantics and the browser's sharing/editing paths.
//
//   * status.php and the client capabilities (the sync client reads these first)
//   * a chunked upload of a file larger than the chunk size, assembled by the
//     server (what the desktop client does for anything non-trivial)
//   * ETag/If-Match conflict handling (a real client retries on 412)
//   * versions and the trashbin (delete + restore)
//   * a public link with a password, fetched by a client with no session
//   * CalDAV: create a calendar and an event, read it back
//   * Collabora: the WOPI discovery document the editor depends on
//
// The client endpoints are exempt from the forward-auth, so - like the real
// desktop client - these calls carry Basic auth and no browser session.

const { user: NC_ADMIN, pass: NC_PASS } = appAdmin('nextcloud');
const CHUNK = 5 * 1024 * 1024; // 5 MiB, Nextcloud's chunking granularity

test.describe('nextcloud client', () => {
  test.skip(!installed('nextcloud'), 'nextcloud is not installed');
  test.skip(!NC_PASS, 'UMKIM_THIN_NEXTCLOUD_ADMIN_PASSWORD not set');

  const host = hostOf('nextcloud');
  const base = () => `https://${host}`;
  const auth = () => ({ Authorization: basic(NC_ADMIN, NC_PASS) });
  const ocs = (p: string) => `${base()}/ocs/v2.php/apps/files_sharing/api/v1/shares${p}`;
  const dav = (p: string) => `${base()}/remote.php/dav/files/${NC_ADMIN}${p}`;
  const headers = () => ({ ...auth(), 'OCS-APIRequest': 'true' });

  test('a client can check the server status and capabilities', async ({ request: r }) => {
    const status = await r.get(`${base()}/status.php`);
    expect(status.status(), 'status.php (used by desktop/mobile clients)').toBe(200);
    expect((await status.json()).installed).toBe(true);

    const caps = await r.get(`${base()}/ocs/v1.php/cloud/capabilities?format=json`, { headers: headers() });
    expect(caps.status()).toBe(200);
    const data = (await caps.json())?.ocs?.data?.capabilities ?? {};
    expect(data.files?.bigfilechunking, 'the server advertises chunked upload').toBeTruthy();
    expect(data.dav?.chunking, 'the DAV chunking version').toBeTruthy();
  });

  test('a client uploads a large file in chunks and reads it back', async () => {
    const ctx = await anon();
    await ctx.fetch(dav('/ci'), { method: 'MKCOL', headers: auth() });

    // A file that needs several chunks, with distinguishable content per chunk
    // so a mis-assembly is visible in the comparison.
    const total = 3 * CHUNK + 12345;
    const body = Buffer.alloc(total);
    for (let i = 0; i < total; i++) body[i] = (i * 7 + (i >> 16)) & 0xff;

    const uploadId = `ci-${Date.now()}`;
    const chunkDir = `/remote.php/dav/uploads/${NC_ADMIN}/${uploadId}`;
    // the desktop client creates the upload collection first
    const mk = await ctx.fetch(`${base()}${chunkDir}`, { method: 'MKCOL', headers: auth() });
    expect([201, 405], `MKCOL upload collection -> ${mk.status()}`).toContain(mk.status());
    for (let offset = 0, n = 0; offset < total; offset += CHUNK, n++) {
      const slice = body.subarray(offset, Math.min(offset + CHUNK, total));
      const r = await ctx.fetch(`${base()}${chunkDir}/${n}`, {
        method: 'PUT',
        headers: { ...auth(), 'Content-Type': 'application/octet-stream' },
        data: slice,
      });
      expect([201, 204], `chunk ${n} -> ${r.status()}`).toContain(r.status());
    }

    // MOVE assembles the chunks into the destination in one step
    const moved = await ctx.fetch(`${base()}${chunkDir}/.file`, {
      method: 'MOVE',
      headers: {
        ...auth(),
        Destination: `${base()}/remote.php/dav/files/${NC_ADMIN}/ci/large.bin`,
        'OC-Total-Length': String(total),
      },
    });
    expect([201, 204], `assemble -> ${moved.status()}`).toContain(moved.status());

    const back = await ctx.fetch(dav('/ci/large.bin'), { headers: auth() });
    expect(back.status(), 'read the assembled file').toBe(200);
    const got = Buffer.from(await back.body());
    expect(got.length, 'assembled length').toBe(total);
    expect(got.equals(body), 'assembled bytes match').toBeTruthy();

    await ctx.fetch(dav('/ci/large.bin'), { method: 'DELETE', headers: auth() });
    await ctx.dispose();
  });

  test('a client resolves an edit conflict through If-Match', async () => {
    const ctx = await anon();
    await ctx.fetch(dav('/ci'), { method: 'MKCOL', headers: auth() });

    const first = await ctx.fetch(dav('/ci/conflict.txt'), {
      method: 'PUT',
      headers: { ...auth(), 'Content-Type': 'text/plain' },
      data: 'v1',
    });
    expect([201, 204]).toContain(first.status());
    const etag = first.headers()['etag'];
    expect(etag, 'the server returns an ETag').toBeTruthy();

    // a stale client is rejected instead of silently overwriting
    const stale = await ctx.fetch(dav('/ci/conflict.txt'), {
      method: 'PUT',
      headers: { ...auth(), 'Content-Type': 'text/plain', 'If-Match': '"stale-etag"' },
      data: 'v2',
    });
    expect(stale.status(), 'a stale If-Match must be refused').toBe(412);

    // the client re-reads and retries with the current ETag
    const retry = await ctx.fetch(dav('/ci/conflict.txt'), {
      method: 'PUT',
      headers: { ...auth(), 'Content-Type': 'text/plain', 'If-Match': etag },
      data: 'v2',
    });
    expect([201, 204], 'the retry with the current ETag succeeds').toContain(retry.status());
    const back = await ctx.fetch(dav('/ci/conflict.txt'), { headers: auth() });
    expect(await back.text()).toBe('v2');

    await ctx.fetch(dav('/ci/conflict.txt'), { method: 'DELETE', headers: auth() });
    await ctx.dispose();
  });

  test('a deleted file goes to the trashbin and can be restored', async () => {
    const ctx = await anon();
    await ctx.fetch(dav('/ci'), { method: 'MKCOL', headers: auth() });
    await ctx.fetch(dav('/ci/restore-me.txt'), {
      method: 'PUT',
      headers: { ...auth(), 'Content-Type': 'text/plain' },
      data: 'restore me',
    });

    const del = await ctx.fetch(dav('/ci/restore-me.txt'), { method: 'DELETE', headers: auth() });
    expect(del.status()).toBe(204);

    // the trashbin lists it (a human opens the "Deleted files" view)
    const trash = await ctx.fetch(`${base()}/remote.php/dav/trashbin/${NC_ADMIN}/trash`, {
      method: 'PROPFIND',
      headers: { ...auth(), Depth: '1' },
    });
    expect(trash.status()).toBe(207);
    const listing = await trash.text();
    expect(listing, 'the deleted file is in the trash').toContain('restore-me.txt');

    // restore it, then find it back in its folder
    const entry = (listing.match(/<d:href>([^<]*restore-me\.txt[^<]*)<\/d:href>/) ?? [])[1];
    expect(entry, 'a trash entry for the file').toBeTruthy();
    const restored = await ctx.fetch(`${base()}${entry}`, {
      method: 'MOVE',
      headers: {
        ...auth(),
        Destination: `${base()}/remote.php/dav/trashbin/${NC_ADMIN}/restore/restore-me.txt`,
      },
    });
    expect([201, 204], `restore -> ${restored.status()}`).toContain(restored.status());
    const back = await ctx.fetch(dav('/ci/restore-me.txt'), { headers: auth() });
    expect(back.status(), 'the file is back').toBe(200);
    expect(await back.text()).toBe('restore me');

    await ctx.fetch(dav('/ci/restore-me.txt'), { method: 'DELETE', headers: auth() });
    await ctx.dispose();
  });

  test('a public link with a password works for a client with no session', async () => {
    const ctx = await anon();
    await ctx.fetch(dav('/ci'), { method: 'MKCOL', headers: auth() });
    const content = `public ${Date.now()}`;
    await ctx.fetch(dav('/ci/public.txt'), {
      method: 'PUT',
      headers: { ...auth(), 'Content-Type': 'text/plain' },
      data: content,
    });

    const password = 'Ci-Pass-123';
    const created = await ctx.post(ocs('?format=json'), {
      headers: headers(),
      form: { path: '/ci/public.txt', shareType: '3', permissions: '1', password },
    });
    expect(created.status(), 'create the link').toBe(200);
    const token: string = (await created.json())?.ocs?.data?.token;
    expect(token, 'share token').toBeTruthy();

    // an anonymous client cannot download without the password...
    const anonCtx = await request.newContext({ ignoreHTTPSErrors: true });
    const blocked = await anonCtx.get(`${base()}/s/${token}/download`, { maxRedirects: 0 });
    expect(blocked.status(), 'the download needs the password').not.toBe(200);

    // ...the share page redirects to its own password form, not to the portal...
    const page = await anonCtx.get(`${base()}/s/${token}`, { maxRedirects: 0 });
    expect([200, 303], `the share page -> ${page.status()}`).toContain(page.status());
    const target = page.headers()['location'] ?? '';
    expect(target, 'the share page must not be pushed to the portal').not.toContain('auth.');
    expect(target, 'Nextcloud sends the visitor to its password form').toMatch(/\/s\/[^/]+\/authenticate/);

    // ...that form is served (it is on an exempt path), and it carries the CSRF
    // token the visitor's browser posts back with the password...
    const form = await anonCtx.get(`${base()}${target}`, { maxRedirects: 0 });
    expect(form.status(), `the password form -> ${form.status()}`).toBe(200);
    const html = await form.text();
    const requestToken = (html.match(/data-requesttoken="([^"]+)"/) ?? [])[1];
    expect(requestToken, 'the form carries a request token').toBeTruthy();

    // ...so a visitor who knows the password really can download the file.
    const unlocked = await anonCtx.post(`${base()}/s/${token}/authenticate/showShare`, {
      form: { password, requesttoken: requestToken },
      maxRedirects: 0,
    });
    expect([200, 303], `unlock -> ${unlocked.status()}`).toContain(unlocked.status());
    const download = await anonCtx.get(`${base()}/s/${token}/download`);
    expect(download.status(), 'the anonymous download').toBe(200);
    expect(await download.text(), 'the shared content').toBe(content);

    // The public page is a full Nextcloud web UI; its static assets must load
    // without a portal session or the browser only gets the server-rendered
    // HTML shell.
    for (const asset of [
      '/core/css/server.css',
      '/dist/core-public.js',
      '/apps/theming/theme/default.css',
      '/apps/text/css/text-viewer.css',
    ]) {
      const r = await anonCtx.get(`${base()}${asset}`, { maxRedirects: 0 });
      expect(r.status(), `public share static asset ${asset}`).toBe(200);
      expect(r.headers()['content-type'] ?? '', `asset ${asset} has a static content type`).toMatch(/css|javascript/);
    }

    // The document viewer is token/CSRF-gated, but its endpoints must reach
    // Nextcloud rather than the portal. A redirect to auth.* breaks Collabora.
    const tokenProbe = await anonCtx.post(`${base()}/apps/richdocuments/token`, { maxRedirects: 0 });
    expect(tokenProbe.headers()['location'] ?? '').not.toContain('auth.');
    expect([301, 302, 303, 307, 308]).not.toContain(tokenProbe.status());

    const preview = await anonCtx.get(
      `${base()}/apps/files_sharing/publicpreview/${token}?file=%2F&x=32&y=32&mimeFallback=true`,
      { maxRedirects: 0 },
    );
    expect(preview.headers()['location'] ?? '').not.toContain('auth.');
    expect([301, 302, 303, 307, 308]).not.toContain(preview.status());

    // Private app routes must stay behind the portal.
    const privatePage = await anonCtx.get(`${base()}/index.php/apps/files/`, { maxRedirects: 0 });
    expect([301, 302, 303, 307, 308], 'Nextcloud private routes remain behind the portal').toContain(privatePage.status());
    expect(privatePage.headers()['location'] ?? '').toContain('auth.');

    await anonCtx.dispose();
    // tidy up: drop the share, then the file
    const mine = await (await ctx.get(ocs('?format=json'), { headers: headers() })).json();
    const share = (mine?.ocs?.data ?? []).find((s: any) => s.token === token);
    if (share?.id) {
      await ctx.delete(ocs(`/${share.id}?format=json`), { headers: headers() });
    }
    await ctx.fetch(dav('/ci/public.txt'), { method: 'DELETE', headers: auth() });
    await ctx.dispose();
  });

  test('a calendar client creates a calendar and an event', async () => {
    const ctx = await anon();
    const name = `ci-cal-${Date.now()}`;
    const cal = `${base()}/remote.php/dav/calendars/${NC_ADMIN}/${name}/`;

    const mk = await ctx.fetch(cal, {
      method: 'MKCALENDAR',
      headers: { ...auth(), 'Content-Type': 'application/xml; charset=utf-8' },
      data:
        '<?xml version="1.0" encoding="utf-8"?>' +
        '<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>' +
        '<d:displayname>CI Cal</d:displayname>' +
        '<c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set>' +
        '</d:prop></d:set></c:mkcalendar>',
    });
    expect([201, 405], `MKCALENDAR -> ${mk.status()}`).toContain(mk.status());

    const ics =
      'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//umkim-thin//CI//EN\r\n' +
      'BEGIN:VEVENT\r\nUID:ci-event@test\r\nDTSTAMP:20260101T000000Z\r\n' +
      'DTSTART:20260101T100000Z\r\nDTEND:20260101T110000Z\r\nSUMMARY:CI Event\r\n' +
      'END:VEVENT\r\nEND:VCALENDAR\r\n';
    const put = await ctx.fetch(`${cal}ci-event.ics`, {
      method: 'PUT',
      headers: { ...auth(), 'Content-Type': 'text/calendar; charset=utf-8' },
      data: ics,
    });
    expect([201, 204], `PUT event -> ${put.status()}`).toContain(put.status());

    const read = await ctx.fetch(`${cal}ci-event.ics`, { headers: auth() });
    expect(read.status(), 'read the event back').toBe(200);
    expect(await read.text(), 'the event summary').toContain('CI Event');

    await ctx.fetch(`${cal}ci-event.ics`, { method: 'DELETE', headers: auth() });
    await ctx.dispose();
  });

  test('the document editor is reachable from the browser', async ({ request: r0 }) => {
    // Collabora rolls out (and warms up) well after the app is ready, so give
    // this one more room than the default.
    test.setTimeout(180_000);
    // The browser loads the editor from Collabora and asks it for WOPI
    // discovery. Both must work from outside the cluster: the in-cluster
    // Service name is not reachable from a browser, so the editor has its own
    // route, and it is not behind the forward-auth (Collabora authenticates
    // its own WOPI traffic with per-document tokens).
    const collabora = `https://collabora.${new URL(base()).hostname.split('.').slice(1).join('.')}`;
    // Collabora warms up (and its route rolls out) asynchronously, so poll.
    let status = 0;
    let body = '';
    await waitFor(
      async () => {
        const r = await r0.get(`${collabora}/hosting/discovery`, { maxRedirects: 0 });
        status = r.status();
        if (status === 200) body = await r.text();
        return status === 200;
      },
      { what: `WOPI discovery at ${collabora} (last status ${status})`, timeoutMs: 90_000, intervalMs: 3000 },
    ).catch(async (e) => {
      throw new Error(`${e.message}`);
    });
    expect(status, `WOPI discovery at ${collabora}`).toBe(200);
    expect(body, 'a WOPI discovery document').toContain('wopi-discovery');
    // and Nextcloud is configured to use that public host for the browser
    const caps = await r0.get(`${base()}/ocs/v1.php/cloud/capabilities?format=json`, { headers: headers() });
    expect(caps.status()).toBe(200);
  });

});
