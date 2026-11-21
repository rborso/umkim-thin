import { test, expect, Page } from '@playwright/test';
import { installed, hostOf, createUser, AUTH_HOST } from '../lib/session';

// BookStack client journey: what a person does with a wiki, in a browser.
//
// journeys/bookstack.spec.ts already posts the create-book and create-page forms
// and checks the index lists the book. That proves the writes work. It does not
// prove the reading and editing side - that the page renders, that the editor
// opens with the existing text, that an edit replaces the old body - which is what
// a wiki is actually for, and what breaks first when the asset bundle or the
// session handling regresses while the API keeps working.
//
// This drives the real UI: click "Books", click "Create new book", fill the form,
// submit, then click into the book and create a page through the editor.
//
// It deliberately clicks rather than scraping tokens out of HTML. The previous
// attempt did scrape, and its book-id regex silently matched nothing, so it
// created a page with no book and reported "no csrf token" - a broken test that
// looked like a broken app.

async function bookstackLogin(page: Page, host: string, user: string, pass: string): Promise<void> {
  await page.goto(`https://${host}/`, { waitUntil: 'domcontentloaded' });
  // First the portal.
  if (page.url().includes(AUTH_HOST) || page.url().includes('/?rd=')) {
    await page.locator('#username-textfield').fill(user);
    await page.locator('#password-textfield').fill(pass);
    await page.locator('#sign-in-button').click();
    await page.waitForLoadState('domcontentloaded');
  }
  await page.waitForURL((u) => u.hostname === host, { timeout: 45_000 }).catch(() => undefined);
  await page.waitForTimeout(1500);

  // BookStack then shows its own /login, and under AUTH_METHOD=oidc the only
  // usable control there is the #oidc-login button. It is not a link and its text
  // is only an icon, so it has to be found by id - a getByRole name match has
  // nothing to match against.
  if (new URL(page.url()).pathname === '/login') {
    await page.locator('#oidc-login').click();
    await page.waitForURL((u) => u.hostname === host && !u.pathname.startsWith('/login'), {
      timeout: 45_000,
    }).catch(() => undefined);
    await page.waitForTimeout(2000);
  }
}


// writeInEditor sets BookStack's page body through TinyMCE's own API.
//
// Three approaches were tried, and only this one is correct:
//   * fill() on textarea[name=html] - the textarea is HIDDEN behind the editor,
//     so fill() waits for visibility and the test hangs;
//   * assigning textarea.value - the form posts what the WIDGET holds, so the
//     save succeeds and stores an empty page, which looks like data loss;
//   * clicking into the iframe and typing - works interactively but the click
//     blocks here, because the iframe is inside a widget that manages focus.
// setContent() is the editor's documented entry point, and the save then stores
// exactly what was set.
async function waitForEditor(page: Page): Promise<void> {
  await page
    .waitForFunction(
      () => {
        const w = window as any;
        return !!(w.tinymce?.get?.('html-editor') ?? w.tinymce?.activeEditor);
      },
      undefined,
      { timeout: 30_000 },
    )
    .catch(() => undefined);
}

async function writeInEditor(page: Page, text: string): Promise<void> {
  await waitForEditor(page);
  const ok = await page.evaluate((html: string) => {
    const w = window as any;
    const ed = w.tinymce?.get?.('html-editor') ?? w.tinymce?.activeEditor;
    if (!ed) return false;
    ed.setContent(`<p>${html}</p>`);
    ed.fire('change');
    return true;
  }, text);
  if (!ok) throw new Error('the TinyMCE editor instance was not available; the page body cannot be set');
}

// readEditor returns the body TinyMCE is currently showing, which is what a person
// sees in the editor - not what the hidden textarea happens to contain.
async function readEditor(page: Page): Promise<string> {
  await waitForEditor(page);
  return page.evaluate(() => {
    const w = window as any;
    const ed = w.tinymce?.get?.('html-editor') ?? w.tinymce?.activeEditor;
    const html = ed ? (ed.getContent() as string) : '';
    // Strip the markup so the assertion reads like the visible text.
    return html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
  });
}

