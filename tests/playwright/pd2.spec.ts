import { test } from '@playwright/test';
import { createUser, oidcLogin, hostOf } from './lib/session';
test('account page content', async ({ browser }) => {
  test.setTimeout(220_000);
  const u = `ci-pe-${Date.now()}`; const p = 'Probe' + Math.random().toString(36).slice(2, 16);
  await createUser(u, p, { displayName: u });
  const host = hostOf('bookstack');
  const { page, context } = await oidcLogin(browser, host, {
    credentials: { username: u, password: p },
    ready: async (pg) => { try { const x = new URL(pg.url()); return x.hostname === host && !x.pathname.startsWith('/oidc') && x.pathname !== '/login'; } catch { return false; } },
  });
  const info = await page.evaluate(async () => {
    const h = await (await fetch('/my-account/auth', { credentials: 'same-origin' })).text();
    const links = [...h.matchAll(/href="([^"]*api-token[^"]*)"/g)].map(m => m[1]);
    const createLinks = [...h.matchAll(/href="([^"]*create[^"]*)"/g)].map(m => m[1]).slice(0, 5);
    return { links, createLinks, hasApiTokens: /api-token/i.test(h), snippet: (h.match(/.{60}api-token.{80}/i) ?? ['none'])[0].replace(/\s+/g, ' ') };
  });
  console.log('INFO', JSON.stringify(info));
  await context.close();
});
