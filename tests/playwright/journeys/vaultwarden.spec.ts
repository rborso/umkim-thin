import { test, expect, Page } from '@playwright/test';
import {
  PORTAL_PASS,
  PORTAL_USER,
  hostOf,
  installed,
  portalSession,
} from '../lib/session';

// Vaultwarden is SSO-only: Authelia/LDAP supplies the user identity, while the
// master password and vault keys are created and used in the client. The
// second session proves the client-side key survives re-authentication.
test.describe('vaultwarden', () => {
  test.skip(!installed('vaultwarden'), 'vaultwarden is not installed');
  test.skip(!PORTAL_USER || !PORTAL_PASS, 'authelia portal credentials are not set');
  const host = hostOf('vaultwarden');

  test('uses Authelia SSO and a client-side master password', async ({ browser }) => {
    test.setTimeout(180_000);
    const ctx = await portalSession({ username: PORTAL_USER, password: PORTAL_PASS });
    const state = await ctx.storageState();
    await ctx.dispose();

    const suffix = `${Date.now()}-${Math.floor(Math.random() * 1_000_000)}`;
    const email = `${PORTAL_USER}@ci.local`;
    // Stable for the run so a Playwright retry can unlock an account created by
    // the previous attempt. The vault item name stays unique per attempt.
    const masterPassword = `Vw-SSO-${PORTAL_USER}-Aa1!`;
    const itemName = `CI SSO note ${suffix}`;

    const contextFor = () =>
      browser.newContext({
        storageState: state,
        ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
        locale: 'en-US',
      });

    const ssoLogin = async (page: Page) => {
      await page.goto(`https://${host}/`, { waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(2500);
      await page.locator('#bit-form-field-1').fill(email);
      await page.getByRole('button', { name: 'Use single sign-on' }).click();
    };

    const dismissOnboarding = async (page: Page) => {
      const addLater = page.getByRole('button', { name: 'Add it later' });
      const add = page.getByRole('button', { name: 'Add', exact: true });
      await expect(addLater.or(add)).toBeVisible({ timeout: 30_000 });
      if (await addLater.isVisible().catch(() => false)) {
        await addLater.click();
        const skipWeb = page.getByRole('link', { name: 'Skip to web app' });
        await expect(skipWeb).toBeVisible({ timeout: 15_000 });
        await skipWeb.click();
      }
      await page.waitForTimeout(1000);
      const tourSkip = page.getByRole('button', { name: 'Skip', exact: true });
      if (await tourSkip.count()) await tourSkip.click();
    };

    const first = await contextFor();
    const page = await first.newPage();
    try {
      await ssoLogin(page);

      // A fresh account lands on the client-side master-password setup. A
      // retry of the same test may already have created the account, in which
      // case SSO lands on the lock screen and the stable password unlocks it.
      const join = page.getByRole('heading', { name: 'Join organization' });
      const lock = page.getByRole('heading', { name: 'Your vault is locked' });
      await expect(join.or(lock)).toBeVisible({ timeout: 30_000 });
      if (await join.isVisible().catch(() => false)) {
        await page.locator('#input-password-form_new-password').fill(masterPassword);
        await page.locator('#input-password-form_new-password-confirm').fill(masterPassword);
        await page.getByRole('button', { name: 'Create account' }).click();
      } else {
        await page.locator('#bit-form-field-1').fill(masterPassword);
        await page.getByRole('button', { name: 'Unlock' }).click();
      }

      await page.waitForTimeout(1000);
      await dismissOnboarding(page);

      await expect(page.getByRole('button', { name: 'Add', exact: true })).toBeVisible({
        timeout: 30_000,
      });
      await page.getByRole('button', { name: 'Add', exact: true }).click();
      await page.getByRole('menuitem', { name: 'Note' }).click();

      const dialog = page.locator('cdk-dialog-container');
      await expect(dialog).toBeVisible({ timeout: 15_000 });
      await dialog.getByLabel('Item name').fill(itemName);
      await dialog.getByLabel('Notes').fill('Created by the Vaultwarden SSO journey.');
      await dialog.getByRole('button', { name: 'Save', exact: true }).click();

      await expect(dialog).toContainText('View note', { timeout: 15_000 });
      await expect(dialog.getByRole('heading', { name: itemName })).toBeVisible();
      await dialog.getByRole('button', { name: 'Close' }).click();
      await expect(dialog).toBeHidden({ timeout: 15_000 });
      await expect(page.getByText(itemName, { exact: true })).toBeVisible({
        timeout: 15_000,
      });
    } finally {
      await first.close();
    }

    // A new browser session has no Vaultwarden session, but the same Authelia
    // identity. SSO gets us to the lock screen; the master password decrypts
    // the item created above.
    const second = await contextFor();
    const page2 = await second.newPage();
    try {
      await ssoLogin(page2);
      await expect(page2.getByRole('heading', { name: 'Your vault is locked' })).toBeVisible({
        timeout: 30_000,
      });
      await page2.locator('#bit-form-field-1').fill(masterPassword);
      await page2.getByRole('button', { name: 'Unlock' }).click();
      await page2.waitForTimeout(1000);
      await dismissOnboarding(page2);
      await expect(page2.getByRole('button', { name: 'Add', exact: true })).toBeVisible({
        timeout: 30_000,
      });
      await expect(page2.getByText(itemName, { exact: true })).toBeVisible({
        timeout: 15_000,
      });
    } finally {
      await second.close();
    }
  });
});
