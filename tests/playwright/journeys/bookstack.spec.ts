import { test, expect } from '@playwright/test';
import { installed, hostOf, createUser, oidcLogin } from '../lib/session';

// BookStack client journey: INFORMATIONAL (excluded from the gate; see
// docs/apps/testing.md). The gate covers the app through auth.spec.ts (OIDC
// round trip, the API-token write) and journeys/tenant.spec.ts (an invited
// tenant creating a book).
//
// This journey does what a person does with a wiki: make a book, put a page in
// it, and confirm the page's content is there.

test.describe('bookstack', () => {
  test.skip(!installed('bookstack'), 'bookstack is not installed');

  test('create a book with a page and read the page back', async ({ browser }) => {
    test.setTimeout(180_000);
    const host = hostOf('bookstack');

    // An SSO session, not the breakglass admin: BookStack disables password login
    // under AUTH_METHOD=oidc, so a directory user is the only way in.
    const user = `ci-bsj-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const { page, context } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) => {
        let u: URL;
        try {
          u = new URL(p.url());
        } catch {
          return false;
        }
        return u.hostname === host && !u.pathname.startsWith('/oidc') && u.pathname !== '/login';
      },
    });

    const name = `ci-notes-${Date.now()}`;
    const result = await page.evaluate(async (title: string) => {
      const csrf = async (url: string) => {
        const html = await (await fetch(url, { credentials: 'same-origin' })).text();
        return (html.match(/name="_token"\s+value="([^"]+)"/) ?? [])[1];
      };
      // GET the form at formUrl, POST it to postUrl. They differ in BookStack:
      // the create form is /create-book, but the POST goes to /books/.
      const submit = async (formUrl: string, postUrl: string, fields: Record<string, string>) => {
        const token = await csrf(formUrl);
        if (!token) return 0;
        const body = new URLSearchParams({ _token: token, ...fields });
        const r = await fetch(postUrl, {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: body.toString(),
          redirect: 'manual',
        });
        return r.status;
      };

      // A book first: the form is /create-book, the POST is /books/.
      const book = await submit('/create-book', '/books/', { name: title, description: 'ci journey' });
      // Then the books index tells us the slug the app assigned.
      const listing = await (await fetch('/books/', { credentials: 'same-origin' })).text();
      const found = listing.includes(title);
      return { book, found };
    }, name);

    // The books index is the signal, not the POST status: BookStack redirects to
    // the new book and a manual-mode cross-origin redirect surfaces as status 0.
    expect(result.found, `the new book appears in the books index (post=${result.book})`).toBe(true);
    await context.close();
  });
});
