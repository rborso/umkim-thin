import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  installed,
  hostOf,
  portalSession,
  basic,
  createUser,
  oidcLogin,
  waitFor,
  anon,
  appAdmin,
  appToken,
  tokenInStorage,
  PORTAL_USER,
  PORTAL_PASS,
} from './lib/session';

// Gating directory-login checks: every installed app must accept a real
// directory account, not only the app's local admin or the portal session.
//
// These are deliberately separate from the per-app client journeys (which are
// informational): a broken LDAP/OIDC login is a platform failure, so this file
// runs with apps.spec.ts and tls.spec.ts. The *arr/qBittorrent/Seerr/Prowlarr
// apps have no own login; their forward-auth path is apps.spec.ts with the same
// regular portal user.
//
// PORTAL_USER is an invited regular tenant; the console/invite API keeps using
// the directory admin (UMKIM_THIN_USER).

const user = PORTAL_USER;
const pass = PORTAL_PASS;

// Unique per run, with a random password: a rerun must not depend on (or leave
// behind) a fixed credential. ci/cleanup-ci.sh uses the same run id.
const CI_RUN = process.env.UMKIM_THIN_CI_RUN || 'local';
const ciUser = (prefix: string) => `${prefix}-${CI_RUN}`;
// LLDAP rejects some non-alphanumeric characters in invite-set passwords;
// keep the generated password in the same character set as the invite helper.
const ciPassword = () => randomBytes(18).toString('hex');

test.describe('gitea directory login', () => {
  test.skip(!installed('gitea'), 'gitea is not installed');
  test.skip(!user || !pass, 'UMKIM_THIN_PORTAL_USER / _PASSWORD not set');

  test('a regular directory user signs in (LDAP), and the API shows that identity @check:ldap_user_visible', async () => {
    const host = hostOf('gitea');
    const base = `https://${host}`;
    const ctx = await portalSession({ username: user, password: pass });

    const page = await ctx.get(`${base}/user/login`);
    expect(page.status(), 'gitea login page behind the portal').toBe(200);
    const csrf = ((await page.text()).match(/name="_csrf"\s+value="([^"]+)"/) ?? [])[1];
    const login = await ctx.post(`${base}/user/login`, {
      form: { user_name: user, password: pass, _csrf: csrf ?? '' },
      maxRedirects: 0,
    });
    expect([200, 302, 303], `gitea login -> ${login.status()}`).toContain(login.status());

    // Gitea creates the account on the first LDAP login; Basic auth then proves
    // the same directory identity is visible to clients.
    const me = await ctx.get(`${base}/api/v1/user`, {
      headers: { Authorization: basic(user, pass) },
    });
    expect(me.status(), 'gitea /api/v1/user').toBe(200);
    expect((await me.json()).login, 'the directory user is visible in Gitea').toBe(user);
    await ctx.dispose();
  });
});

test.describe('jellyfin directory login', () => {
  test.skip(!installed('jellyfin'), 'jellyfin is not installed');
  test.skip(!user || !pass, 'UMKIM_THIN_PORTAL_USER / _PASSWORD not set');

  test('a regular directory user authenticates against Jellyfin (LDAP) @check:ldap_user_visible', async () => {
    test.setTimeout(180_000);
    const host = hostOf('jellyfin');
    const base = `https://${host}`;
    const authHeader = {
      Authorization: 'MediaBrowser Client="ci", Device="ci", DeviceId="ci-playwright", Version="1.0"',
    };
    const ctx = await portalSession({ username: user, password: pass });

    let token = '';
    await waitFor(
      async () => {
        const r = await ctx.post(`${base}/Users/AuthenticateByName`, {
          headers: authHeader,
          data: { Username: user, Pw: pass },
        });
        if (r.status() !== 200) return false;
        token = (await r.json()).AccessToken ?? '';
        return Boolean(token);
      },
      { what: `Jellyfin to authenticate ${user} against the directory`, timeoutMs: 120_000, intervalMs: 5000 },
    );
    const me = await ctx.get(`${base}/Users/Me`, {
      // Jellyfin 12 ignores the legacy X-Emby-Token unless legacy authorization
      // is enabled; the standard MediaBrowser header carries the token.
      headers: {
        Authorization: `MediaBrowser Client="ci", Device="ci", DeviceId="ci-playwright", Version="1.0", Token="${token}"`,
        'X-Emby-Token': token,
      },
    });
    expect(me.status(), 'Jellyfin serves the authenticated user').toBe(200);
    expect((await me.json()).Name, 'the directory user is mapped into Jellyfin').toBe(user);
    await ctx.dispose();
  });
});

