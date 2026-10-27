import { readFile } from 'node:fs/promises';
import { chromium, type Page } from 'playwright';
import { setupSeerrViaApi } from '../api/seerr.js';
import { BootstrapConfig } from '../config.js';
import { http } from '../lib/http.js';

export async function setupSeerr(
  config: BootstrapConfig,
  sonarrApiKey: string,
  radarrApiKey: string,
  sonarrProfileId: number,
  radarrProfileId: number,
  jellyfinApiKey?: string,
): Promise<void> {
  const base = config.urls.seerr.replace(/\/$/, '');
  const statusResponse = await http('GET', `${base}/api/v1/status`);
  if (!statusResponse.ok) {
    throw new Error(`Seerr status check failed: HTTP ${statusResponse.status}`);
  }

  // A freshly started Seerr can answer /api/v1/status while its settings
  // endpoints and Jellyfin-backed session are still coming up. Retry the API
  // path before falling back to the UI: the fallback cannot drive the wizard
  // until Seerr is ready either, and the next Job attempt would only repeat the
  // same transient failure.
  let lastError: unknown;
  for (let attempt = 1; attempt <= 6; attempt++) {
    try {
      await setupSeerrViaApi(config, sonarrApiKey, radarrApiKey, sonarrProfileId, radarrProfileId);
      return;
    } catch (error) {
      lastError = error;
      console.log(
        `WARN: Seerr API setup attempt ${attempt}/6 failed (${error instanceof Error ? error.message : error})`,
      );
      if (attempt < 6) {
        await sleep(10_000);
      }
    }
  }
  console.log(
    `WARN: Seerr API setup failed (${lastError instanceof Error ? lastError.message : lastError}); trying Playwright`,
  );

  if (process.env.SKIP_SEERR_UI === 'true') {
    console.log('==> Seerr: SKIP_SEERR_UI=true, skipping wizard');
    return;
  }

  let apiKey = jellyfinApiKey;
  if (!apiKey) {
    try {
      apiKey = (await readFile(config.jellyfinApiKeyFile, 'utf8')).trim();
    } catch {
      apiKey = undefined;
    }
  }

  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();

  try {
    await page.goto(base, { waitUntil: 'domcontentloaded', timeout: 120_000 });
    await completeWizard(page, config, sonarrApiKey, radarrApiKey, apiKey);
    console.log('==> Seerr: wizard completed');
  } catch (error) {
    await page.screenshot({
      path: `${config.artifactsDir}/seerr-failure.png`,
      fullPage: true,
    });
    console.log(
      `WARN: Seerr wizard automation failed (${error instanceof Error ? error.message : error}); complete setup manually`,
    );
  } finally {
    await browser.close();
  }
}

async function completeWizard(
  page: Page,
  config: BootstrapConfig,
  sonarrApiKey: string,
  radarrApiKey: string,
  jellyfinApiKey?: string,
): Promise<void> {
  await clickIfVisible(page, 'button:has-text("Get Started")', 10_000);
  await clickIfVisible(page, 'button:has-text("Jellyfin")', 10_000);
  await clickIfVisible(page, 'button:has-text("Continue")', 10_000);

  await page.locator('input[name="hostname"], input[placeholder*="URL"], input[type="url"]').first()
    .fill(config.urls.jellyfin.replace(/\/$/, ''));
  if (jellyfinApiKey) {
    const apiKeyInput = page.locator('input[name="jellyfinApiKey"], input[placeholder*="API"]');
    if (await apiKeyInput.first().isVisible({ timeout: 3_000 }).catch(() => false)) {
      await apiKeyInput.first().fill(jellyfinApiKey);
    }
  } else {
    await page.locator('input[name="username"], input[type="text"]').first().fill(config.jellyfinAdminUser);
    await page.locator('input[type="password"]').first().fill(config.jellyfinAdminPassword);
  }
  await clickIfVisible(page, 'button:has-text("Continue")', 15_000);
  await clickIfVisible(page, 'button:has-text("Sign in")', 10_000);

  await configureArrServer(page, 'Sonarr', config.urls.sonarr, sonarrApiKey, '/media/tv');
  await configureArrServer(page, 'Radarr', config.urls.radarr, radarrApiKey, '/media/movies');

  if (config.seerrAutoApprove) {
    const autoApprove = page.getByLabel(/auto.?approve/i);
    if (await autoApprove.isVisible({ timeout: 3_000 }).catch(() => false)) {
      await autoApprove.check();
    }
  }

  await clickIfVisible(page, 'button:has-text("Finish")', 10_000);
  await clickIfVisible(page, 'button:has-text("Start using")', 10_000);
}

async function configureArrServer(
  page: Page,
  name: 'Sonarr' | 'Radarr',
  serverUrl: string,
  apiKey: string,
  rootFolder: string,
): Promise<void> {
  await clickIfVisible(page, `button:has-text("${name}")`, 10_000);
  await page.locator('input[name="hostname"], input[placeholder*="URL"]').first().fill(serverUrl);
  await page.locator('input[name="apiKey"], input[placeholder*="API"]').first().fill(apiKey);
  await clickIfVisible(page, 'button:has-text("Test")', 10_000);
  await clickIfVisible(page, 'button:has-text("Continue")', 10_000);

  const rootSelect = page.locator('select').filter({ hasText: rootFolder });
  if (await rootSelect.first().isVisible({ timeout: 5_000 }).catch(() => false)) {
    await rootSelect.first().selectOption({ label: rootFolder }).catch(() => undefined);
  }
  await clickIfVisible(page, 'button:has-text("Continue")', 10_000);
  console.log(`==> Seerr: configured ${name}`);
}

async function clickIfVisible(page: Page, selector: string, timeout: number): Promise<void> {
  const locator = page.locator(selector).first();
  if (await locator.isVisible({ timeout }).catch(() => false)) {
    if (await locator.isEnabled().catch(() => false)) {
      await locator.click();
    }
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
