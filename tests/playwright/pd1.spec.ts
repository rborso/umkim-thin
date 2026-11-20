import { test } from '@playwright/test';
import { createUser, oidcLogin, hostOf } from './lib/session';
test('what the page sees', async ({ browser }) => {
  test.setTimeout(220_000);
  const u = `ci-pd-${Date.now()}`; const p = 'Probe' + Math.random().toString(36).slice(2, 16);
  await createUser(u, p, { displayName: u });
  const host = hostOf('bookstack');
  const { page, context } = await oidcLogin(browser, host, {
    credentials: { username: u, password: p },
    ready: async (pg) => { try { const x = new URL(pg.url()); return x.hostname === host && !x.pathname.startsWith('/oidc') && x.pathname !== '/login'; } catch { return false; } },
  });
  console.log('URL', page.url().slice(0, 90));
  const probe = await page.evaluate(async () => {
    const r = await fetch('/my-account/auth', { credentials: 'same-origin' });
    const h = await r.text();
    return { status: r.status, len: h.length, login: h.includes('id="login-form"'), tok: /\/api-tokens\/(\d+)\/create/.test(h), title: (h.match(/<title>([^<]*)</) ?? [])[1] };
  });
  console.log('PROBE', JSON.stringify(probe));
  const cookies = await context.cookies();
  console.log('COOKIES', JSON.stringify(cookies.map(c => c.name)));
  await context.close();
});