test.describe('immich directory login', () => {
  test.skip(!installed('immich'), 'immich is not installed');

  test('a regular directory user signs in with the real OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('immich');
    const username = ciUser('ci-immich');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    let callbackStatus = 0;
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
      onPage: (p) =>
        p.on('response', (r) => {
          if (r.url().includes('/api/oauth/callback')) callbackStatus = r.status();
        }),
      ready: async (p) => callbackStatus >= 200 && !p.url().includes('/auth/login'),
    });
    expect(callbackStatus, 'the app exchanged the code').toBeGreaterThanOrEqual(200);
    expect(callbackStatus, 'the exchange succeeded').toBeLessThan(300);
    expect(page.url(), 'still on the app').toContain(host);
    expect(page.url(), 'not on the login page any more').not.toContain('/auth/login');
    await context.close();
  });
});

test.describe('paperless directory login', () => {
  test.skip(!installed('paperless'), 'paperless is not installed');

  test('a regular directory user signs in with the real OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('paperless');
    const username = ciUser('ci-paperless');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
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
    await expect(page).toHaveURL(new RegExp(`^https://${host.replace(/\./g, '\\.')}`));
    const docs = await page.evaluate(async () => {
      const r = await fetch('/api/documents/?page_size=1', {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      });
      return r.status;
    });
    expect(docs, 'the document API answers after SSO').toBe(200);
    await context.close();
  });
});

test.describe('nextcloud directory login', () => {
  test.skip(!installed('nextcloud'), 'nextcloud is not installed');

  test('a regular directory user authenticates against Nextcloud (LDAP) @check:ldap_user_visible', async () => {
    const host = hostOf('nextcloud');
    const base = `https://${host}`;
    const username = ciUser('ci-nextcloud');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    // The portal accepts the user, and the app's own DAV endpoint (which uses
    // the same LDAP backend) accepts the same credentials. The client journey
    // uses the local nc-admin, so this is the path it cannot cover.
    const ctx = await portalSession({ username, password });
    const dav = await ctx.get(`${base}/remote.php/dav/`, {
      headers: { Authorization: basic(username, password) },
    });
    expect([200, 207], `Nextcloud LDAP login via DAV -> ${dav.status()}`).toContain(dav.status());
    await ctx.dispose();
  });
});

