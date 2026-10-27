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

  test('a regular directory user signs in (LDAP), and the API shows that identity', async () => {
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

  test('a regular directory user authenticates against Jellyfin (LDAP)', async () => {
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
      headers: { ...authHeader, 'X-Emby-Token': token },
    });
    expect(me.status(), 'Jellyfin serves the authenticated user').toBe(200);
    expect((await me.json()).Name, 'the directory user is mapped into Jellyfin').toBe(user);
    await ctx.dispose();
  });
});

test.describe('immich directory login', () => {
  test.skip(!installed('immich'), 'immich is not installed');

  test('a regular directory user signs in with the real OIDC flow', async ({ browser }) => {
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

  test('a regular directory user signs in with the real OIDC flow', async ({ browser }) => {
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

  test('a regular directory user authenticates against Nextcloud (LDAP)', async () => {
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

  test('git clones and pushes over the dedicated SSH port', async () => {
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
