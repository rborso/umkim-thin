import { test, expect } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { AUTH_HOST, CONSOLE_HOST, consoleSession, portalSession } from '../lib/session';

// Platform user journey: an operator creates an invitation in the console, the
// invitee opens the public invitation page, chooses a password, and then signs
// in through Authelia like any other tenant. This exercises the full invitation
// path (console API -> invite-signup -> console redeem -> directory -> portal)
// rather than only creating the CI fixture account.
test.describe('platform invitation', () => {
  test.skip(!AUTH_HOST || !CONSOLE_HOST, 'set UMKIM_THIN_ZONE / console host');

  test('an invited tenant accepts the public invitation and signs in', async ({ browser }) => {
    test.setTimeout(120_000);
    const suffix = `${Date.now()}-${randomBytes(3).toString('hex')}`;
    const username = `ci-invite-${suffix}`;
    const password = randomBytes(18).toString('hex');

    const admin = await consoleSession();
    let link = '';
    try {
      const created = await admin.post(`https://${CONSOLE_HOST}/api/invites`, {
        data: {
          displayName: username,
          email: `${username}@ci.local`,
          group: 'default-tenant',
        },
      });
      expect(created.status(), `create invitation -> ${created.status()}`).toBe(200);
      const body = await created.json();
      const token: string = body?.invite?.token ?? '';
      link = body?.link ?? '';
      expect(token, 'invitation token').toBeTruthy();
      expect(link, 'invitation link').toContain('/invite?token=');

      const context = await browser.newContext({ ignoreHTTPSErrors: true });
      const page = await context.newPage();
      await page.goto(link);
      await expect(page.getByRole('heading', { name: 'Accept invitation' })).toBeVisible();
      await page.fill('#username', username);
      await page.fill('#displayName', username);
      await page.fill('#password', password);
      await page.fill('#confirm', password);
      await page.click('#submit');
      await expect(page.locator('#msg')).toContainText(`Account created: ${username}`, {
        timeout: 30_000,
      });

      // The real proof: the account created by the public page can authenticate.
      const session = await portalSession({ username, password });
      await session.dispose();
      await context.close();
    } finally {
      await admin
        .delete(`https://${CONSOLE_HOST}/api/directory/users/${username}`, {
          headers: { Origin: `https://${CONSOLE_HOST}` },
        })
        .catch(() => undefined);
      await admin.dispose();
    }
  });
});
