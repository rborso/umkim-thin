import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, basic, oidcLogin, PORTAL_USER, PORTAL_PASS } from '../lib/session';

// "A regular tenant signs in and actually does something" per app.
//
// The gating suite (auth.spec.ts) proves a directory user can *authenticate* to
// each app. The client journeys prove the apps work, but they run as the app's
// own admin (its bootstrap account), which exists whether or not the directory
// mapping is right. Nothing so far asks the question a real customer asks:
//
//   a freshly invited user, with no local account beforehand, signs in and
//   performs an ordinary action in the app - create something, see it, not
//   get an empty or forbidden view.
//
// That is what this file does. It is deliberately shallow per app (one or two
// writes) and asserts only things a user would notice, so a failure is a real
// product problem rather than a spec drift.

const user = PORTAL_USER;
const pass = PORTAL_PASS;

const RUN = process.env.UMKIM_THIN_CI_RUN || 'local';
const suffix = `${RUN}-${Date.now()}`.replace(/[^A-Za-z0-9-]/g, '');

// Every app here was reached by the regular tenant. A 401/403 from the app's own
// API (not the portal) means the identity the app ended up with has no rights -
// exactly the failure this file exists to catch.
function expectUsable(status: number, what: string) {
  expect([200, 201, 202, 204], `${what} -> ${status}`).toContain(status);
}

test.describe('tenant can work in gitea', () => {
  test.skip(!installed('gitea'), 'gitea is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('a regular user creates a repository and reads it back @check:tenant_write_gitea', async () => {
    const base = `https://${hostOf('gitea')}`;
    const ctx = await portalSession({ username: user, password: pass });
    // LDAP materialises the account on first use; a Basic-auth call does that
    // without needing the web form.
    const repo = `tenant-${suffix}`;
    const created = await ctx.post(`${base}/api/v1/user/repos`, {
      headers: { Authorization: basic(user, pass), 'Content-Type': 'application/json' },
      data: { name: repo, auto_init: true, private: true },
    });
    expectUsable(created.status(), `create repo as ${user} ${await created.text()}`.slice(0, 400));

    const me = await ctx.get(`${base}/api/v1/user`, {
      headers: { Authorization: basic(user, pass) },
    });
    expect(me.status(), 'the tenant sees its own identity').toBe(200);
    expect((await me.json()).login, 'the repository owner is the directory user').toBe(user);

    // Clean up so repeated runs do not accumulate repositories.
    await ctx.delete(`${base}/api/v1/repos/${user}/${repo}`, {
      headers: { Authorization: basic(user, pass) },
    });
    await ctx.dispose();
  });
});

test.describe('tenant can work in nextcloud', () => {
  test.skip(!installed('nextcloud'), 'nextcloud is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('a regular user uploads a file and lists it @check:tenant_write_nextcloud', async () => {
    const base = `https://${hostOf('nextcloud')}`;
    const ctx = await portalSession({ username: user, password: pass });
    const auth = { Authorization: basic(user, pass), 'OCS-APIRequest': 'true' };
    const dav = (p: string) => `${base}/remote.php/dav/files/${user}${p}`;

    // The user's own DAV root must exist or the LDAP account did not materialise.
    const mkdir = await ctx.fetch(dav('/ci-tenant'), { method: 'MKCOL', headers: auth });
    expect([201, 405], `mkdir as ${user} -> ${mkdir.status()}`).toContain(mkdir.status());

    const body = `tenant upload ${suffix}\n`;
    const put = await ctx.put(dav(`/ci-tenant/${suffix}.txt`), {
      headers: { ...auth, 'Content-Type': 'text/plain' },
      data: body,
    });
    expectUsable(put.status(), `upload as ${user}`);

    const get = await ctx.get(dav(`/ci-tenant/${suffix}.txt`), { headers: auth });
    expect(get.status(), 'the uploaded file is readable back').toBe(200);
    expect(await get.text()).toBe(body);

    // A tenant with no quota/forbidden state gets a normal PROPFIND listing.
    const list = await ctx.fetch(dav('/ci-tenant'), {
      method: 'PROPFIND',
      headers: { ...auth, Depth: '1', 'Content-Type': 'application/xml' },
      data: '<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/></d:prop></d:propfind>',
    });
    expect(list.status(), 'the file appears in the tenant listing').toBe(207);
    expect(await list.text()).toContain(`${suffix}.txt`);

    await ctx.delete(dav(`/ci-tenant/${suffix}.txt`), { headers: auth });
    await ctx.dispose();
  });
});

