import { test, expect } from '@playwright/test';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { installed, hostOf, AUTH_HOST, USER, PASS } from '../lib/session';

// Actual Budget client journey: create a real budget through the sync server,
// add an account and a transaction, sync it, then download the same budget into
// a second client directory and verify the transaction survived the round trip.
// The OIDC session token is captured from the browser login and handed to the
// official @actual-app/api client.
test.describe('actualbudget client', () => {
  test.skip(!installed('actualbudget'), 'actualbudget is not installed');
  test.skip(!PASS, 'UMKIM_THIN_PASSWORD not set');

  const host = hostOf('actualbudget');
  const base = `https://${host}`;

  test('creates, syncs and downloads a budget', async ({ browser }) => {
    test.setTimeout(180_000);

    let sessionToken = '';
    const capture = (url: string) => {
      const match = url.match(/[?&]token=([^&#]+)/);
      if (match) sessionToken = decodeURIComponent(match[1]);
    };

    const readTokenFromPage = async (p: any): Promise<string> =>
      p
        .evaluate(async () => {
          const databases = await (indexedDB as any).databases?.();
          if (databases && !databases.some((d: any) => d.name === 'actual')) return '';
          const db = await new Promise<IDBDatabase>((resolve, reject) => {
            const request = indexedDB.open('actual');
            request.onsuccess = () => resolve(request.result);
            request.onerror = () => reject(request.error);
          });
          return new Promise<string>((resolve, reject) => {
            const tx = db.transaction(['asyncStorage'], 'readonly');
            const request = tx.objectStore('asyncStorage').get('user-token');
            request.onsuccess = () => resolve(String(request.result ?? ''));
            request.onerror = () => reject(request.error);
          });
        })
        .catch(() => '');

    const loginActual = async () => {
      const context = await browser.newContext({
        ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
      });
      const page = await context.newPage();
      page.on('framenavigated', (frame) => capture(frame.url()));
      page.on('response', (response) => {
        if (response.url().includes('/openid/callback')) {
          capture(response.headers()['location'] ?? '');
        }
      });
      await page.goto(base);
      await page.waitForTimeout(3000);

      // A fresh browser context has no server URL in Actual's IndexedDB and
      // lands on the welcome page. Point it at this instance first.
      const connectServer = page.getByRole('button', { name: 'Connect to a sync server' });
      if (await connectServer.count()) {
        await connectServer.click();
        const url = page.locator('input[placeholder="https://example.com"]');
        await url.waitFor({ state: 'visible', timeout: 10_000 });
        await url.fill(base);
        await page.getByRole('button', { name: 'Connect', exact: true }).click();
        await page.waitForTimeout(5000);
      }

      for (let attempt = 0; attempt < 4; attempt++) {
        const openid = page.getByRole('button', { name: /OpenID/i });
        if (await openid.count()) {
          await openid.first().click();
          await page.waitForTimeout(3000);
        }
        if (page.url().includes(AUTH_HOST)) {
          const username = page
            .locator('#username-textfield, input[autocomplete="username"]')
            .first();
          await username.waitFor({ state: 'visible', timeout: 10_000 }).catch(() => undefined);
          if (await username.count()) {
            await username.fill(USER);
            const password = page
              .locator('#password-textfield, input[autocomplete="current-password"]')
              .first();
            await password.fill(PASS);
            await password.press('Enter');
            await page.waitForTimeout(5000);
          }
        }
        const token = await readTokenFromPage(page);
        if (token) return { context, page, token };
        await page.waitForTimeout(3000);
      }
      return { context, page, token: '' };
    };

    const { context, page, token: loginToken } = await loginActual();
    sessionToken = loginToken;
    // The Actual web client can spend a while on "Initializing the connection
    // to the local database…" before it writes the token. Poll; do not reload,
    // which would restart that initialization.
    sessionToken = await readTokenFromPage(page);
    for (let i = 0; i < 60 && !sessionToken; i++) {
      await page.waitForTimeout(1000);
      sessionToken = await readTokenFromPage(page);
    }
    expect(sessionToken, 'the OIDC login handed the client a sync token').toBeTruthy();
    const cookies = await context.cookies(base);
    const cookieHeader = cookies.map((c) => `${c.name}=${c.value}`).join('; ');
    expect(cookieHeader, 'the Authelia session cookie').toContain('authelia_session=');
    await context.close();

    // The Node API client has no browser cookie jar. Add the portal session to
    // its requests so the forward-auth in front of Actual lets the sync calls
    // through; the client's own server token still authenticates the budget.
    const originalFetch = globalThis.fetch;
    globalThis.fetch = ((input: any, init: any = {}) => {
      const headers = new Headers(init.headers ?? {});
      if (!headers.has('cookie')) headers.set('cookie', cookieHeader);
      return originalFetch(input, { ...init, headers });
    }) as typeof fetch;

    // Import after the fetch shim: the API package may capture the platform
    // fetch implementation at module load time.
    const actual: typeof import('@actual-app/api') = await import('@actual-app/api');
    const dataDir = mkdtempSync(join(tmpdir(), 'umkim-actual-a-'));
    const dataDir2 = mkdtempSync(join(tmpdir(), 'umkim-actual-b-'));
    const marker = `ci-tx-${Date.now()}`;
    const accountName = `CI Account ${marker}`;
    const budgetName = `CI Budget ${marker}`;
    let budget: any;
    let api: any;
    let groupId = '';
    let cloudFileId = '';

    try {
      // init()'s returned send() is the supported path for handlers that the
      // public API does not re-export (create-budget).
      api = await actual.init({ dataDir, serverURL: base, sessionToken });
      await api.send('create-budget', { budgetName });

      const budgets = await actual.getBudgets();
      budget = budgets.find((b) => b.name === budgetName);
      expect(budget, 'the budget was created on the sync server').toBeTruthy();
      expect(budget.cloudFileId, 'the budget has a sync id').toBeTruthy();

      const accountId = await actual.createAccount(
        { name: accountName, type: 'checking' } as any,
        0,
      );
      await actual.addTransactions(accountId, [
        {
          date: '2026-01-15',
          amount: 1234,
          payee_name: marker,
          notes: marker,
        } as any,
      ]);

      // create-budget uploads best-effort and swallows upload errors. Make the
      // upload explicit and verify the server has the file before downloading
      // it into the second client.
      const uploaded = await api.send('upload-budget');
      expect(uploaded.error, 'the budget uploaded to the sync server').toBeUndefined();
      const remote = await api.send('get-remote-files');
      const remoteFile = (remote ?? []).find(
        (f: any) =>
          f.name === budgetName ||
          f.fileId === budget.cloudFileId ||
          f.cloudFileId === budget.cloudFileId,
      );
      expect(remoteFile, 'the sync server lists the uploaded budget').toBeTruthy();
      groupId = remoteFile.groupId ?? budget.groupId;
      cloudFileId = remoteFile.fileId ?? remoteFile.cloudFileId ?? budget.cloudFileId;
      expect(groupId, 'the uploaded budget has a group id').toBeTruthy();
      expect(cloudFileId, 'the uploaded budget has a cloud file id').toBeTruthy();
      await actual.sync();

      const transactions = await actual.getTransactions(accountId, '2026-01-01', '2026-01-31');
      expect(
        transactions.some((t) => t.notes === marker),
        'the transaction is in the first client',
      ).toBeTruthy();

      await actual.shutdown();
      api = await actual.init({ dataDir: dataDir2, serverURL: base, sessionToken });
      await actual.downloadBudget(groupId);

      const accounts = await actual.getAccounts();
      const downloaded = accounts.find((a) => a.name === accountName);
      expect(downloaded, 'the account came back from the server').toBeTruthy();
      const downloadedTransactions = await actual.getTransactions(
        downloaded!.id,
        '2026-01-01',
        '2026-01-31',
      );
      expect(
        downloadedTransactions.some((t) => t.notes === marker),
        'the transaction survived sync and download',
      ).toBeTruthy();
    } finally {
      if (budget && api && cloudFileId) {
        await api
          .send('delete-budget', { id: budget.id, cloudFileId })
          .catch(() => undefined);
      }
      await actual.shutdown().catch(() => undefined);
      globalThis.fetch = originalFetch;
      rmSync(dataDir, { recursive: true, force: true });
      rmSync(dataDir2, { recursive: true, force: true });
    }
  });
});
