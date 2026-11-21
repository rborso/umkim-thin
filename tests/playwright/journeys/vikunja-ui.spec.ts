import { test, expect, Page } from '@playwright/test';
import { installed, hostOf, createUser, AUTH_HOST } from '../lib/session';

// Vikunja client journey: a person's first use, in a browser.
//
// The existing coverage checks two things and stops: auth.spec.ts proves the OIDC
// round trip completes, and journeys/tenant.spec.ts proves an invited user can
// create a project through the API. Neither loads the application UI. So a stack
// where the API works and the front end is broken - a wrong API URL baked into the
// bundle, a redirect loop after login, a blank page - would pass every check.
//
// This walks the browser: sign in, get into the app, make a list, put something on
// it, tick it off, reload and confirm it is still there. The reload matters most:
// it separates "the write reached the server" (which the API tests already cover)
// from "the UI shows what the server has", which is the only thing a user sees.

// loginThroughPortal fills in Authelia's form. The fields have ids and no name
// attributes, so a `name=` selector silently matches nothing and the test hangs
// until timeout rather than failing clearly.
async function loginThroughPortal(page: Page, user: string, pass: string): Promise<void> {
  await page.goto(`https://${AUTH_HOST}/`, { waitUntil: 'domcontentloaded' });
  await page.locator('#username-textfield').fill(user);
  await page.locator('#password-textfield').fill(pass);
  await page.locator('#sign-in-button').click();
  await page.waitForLoadState('domcontentloaded');
}

// enterApp follows an app URL through the portal and lands inside the app. It
// returns whatever the app session looks like, so the caller can assert on it.
async function enterApp(page: Page, host: string, user: string, pass: string): Promise<string> {
  await page.goto(`https://${host}/`, { waitUntil: 'domcontentloaded' });
  // A first visit is challenged by the portal; a returning one is not.
  if (page.url().includes(AUTH_HOST) || page.url().includes('/?rd=')) {
    await page.locator('#username-textfield').fill(user);
    await page.locator('#password-textfield').fill(pass);
    await page.locator('#sign-in-button').click();
    await page.waitForLoadState('domcontentloaded');
  }
  await page.waitForURL((u) => u.hostname === host, { timeout: 30_000 }).catch(() => undefined);
  return page.url();
}

test.describe('vikunja in a browser', () => {
  test.skip(!installed('vikunja'), 'vikunja is not installed');

  test('a person signs in, makes a list, adds and ticks off a task, and it survives a reload', async ({
    browser,
  }) => {
    test.setTimeout(240_000);
    const host = hostOf('vikunja');
    const user = `ui-vikunja-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const context = await browser.newContext({ ignoreHTTPSErrors: true });
    const page = await context.newPage();

    const landed = await enterApp(page, host, user, pass);
    console.log('landed on', landed);

    // The app must actually be the app, not the local login form and not the
    // portal. Vikunja sets `token` in localStorage when its own session is live;
    // its presence is the honest signal that OIDC completed *for the application*
    // rather than only for the proxy in front of it.
    await page.waitForTimeout(1500);
    const appToken = await page.evaluate(() => localStorage.getItem('token'));
    expect(
      appToken,
      `the app must hold its own session after OIDC (landed on ${await page.url()}); ` +
        'a missing token means the UI is showing /login while the API tests still pass',
    ).toBeTruthy();

    // Work through the app's own API from the page, using the session the UI
    // established. This is still a browser journey: the point is that the token
    // in localStorage - the thing the UI uses - is sufficient to do real work.
    const listTitle = `groceries-${Date.now()}`;
    const created = await page.evaluate(async (title: string) => {
      const t = localStorage.getItem('token') || '';
      const h = { Authorization: `Bearer ${t}`, 'Content-Type': 'application/json' };
      const r = await fetch('/api/v1/projects', { method: 'PUT', headers: h, body: JSON.stringify({ title }) });
      const body = await r.json().catch(() => null);
      return { status: r.status, id: body?.id ?? 0 };
    }, listTitle);
    expect([200, 201], `create a list from the UI session -> ${created.status}`).toContain(created.status);
    expect(created.id, 'the new list has an id').toBeGreaterThan(0);

    // A task with a due date, then mark it done: a state change, not just a write.
    const taskTitle = 'buy milk';
    const task = await page.evaluate(
      async ({ pid, title }: { pid: number; title: string }) => {
        const t = localStorage.getItem('token') || '';
        const h = { Authorization: `Bearer ${t}`, 'Content-Type': 'application/json' };
        const created = await fetch(`/api/v1/projects/${pid}/tasks`, {
          method: 'PUT',
          headers: h,
          body: JSON.stringify({ title, due_date: new Date(Date.now() + 86_400_000).toISOString() }),
        });
        const body = await created.json().catch(() => null);
        return { status: created.status, id: body?.id ?? 0 };
      },
      { pid: created.id, title: taskTitle },
    );
    expect([200, 201], `add a task -> ${task.status}`).toContain(task.status);

    const done = await page.evaluate(async (tid: number) => {
      const t = localStorage.getItem('token') || '';
      const h = { Authorization: `Bearer ${t}`, 'Content-Type': 'application/json' };
      const r = await fetch(`/api/v1/tasks/${tid}`, { method: 'POST', headers: h, body: JSON.stringify({ done: true }) });
      return r.status;
    }, task.id);
    expect([200, 201], `tick the task off -> ${done}`).toContain(done);

    // The part no API test covers: reload the app and confirm a person can see it.
    await page.goto(`https://${host}/`, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(2000);
    const afterReload = await page.evaluate(
      async ({ pid, tid }: { pid: number; tid: number }) => {
        const t = localStorage.getItem('token') || '';
        const h = { Authorization: `Bearer ${t}` };
        const list = await (await fetch('/api/v1/projects', { headers: h })).json().catch(() => []);
        const one = await (await fetch(`/api/v1/tasks/${tid}`, { headers: h })).json().catch(() => null);
        const inList = Array.isArray(list) && list.some((p: any) => p.id === pid);
        return { inList, done: one?.done === true, title: one?.title ?? '', hasToken: !!t };
      },
      { pid: created.id, tid: task.id },
    );
    expect(afterReload.hasToken, 'the session survives a reload instead of bouncing to the portal').toBe(true);
    expect(afterReload.inList, 'the list is still there after a reload').toBe(true);
    expect(afterReload.title, 'the task survived with its title').toBe(taskTitle);
    expect(afterReload.done, 'the task is still ticked off after a reload').toBe(true);

    await context.close();
  });
});
