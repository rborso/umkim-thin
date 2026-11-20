import { test } from '@playwright/test';
import { createUser, oidcLogin, hostOf } from './lib/session';
test('tenant session token', async ({ browser }) => {
  test.setTimeout(220_000);
  const u = `ci-bt2-${Date.now()}`; const p = 'Probe' + Math.random().toString(36).slice(2, 16);
  await createUser(u, p, { displayName: u });
  const host = hostOf('bookstack');
  const { page, context } = await oidcLogin(browser, host, {
    credentials: { username: u, password: p },
    ready: async (pg) => { try { const x = new URL(pg.url()); return x.hostname === host && !x.pathname.startsWith('/oidc') && x.pathname !== '/login'; } catch { return false; } },
  });
  const r = await page.evaluate(async (title: string) => {
    const f = await fetch('/create-book', { credentials: 'same-origin' });
    const html = await f.text();
    const token = (html.match(/name="_token"\s+value="([^"]+)"/) ?? [])[1];
    if (!token) { const body = await (await fetch('/books/')).text(); return { st: 0, snip: body.slice(0, 150).replace(/\s+/g, ' ') }; }
    const b = new URLSearchParams({ _token: token, name: title, description: 'x' });
    const post = await fetch('/books/', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: b.toString(), redirect: 'manual' });
    const list = await (await fetch('/books/', { credentials: 'same-origin' })).text();
    return { st: post.status, found: list.includes(title), loc: post.headers.get('location') ?? '' };
  }, `tb-${Date.now()}`);
  console.log('RESULT', JSON.stringify(r));
  await context.close();
});