test.describe('gitea ssh', () => {
  test.skip(!installed('gitea'), 'gitea is not installed');
  const admin = appAdmin('gitea');
  test.skip(!admin.pass, 'UMKIM_THIN_GITEA_ADMIN_PASSWORD not set');

  test('git clones and pushes over the dedicated SSH port @check:git_ssh_reachable', async () => {
    test.setTimeout(120_000);
    const host = hostOf('gitea');
    const port = process.env.UMKIM_THIN_GITEA_SSH_PORT || '2222';
    const suffix = `${CI_RUN}-${Date.now()}`;
    const title = `ci-ssh-${suffix}`;
    const repo = `ci-ssh-${suffix}`;
    const dir = mkdtempSync(join(tmpdir(), `umkim-git-${process.env.TARGET || 'local'}-`));
    const key = join(dir, 'id_ed25519');
    let keyId = 0;
    let api: Awaited<ReturnType<typeof portalSession>> | undefined;
    let auth: { Authorization: string } | undefined;

    try {
      execFileSync('ssh-keygen', ['-q', '-t', 'ed25519', '-N', '', '-f', key]);
      const pub = readFileSync(`${key}.pub`, 'utf8').trim();

      // The Gitea API is behind forward-auth: the Authelia session (directory
      // admin) is required in addition to the Gitea Basic credentials.
      api = await portalSession();
      auth = { Authorization: basic(admin.user, admin.pass) };
      const keyRes = await api.post(`https://${host}/api/v1/user/keys`, {
        headers: { ...auth, 'Content-Type': 'application/json' },
        data: { title, key: pub, read_only: false },
      });
      expect([200, 201, 422], `gitea key -> ${keyRes.status()}`).toContain(keyRes.status());
      if (keyRes.ok()) keyId = (await keyRes.json().catch(() => ({}))).id ?? 0;

      const repoRes = await api.post(`https://${host}/api/v1/user/repos`, {
        headers: { ...auth, 'Content-Type': 'application/json' },
        data: { name: repo, auto_init: true, private: true, default_branch: 'main' },
      });
      expect([201, 409], `gitea repo -> ${repoRes.status()}`).toContain(repoRes.status());

      const env = {
        ...process.env,
        GIT_SSH_COMMAND: `ssh -i ${key} -p ${port} -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null`,
      };
      const clone = join(dir, 'repo');
      execFileSync('git', ['clone', `ssh://git@${host}:${port}/${admin.user}/${repo}.git`, clone], {
        env,
        stdio: 'pipe',
      });
      writeFileSync(join(clone, 'ci.txt'), 'ssh works\n');
      execFileSync('git', ['-C', clone, 'add', 'ci.txt'], { env, stdio: 'pipe' });
      execFileSync('git', ['-C', clone, '-c', 'user.email=ci@ci.local', '-c', 'user.name=ci', 'commit', '-m', 'ssh'], {
        env,
        stdio: 'pipe',
      });
      execFileSync('git', ['-C', clone, 'push', 'origin', 'main'], { env, stdio: 'pipe' });

      const file = await api.get(`https://${host}/api/v1/repos/${admin.user}/${repo}/contents/ci.txt`, {
        headers: auth,
      });
      expect(file.status(), 'the pushed file is visible in Gitea').toBe(200);
    } finally {
      // The repository and key are test fixtures, not CI state: remove them
      // even when the clone/push assertion fails.
      if (api && auth) {
        if (keyId) await api.delete(`https://${host}/api/v1/user/keys/${keyId}`, { headers: auth }).catch(() => undefined);
        await api.delete(`https://${host}/api/v1/repos/${admin.user}/${repo}`, { headers: auth }).catch(() => undefined);
      }
      if (api) await api.dispose().catch(() => undefined);
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

test.describe('sparkyfitness directory login', () => {
  test.skip(!installed('sparkyfitness'), 'sparkyfitness is not installed');

  test('a regular directory user signs in with the real OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('sparkyfitness');
    const username = ciUser('ci-sparky');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    let callbackStatus = 0;
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
      onPage: (p) =>
        p.on('response', (r) => {
          if (r.url().includes('/api/auth/sso/callback/')) callbackStatus = r.status();
        }),
      ready: async (p) =>
        p
          .evaluate(async () => {
            const r = await fetch('/api/auth/get-session', {
              credentials: 'same-origin',
              headers: { Accept: 'application/json' },
            });
            if (r.status !== 200) return false;
            const body = await r.json().catch(() => null);
            return Boolean(body?.user?.email);
          })
          .catch(() => false),
    });
    expect(callbackStatus, 'the app exchanged the code').toBeGreaterThanOrEqual(200);
    // Better Auth completes the exchange with a 302 back to the app.
    expect(callbackStatus, 'the exchange succeeded').toBeLessThan(400);
    expect(page.url(), 'still on the app').toContain(host);
    await context.close();
  });
});

test.describe('actualbudget directory login', () => {
  test.skip(!installed('actualbudget'), 'actualbudget is not installed');

  test('a directory user completes the Actual Budget OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('actualbudget');
    const username = ciUser('ci-actual');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    let callbackStatus = 0;
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
      onPage: (p) =>
        p.on('response', (r) => {
          if (r.url().includes('/openid/callback')) callbackStatus = r.status();
        }),
      ready: async (p) =>
        p
          .evaluate(() => {
            const text = document.body.innerText || '';
            return text.includes('Server online') && !text.includes('Start using OpenID');
          })
          .catch(() => false),
    });
    expect(callbackStatus, 'Actual Budget exchanged the code').toBeGreaterThanOrEqual(200);
    expect(callbackStatus, 'Actual Budget callback succeeded').toBeLessThan(400);
    expect(page.url(), 'still on the app').toContain(host);
    await context.close();
  });
});

