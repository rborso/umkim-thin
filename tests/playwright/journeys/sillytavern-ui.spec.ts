import { test, expect, Page } from '@playwright/test';
import { AUTH_HOST } from '../lib/session';

// SillyTavern ÚTK: the browser journey.
//
// Everything below this test was verified with node scripts talking to the API. That
// proves the wiring, not the product: SillyTavern's own UI is what a person uses, and
// it has its own login, its own extension loading and its own generation path.
//
// The host is fixed rather than read from UMKIM_THIN_APPS because this app is a
// custom catalog entry on one lab host, not part of the platform's catalog.
const HOST = process.env.UMKIM_THIN_ST_HOST || 'st.gpu.ha-dev.umkim.com';
const ALKOTO = process.env.UMKIM_THIN_ALKOTO_HOST || 'alkoto.gpu.ha-dev.umkim.com';

// stLogin signs in through the portal and waits for SillyTavern itself to finish
// booting.
//
// The app renders an "Initializing…" placeholder and then swaps in the UI, so
// waiting for the URL is not enough: an assertion taken in that window sees a bare
// body. Wait for a control that only exists after the client-side boot.
async function stLogin(page: Page): Promise<void> {
  await page.goto(`https://${HOST}/`, { waitUntil: 'domcontentloaded' });

  // The portal is in front of the app; fill its form when it appears.
  if (page.url().includes(AUTH_HOST)) {
    const u = page.locator('#username-textfield, input[autocomplete="username"]').first();
    await u.waitFor({ state: 'visible', timeout: 25_000 });
    // The SPA mounts the form before it attaches handlers; typing too early drops
    // the keystrokes.
    await page.waitForTimeout(1200);
    await u.fill(process.env.UMKIM_THIN_USER || 'admin');
    const p = page.locator('#password-textfield, input[autocomplete="current-password"]').first();
    await p.fill(process.env.UMKIM_THIN_PASSWORD || '');
    await p.press('Enter');
    await page.waitForURL((x) => x.hostname === HOST, { timeout: 45_000 }).catch(() => undefined);
  }

  // The client bundle replaces the "Initializing…" placeholder once extensions and
  // characters have loaded. #send_but is NOT the right signal: it exists in the DOM
  // from the start but carries `displayNone` until a chat is open, so waiting for
  // it to be visible times out on a perfectly loaded page.
  //
  // The character list is the honest signal — it is populated by the pack and only
  // rendered after boot.
  await page
    .locator('#rm_print_characters_block .character_select, #rm_print_characters_block .avatar')
    .first()
    .waitFor({ state: 'attached', timeout: 60_000 });
}

test.describe('sillytavern UI', () => {
  test.skip(!process.env.UMKIM_THIN_PASSWORD, 'needs UMKIM_THIN_USER/PASSWORD');

  test('a person signs in, sees the ÚTK pack, and the UI is usable', async ({ browser }) => {
    test.setTimeout(180_000);
    const context = await browser.newContext({
      ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
    });
    const page = await context.newPage();
    await stLogin(page);

    // Assert on what the UI RENDERS rather than on an internal endpoint: this build
    // answers POST /api/characters/all and 404s the GET, and a UI test that depends
    // on internal API shapes is testing the wrong thing.
    //
    // The character list is the pack's most visible effect: the KM card appears in
    // the sidebar only if the file landed where SillyTavern looks.
    const listText = await page.locator('#rm_print_characters_block').innerText();
    expect(listText, 'the ÚTK KM card is listed in the UI').toMatch(/ynevi/i);

    // World Info books: the UI exposes them in the same panel, and the pack ships
    // dozens. Counting them from the DOM keeps this a UI test.
    const worldsText = await page.evaluate(async () => {
      // The world list comes from the same boot payload the UI used, so read the
      // visible selector rather than calling the endpoint again.
      const sel = document.querySelector('#world_info') as HTMLSelectElement | null;
      return sel ? Array.from(sel.options).map(o => o.textContent || '') : [];
    });
    if (worldsText.length) {
      expect(
        worldsText.filter((n) => /^UTK_/i.test(n)).length,
        `the ÚTK world books are offered (${worldsText.length} total)`,
      ).toBeGreaterThanOrEqual(5);
    }

    // The MCP client extension is what lets the model call the rules engine, and it
    // is user-installed state, not chart state: a deploy that forgets it leaves a
    // chat UI that cannot roll dice.
    //
    // /api/extensions/discover is the endpoint the UI itself uses, and third-party
    // extensions come back with type "global" and a "third-party/<name>" name — not
    // under /api/extensions/settings, which 404s in this build.
    const ext = await page.evaluate(async () => {
      const r = await fetch('/api/extensions/discover', { credentials: 'same-origin' });
      const list = await r.json().catch(() => []);
      return {
        status: r.status,
        globals: (Array.isArray(list) ? list : []).filter((e: any) => e.type === 'global').map((e: any) => e.name),
      };
    });
    expect(ext.status, 'extension discovery answers').toBe(200);
    expect(
      ext.globals.some((n) => /MCP-Client/i.test(n)),
      `the MCP client extension is discovered (saw ${JSON.stringify(ext.globals)})`,
    ).toBe(true);

    await context.close();
  });

  test('the character builder serves its UI behind the portal', async ({ browser }) => {
    test.setTimeout(120_000);
    const context = await browser.newContext({
      ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
    });
    const page = await context.newPage();
    await page.goto(`https://${ALKOTO}/`, { waitUntil: 'domcontentloaded' });
    if (page.url().includes(AUTH_HOST)) {
      const u = page.locator('#username-textfield, input[autocomplete="username"]').first();
      await u.waitFor({ state: 'visible', timeout: 25_000 });
      await page.waitForTimeout(1200);
      await u.fill(process.env.UMKIM_THIN_USER || 'admin');
      const p = page.locator('#password-textfield, input[autocomplete="current-password"]').first();
      await p.fill(process.env.UMKIM_THIN_PASSWORD || '');
      await p.press('Enter');
      await page.waitForURL((x) => x.hostname === ALKOTO, { timeout: 45_000 }).catch(() => undefined);
    }
    await expect(page.locator('body'), 'the builder renders its page').toContainText(
      /karakteralkot/i,
      { timeout: 30_000 },
    );
    await context.close();
  });
});