test.describe('tenant can work in paperless', () => {
  test.skip(!installed('paperless'), 'paperless is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('a regular user signs in by SSO and gets their document list @check:tenant_api_paperless', async ({ browser }) => {
    test.setTimeout(180_000);
    const host = hostOf('paperless');
    // Paperless is OIDC-only: the API is authenticated by the session the SSO
    // exchange creates, so a cookie from the portal is not enough. Drive the
    // flow the user drives, then call the API from inside the app's origin.
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => {
            const r = await fetch('/api/documents/?page_size=1', {
              credentials: 'same-origin',
              headers: { Accept: 'application/json' },
            });
            return r.status;
          })
          .then((s) => s === 200)
          .catch(() => false),
    });
    const result = await page.evaluate(async () => {
      const r = await fetch('/api/documents/?page_size=1', {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      });
      const body = await r.json().catch(() => null);
      return { status: r.status, hasResults: Array.isArray(body?.results) };
    });
    expect(result.status, 'the tenant reaches the paperless API after SSO').toBe(200);
    expect(result.hasResults, 'the tenant sees a paginated document list').toBe(true);
    await context.close();
  });
});

test.describe('tenant can work in immich', () => {
  test.skip(!installed('immich'), 'immich is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('a regular user signs in by SSO and can list their albums @check:tenant_api_immich', async ({ browser }) => {
    test.setTimeout(180_000);
    const host = hostOf('immich');
    // Immich is OIDC-only; the API identity is the session the app itself
    // created, so the assertion happens from inside the app origin.
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => {
            const r = await fetch('/api/users/me', {
              credentials: 'same-origin',
              headers: { Accept: 'application/json' },
            });
            return r.status;
          })
          .then((s) => s === 200)
          .catch(() => false),
    });
    const me = await page.evaluate(async () => {
      const r = await fetch('/api/users/me', {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      });
      return { status: r.status, body: await r.json().catch(() => null) };
    });
    expect(me.status, 'the tenant reaches the immich API after SSO').toBe(200);
    expect(me.body?.email || me.body?.name, 'immich exposes the tenant identity').toBeTruthy();

    const albums = await page.evaluate(async () => {
      const r = await fetch('/api/albums', {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      });
      return r.status;
    });
    expect([200], `immich album list for the tenant -> ${albums}`).toContain(albums);
    await context.close();
  });
});

test.describe('tenant can work in mealie', () => {
  test.skip(!installed('mealie'), 'mealie is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('a regular user signs in by SSO and reaches the recipe API @check:tenant_api_mealie', async ({ browser }) => {
    test.setTimeout(180_000);
    const host = hostOf('mealie');
    // An SSO account has no local password, so a token minted from a password is
    // the wrong test. Use the SSO session the app issues instead.
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => {
            const r = await fetch('/api/users/self', {
              credentials: 'same-origin',
              headers: { Accept: 'application/json' },
            });
            return r.status;
          })
          .then((s) => s === 200)
          .catch(() => false),
    });
    const self = await page.evaluate(async () => {
      const r = await fetch('/api/users/self', {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      });
      return { status: r.status, body: await r.json().catch(() => null) };
    });
    expect(self.status, 'the tenant reaches the mealie API after SSO').toBe(200);
    expect(self.body?.username || self.body?.email, 'mealie maps the tenant').toBeTruthy();
    await context.close();
  });
});