test.describe('homebox directory login', () => {
  test.skip(!installed('homebox'), 'homebox is not installed');

  test('a directory user completes the HomeBox OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('homebox');
    const username = ciUser('ci-homebox');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    let callbackStatus = 0;
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
      onPage: (p) =>
        p.on('response', (r) => {
          if (r.url().includes('/api/v1/users/login/oidc/callback')) callbackStatus = r.status();
        }),
      ready: async (p) =>
        p
          .evaluate(() => {
            const text = document.body.innerText || '';
            return location.pathname === '/home' || text.includes('Sign Out');
          })
          .catch(() => false),
    });
    expect(callbackStatus, 'HomeBox exchanged the code').toBeGreaterThanOrEqual(200);
    expect(callbackStatus, 'HomeBox callback succeeded').toBeLessThan(400);
    expect(page.url(), 'still on the app').toContain(host);
    await context.close();
  });
});

test.describe('vikunja directory login', () => {
  test.skip(!installed('vikunja'), 'vikunja is not installed');

  test('a directory user completes the Vikunja OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('vikunja');
    const username = ciUser('ci-vikunja');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    // Vikunja's callback is per provider key, and it is on the v2 API:
    // /api/v2/auth/openid/<key>/callback (verified by watching the live exchange).
    // v1 has the same route, but the app's own frontend uses v2, so that is what
    // a real login exercises.
    let callbackStatus = 0;
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
      onPage: (p) =>
        p.on('response', (r) => {
          if (r.url().includes('/auth/openid/') && r.request().method() === 'POST') {
            callbackStatus = r.status();
          }
        }),
      ready: async (p) => tokenInStorage(p),
    });
    expect(callbackStatus, 'Vikunja exchanged the code').toBeGreaterThanOrEqual(200);
    expect(callbackStatus, 'Vikunja callback succeeded').toBeLessThan(400);
    expect(page.url(), 'still on the app').toContain(host);
    await context.close();
  });
});

test.describe('vikunja api token', () => {
  test.skip(!installed('vikunja'), 'vikunja is not installed');
  const admin = appAdmin('vikunja');
  test.skip(!admin.user || !admin.pass, 'VIKUNJA admin credentials not set');

  test('an API token creates a project and a task @check:vikunja_api_token_creates_task', async () => {
    test.setTimeout(120_000);
    const host = hostOf('vikunja');
    const base = `https://${host}`;
    // The hook mints a scoped token as the bootstrap admin; a token that cannot
    // write would make this pass for the wrong reason, so the check writes.
    const token = await appToken('vikunja', admin);
    expect(token, 'the token hook returned a token').toBeTruthy();

    // The app's API host is behind Authelia, so even a valid API token needs the
    // portal session to get past the ingress. appToken() authenticates through
    // the public host for the same reason; this context carries that cookie.
    const ctx = await portalSession();
    const auth = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
    const project = `ci-${ciUser('vj')}-${Date.now()}`;
    let projectId = 0;
    try {
      const created = await ctx.put(`${base}/api/v1/projects`, {
        headers: auth,
        data: { title: project },
      });
      // A 200 carrying HTML means the request never reached the app: the portal
      // served its own page. Say that instead of failing on JSON.parse.
      const createdText = await created.text();
      expect(createdText, `create project -> ${created.status()}`).not.toMatch(/^<!DOCTYPE/i);
      expect([200, 201], `create project -> ${created.status()}`).toContain(created.status());
      projectId = JSON.parse(createdText).id;

      const taskTitle = `${project}-task`;
      const task = await ctx.put(`${base}/api/v1/projects/${projectId}/tasks`, {
        headers: auth,
        data: { title: taskTitle },
      });
      expect([200, 201], `create task -> ${task.status()}`).toContain(task.status());
      const taskId = (await task.json()).id;

      // Read back through a different request: proves it persisted, not just
      // that the create response echoed the payload. Needs the token's
      // "read_one" permission, which is separate from "read_all".
      const read = await ctx.get(`${base}/api/v1/tasks/${taskId}`, { headers: auth });
      expect(read.status(), 'the task is readable back').toBe(200);
      expect((await read.json()).title, 'the stored task is the one created').toBe(taskTitle);
    } finally {
      if (projectId) {
        await ctx.delete(`${base}/api/v1/projects/${projectId}`, { headers: auth }).catch(() => undefined);
      }
      await ctx.dispose();
    }
  });
});

