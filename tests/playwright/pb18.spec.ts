import { test, expect } from '@playwright/test';
import { createUser, oidcLogin, hostOf } from './lib/session';
test('tenant gets a role', async ({ browser }) => {
  test.setTimeout(220_000);
  const u = `ci-br-${Date.now()}`; const p = 'Probe' + Math.random().toString(36).slice(2, 16);
  await createUser(u, p, { displayName: u });
  const host = hostOf('bookstack');
  const { page, context } = await oidcLogin(browser, host, {
    credentials: { username: u, password: p },
    ready: async (pg) => { try { const x = new URL(pg.url()); return x.hostname === host && !x.pathname.startsWith('/oidc') && x.pathname !== '/login'; } catch { return false; } },
  });
  const body = await page.locator('body').innerText().catch(() => '');
  console.log('CREATEBTN', await page.locator('a[href*="/create"], .button').count());
  console.log('BODY', body.slice(0, 200).replace(/\n/g, ' | '));
  // Try creating a page as the tenant.
  const r = await page.evaluate(async (title: string) => {
    const f = await fetch('/create-page', { credentials: 'same-origin' });
    const html = await f.text();
    const token = (html.match(/name="_token"\s+value="([^"]+)"/) ?? [])[1];
    if (!token) return { status: 0, why: 'no csrf' };
    const form = new URLSearchParams({ _token: token, name: title, html: '<p>tenant wrote this</p>' });
    const post = await fetch('/create-page', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: form.toString(), redirect: 'manual' });
    return { status: post.status, why: post.headers.get('location') ?? '' };
  }, `tenant-page-${Date.now()}`);
  console.log('CREATE', JSON.stringify(r));
  await context.close();
});