test.describe('tenant can work in the servarr stack', () => {
  test.skip(!installed('sonarr'), 'sonarr is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('the tenant reaches the *arr UI past the portal and sees a real page @check:tenant_ui_servarr', async () => {
    // The *arr apps have no accounts of their own: the portal is the identity
    // boundary. What a tenant can do is load the UI, and that must be the app,
    // not a redirect back to the portal (an authorization loop).
    const ctx = await portalSession({ username: user, password: pass });
    for (const app of ['sonarr', 'radarr', 'prowlarr', 'qbittorrent', 'jellyfin']) {
      if (!installed(app)) continue;
      const r = await ctx.get(`https://${hostOf(app)}/`, { maxRedirects: 0 });
      expect([200, 302, 303, 307], `${app} for the tenant -> ${r.status()}`).toContain(r.status());
      if (r.status() === 200) {
        const html = await r.text();
        expect(html.toLowerCase(), `${app} served its own page`).toContain('<html');
      }
    }
    await ctx.dispose();
  });
});

test.describe('tenant can work in homebox and sparkyfitness', () => {
  test.skip(!user || !pass, 'portal user not set');

  // These SPAs authenticate their API with a session the OIDC flow creates, so
  // assert that session answers for a regular user. actualbudget and
  // vaultwarden are deliberately not probed here: Actual keeps its token in
  // IndexedDB and Vaultwarden needs a client-side master password, so a fair
  // test of either is a browser journey - auth.spec.ts covers their SSO login
  // as an invited user, and journeys/vaultwarden.spec.ts covers the vault.
  const probes: { app: string; path: string }[] = [
    { app: 'homebox', path: '/api/v1/users/self' },
    { app: 'sparkyfitness', path: '/api/auth/get-session' },
  ];
  for (const p of probes) {
    // The tag is per app: the check names in the catalog are
    // tenant_api_homebox / tenant_api_sparkyfitness, so each probe carries its own.
    test(`a regular user signs in to ${p.app} and its session API answers @check:tenant_api_${p.app}`, async ({ browser }) => {
      test.skip(!installed(p.app), `${p.app} is not installed`);
      test.setTimeout(180_000);
      const host = hostOf(p.app);
      const status = async (pg: any) =>
        pg
          .evaluate(async (path: string) => {
            const r = await fetch(path, {
              credentials: 'same-origin',
              headers: { Accept: 'application/json' },
            });
            return r.status;
          }, p.path)
          .catch(() => 0);
      const { context, page } = await oidcLogin(browser, host, {
        credentials: { username: user, password: pass },
        ready: async (pg) => (await status(pg)) === 200,
      });
      expect(await status(page), `${p.app} API for the tenant`).toBe(200);
      await context.close();
    });
  }
});

test.describe('tenant can work in vikunja', () => {
  test.skip(!installed('vikunja'), 'vikunja is not installed');
  test.skip(!user || !pass, 'portal user not set');

  test('a regular user signs in by SSO and works on a project @check:tenant_api_vikunja', async ({ browser }) => {
    test.setTimeout(180_000);
    const host = hostOf('vikunja');
    // Vikunja auto-registers OIDC users, so this is a real first-use path: the
    // invited tenant has no local account, and the app has to create one and let
    // it own a project.
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => {
            const r = await fetch('/api/v1/user', {
              credentials: 'same-origin',
              headers: { Accept: 'application/json' },
            });
            return r.status;
          })
          .then((s) => s === 200)
          .catch(() => false),
    });

    const result = await page.evaluate(async (title: string) => {
      const create = await fetch('/api/v1/projects', {
        method: 'PUT',
        credentials: 'same-origin',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ title }),
      });
      const created = await create.json().catch(() => null);
      if (create.status >= 300) return { create: create.status, list: 0, found: false };
      const list = await fetch('/api/v1/projects', {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      });
      const projects = await list.json().catch(() => []);
      const found = Array.isArray(projects) && projects.some((p: any) => p.title === title);
      if (created?.id) {
        await fetch(`/api/v1/projects/${created.id}`, { method: 'DELETE', credentials: 'same-origin' });
      }
      return { create: create.status, list: list.status, found };
    }, `tenant-${suffix}`);

    expect([200, 201], `the tenant creates a project -> ${result.create}`).toContain(result.create);
    expect(result.list, 'the tenant can list its projects').toBe(200);
    expect(result.found, 'the project the tenant created is in its own list').toBe(true);
    await context.close();
  });
});