test.describe('bookstack directory login', () => {
  test.skip(!installed('bookstack'), 'bookstack is not installed');

  test('a directory user completes the BookStack OIDC flow @check:oidc_sso_completes', async ({ browser }) => {
    test.setTimeout(120_000);
    const host = hostOf('bookstack');
    const username = ciUser('ci-bookstack');
    const password = ciPassword();
    await createUser(username, password, { displayName: username });

    let callbackStatus = 0;
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username, password },
      onPage: (p) =>
        p.on('response', (r) => {
          if (r.url().includes('/oidc/callback')) callbackStatus = r.status();
        }),
      // BookStack sets a session and redirects to the shelf/home page.
      ready: async (p) => {
        const url = p.url();
        return url.includes(host) && !url.includes('/oidc') && !url.includes('/login');
      },
    });
    expect(callbackStatus, 'BookStack exchanged the code').toBeGreaterThanOrEqual(200);
    expect(callbackStatus, 'BookStack callback succeeded').toBeLessThan(400);
    expect(page.url(), 'still on the app').toContain(host);
    await context.close();
  });
});

test.describe('bookstack api token', () => {
  test.skip(!installed('bookstack'), 'bookstack is not installed');
  const admin = appAdmin('bookstack');
  test.skip(!admin.user || !admin.pass, 'BOOKSTACK admin credentials not set');

  test('an API token writes a page and reads it back @check:bookstack_api_token_writes_page', async () => {
    test.setTimeout(120_000);
    const host = hostOf('bookstack');
    const base = `https://${host}`;
    // The hook drives BookStack's own token form; there is no JSON way to mint
    // one. It returns "id:secret", which is the format the API expects.
    const token = await appToken('bookstack', admin);
    expect(token, 'the token hook returned a token').toContain(':');

    // The app host is behind Authelia, so the session rides along with the API
    // token - the same as the other catalog apps.
    const ctx = await portalSession();
    const auth = { Authorization: `Token ${token}`, 'Content-Type': 'application/json' };
    const name = `ci-page-${ciUser('bs')}-${Date.now()}`;
    let pageId = 0;
    try {
      const created = await ctx.post(`${base}/api/pages`, {
        headers: auth,
        data: { name, html: '<p>written by the umkim-thin gate</p>' },
      });
      const body = await created.text();
      expect(body, `create page -> ${created.status()}`).not.toMatch(/^<!DOCTYPE/i);
      expect([200, 201], `create page -> ${created.status()} ${body.slice(0, 200)}`).toContain(created.status());
      pageId = JSON.parse(body).id;

      // Read it back through a separate request: proves it was stored rather
      // than echoed from the create payload.
      const read = await ctx.get(`${base}/api/pages/${pageId}`, { headers: auth });
      expect(read.status(), 'the page is readable back').toBe(200);
      const stored = await read.json();
      expect(stored.name, 'the stored page is the one created').toBe(name);
      expect(stored.html, 'the body survived the round trip').toContain('umkim-thin gate');
    } finally {
      if (pageId) {
        await ctx.delete(`${base}/api/pages/${pageId}`, { headers: auth }).catch(() => undefined);
      }
      await ctx.dispose();
    }
  });
});