test.describe('bookstack in a browser', () => {
  test.skip(!installed('bookstack'), 'bookstack is not installed');

  test('a person creates a book and a page through the UI, edits it, and reads it back', async ({
    browser,
  }) => {
    test.setTimeout(300_000);
    const host = hostOf('bookstack');
    const user = `ui-bs-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const context = await browser.newContext({ ignoreHTTPSErrors: true });
    const page = await context.newPage();
    await bookstackLogin(page, host, user, pass);

    const stamp = Date.now();
    const bookName = `ui-book-${stamp}`;
    const pageName = `ui-page-${stamp}`;
    const draft = `First draft written in a browser at ${stamp}.`;
    const edited = `Edited in a browser at ${stamp}.`;

    // --- create the book through the UI ----------------------------------
    await page.goto(`https://${host}/books`, { waitUntil: 'domcontentloaded' });
    // A signed-in user sees "Create New Book"; an anonymous one is at /login.
    expect(new URL(page.url()).pathname, 'the books view requires a real app session').not.toBe('/login');

    console.log('books view:', page.url());
    await page.getByRole('link', { name: /create new book/i }).first().click({ timeout: 20_000 });
    await page.waitForURL(/\/create-book/, { timeout: 20_000 });
    console.log('create-book form:', page.url());
    await page.locator('input[name="name"]').fill(bookName);
    // The description is a HIDDEN textarea that a rich-text widget drives, so
    // fill() on it waits for visibility forever and the test times out on a
    // working app. It is optional, so it is left alone: the name is the required
    // field and the one the assertions use.
    // noWaitAfter: BookStack answers the POST with a redirect that its own JS
    // follows, so Playwright's "wait for the navigation this click starts" never
    // resolves and the click hangs for the whole test timeout. Waiting explicitly
    // is both faster and honest about what the app does.
    await page.getByRole('button', { name: /save book/i }).click({ timeout: 20_000, noWaitAfter: true });
    await page.waitForURL(/\/books\/[^/]+$/, { timeout: 30_000 });
    await page.waitForTimeout(1000);
    expect(page.url(), 'saving a book lands on the book, not back on the form').not.toMatch(/\/create-book/);

    // --- add a page inside that book -------------------------------------
    console.log('book page:', page.url());
    await page.getByRole('link', { name: /new page/i }).first().click({ timeout: 20_000, noWaitAfter: true });
    // BookStack redirects "new page" straight into a draft editor at
    // /books/<slug>/draft/<id>, not to a /create-page form. That editor keeps a
    // poll open, so wait for the URL itself rather than the load event.
    await page.waitForURL(/\/draft\//, { timeout: 30_000, waitUntil: 'commit' });
    await page.waitForTimeout(1500);
    console.log('draft editor:', page.url());
    await page.locator('input[name="name"]').fill(pageName);
    // The editor is a contenteditable or a textarea depending on the editor used.
    // BookStack's editor is TinyMCE in an IFRAME (#html-editor_ifr), and the
    // `html` TEXTAREA is hidden behind it. Both facts matter:
    //   * fill() on the hidden textarea waits for visibility forever;
    //   * setting .value through the DOM does not reach the widget, so the save
    //     succeeds and stores an EMPTY page - a failure that looks like the app
    //     losing data.
    // Typing into TinyMCE's body is what the editor actually records.
    await writeInEditor(page, draft);
    // #save-button, not a name match: the editor's visible Save Page button has
    // that id, and its sibling #save-button-mobile has no accessible name at all,
    // so a name-based locator resolves to more than one element or to none.
    await page.locator('#save-button').click({ timeout: 20_000, noWaitAfter: true });
    // A new page's first save stays at /draft/<id>; the slug appears on a later
    // save. Accept either, because both mean the write went through.
    //
    // waitUntil 'commit' rather than the default 'load': the draft editor keeps a
    // poll open, so the load event never fires and a default waitForURL times out
    // AFTER the navigation has already happened.
    await page.waitForURL(/\/(page|draft)\//, { timeout: 30_000, waitUntil: 'commit' });
    await page.waitForTimeout(1500);

    // The saved page is what we were sent to, and it shows the text.
    const pageUrl = page.url();
    const bodyAfterSave = await page.locator('body').innerText();
    console.log('pageUrl:', pageUrl);
    console.log('draft present right after save:', bodyAfterSave.includes(draft));
    console.log('body excerpt:', bodyAfterSave.slice(0, 300).replace(/\n+/g, ' | '));
    expect(pageUrl, 'saving a page lands on the page or its draft').toMatch(/\/(page|draft)\//);
    // The creation banner appears before the page body paints, so assert with a
    // retry rather than on the first read of innerText.
    await expect(page.locator('body'), 'the draft is rendered').toContainText(draft, { timeout: 20_000 });

    // --- edit it: the editor must open WITH the existing text -------------
    await page.goto(pageUrl, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(800);
    // A SAVED page shows a read view with an Edit control; only the editor view
    // has TinyMCE. Checking for the textarea is not enough: the view page carries
    // a hidden one too, so that test passed while no editor was loaded and the
    // body read back empty. Check for the editor iframe instead.
    if (!(await page.locator('#html-editor_ifr').count())) {
      await page.getByRole('link', { name: /^edit$/i }).first().click({ timeout: 20_000, noWaitAfter: true });
      await page.waitForURL(/\/edit/, { timeout: 30_000, waitUntil: 'commit' });
    }
    await waitForEditor(page);
    await page.waitForTimeout(800);
    console.log('editor:', page.url());
    // The editor must open with what was saved. Read it from the widget, not the
    // hidden textarea, since that is what a person would see on screen.
    const editorText = await readEditor(page);
    expect(editorText, 'the editor opens with the text already written').toContain(draft);

    // Replace the body rather than appending: this is what an edit does.
    await writeInEditor(page, edited);
    await page.locator('#save-button').click({ timeout: 20_000, noWaitAfter: true });
    await page.waitForTimeout(3000);

    // --- a reload shows the edit, not the draft --------------------------
    // The first save left us on the draft; the second gives the page its slug, so
    // re-read the current URL rather than assuming the draft one is still right.
    const finalUrl = page.url().includes('/draft/') ? pageUrl.replace(/\/draft\/\d+$/, '') : page.url();
    await page.goto(finalUrl || pageUrl, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('body'), 'the edited text is served after a reload').toContainText(edited, {
      timeout: 20_000,
    });
    expect(
      await page.locator('body').innerText(),
      'the original draft was replaced, not appended',
    ).not.toContain(draft);

    // --- and the book lists the page -------------------------------------
    await page.goto(`https://${host}/books`, { waitUntil: 'domcontentloaded' });
    expect(await page.locator('body').innerText(), 'the book is on the shelf').toContain(bookName);

    await context.close();
  });
});
