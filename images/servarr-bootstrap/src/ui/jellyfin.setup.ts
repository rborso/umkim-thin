import { mkdir, writeFile } from 'node:fs/promises';
import { chromium, type Page } from 'playwright';
import { BootstrapConfig } from '../config.js';
import { http, httpJson } from '../lib/http.js';
import { retryAsync } from '../lib/retry.js';

interface JellyfinPublicInfo {
  StartupWizardCompleted?: boolean;
  Id?: string;
}

export async function setupJellyfin(config: BootstrapConfig): Promise<string | undefined> {
  const base = config.urls.jellyfin.replace(/\/$/, '');
  await waitForJellyfinReady(base);

  if (!(await canAuthenticate(base, config))) {
    await ensureJellyfinWizardAndAdmin(base, config);
  } else {
    console.log('==> Jellyfin: admin login already works');
  }

  await retryAsync(() => ensureJellyfinLibraries(config), {
    attempts: 5,
    delayMs: 5_000,
  });

  await retryAsync(() => setLibraryScanInterval(base, config), {
    attempts: 3,
    delayMs: 5_000,
  }).catch((error) => {
    console.log(`WARN: Jellyfin: could not set library scan interval (${error instanceof Error ? error.message : error})`);
  });

  await ensureWizardMarkedComplete(base);

  try {
    return await retryAsync(() => createApiKeyViaApi(config), {
      attempts: 3,
      delayMs: 5_000,
    });
  } catch (error) {
    console.log(`WARN: Jellyfin API key setup skipped (${error instanceof Error ? error.message : error})`);
    return undefined;
  }
}

const JELLYFIN_LIBRARIES = [
  { name: 'Movies', collectionType: 'movies', path: '/media/movies' },
  { name: 'Series', collectionType: 'tvshows', path: '/media/tv' },
] as const;

interface JellyfinVirtualFolder {
  Name?: string;
  Locations?: string[];
  ItemId?: string;
  LibraryOptions?: Record<string, unknown> & {
    EnableRealtimeMonitor?: boolean;
    PathInfos?: Array<{ Path: string }>;
  };
}

function jellyfinLibraryOptions(path: string): Record<string, unknown> {
  return {
    PathInfos: [{ Path: path }],
    EnableRealtimeMonitor: true,
  };
}

async function waitForJellyfinReady(base: string): Promise<void> {
  await retryAsync(
    async () => {
      const info = await httpJson<JellyfinPublicInfo>('GET', `${base}/System/Info/Public`);
      if (!info.Id) {
        throw new Error('Jellyfin public info missing server id');
      }
    },
    { attempts: 30, delayMs: 5_000 },
  );
  // Fresh PVC installs can return HTTP 500 on Startup/User for ~1–2 minutes.
  await sleep(20_000);
  console.log('==> Jellyfin: server ready');
}

async function canAuthenticate(base: string, config: BootstrapConfig): Promise<boolean> {
  const response = await http('POST', `${base}/Users/AuthenticateByName`, {
    body: {
      Username: config.jellyfinAdminUser,
      Pw: config.jellyfinAdminPassword,
    },
    headers: jellyfinAuthHeaders(),
  });
  return response.ok;
}

async function ensureJellyfinWizardAndAdmin(base: string, config: BootstrapConfig): Promise<void> {
  const info = await httpJson<JellyfinPublicInfo>('GET', `${base}/System/Info/Public`);
  if (info.StartupWizardCompleted) {
    const users = await httpJson<unknown[]>('GET', `${base}/Users/Public`);
    if (users.length === 0) {
      throw new Error(
        'Jellyfin wizard completed without users — run scripts/04d-reset-jellyfin-wizard-if-broken.sh and retry bootstrap',
      );
    }
    throw new Error('Jellyfin wizard is complete but bootstrap credentials do not authenticate');
  }

  try {
    await retryAsync(() => completeWizardViaApi(base, config), {
      attempts: 12,
      delayMs: 10_000,
    });
    await waitForJellyfinWizardComplete(base);
  } catch (error) {
    if (await canAuthenticate(base, config)) {
      console.log(
        `WARN: Jellyfin startup API reported failure (${error instanceof Error ? error.message : error}) but admin login works`,
      );
    } else {
      console.log(
        `WARN: Jellyfin startup API failed (${error instanceof Error ? error.message : error}); trying Playwright`,
      );
      await completeWizardViaPlaywright(config);
    }
    // The wizard can return before Jellyfin has committed the local admin.
    // Give it a short window instead of failing the whole bootstrap Job.
    await retryAsync(
      async () => {
        if (!(await canAuthenticate(base, config))) {
          throw new Error('Jellyfin admin login not ready yet');
        }
      },
      { attempts: 12, delayMs: 5_000 },
    ).catch(() => {
      throw new Error('Jellyfin Playwright wizard finished but admin login still fails');
    });
    const afterPlaywright = await httpJson<JellyfinPublicInfo>('GET', `${base}/System/Info/Public`);
    if (!afterPlaywright.StartupWizardCompleted) {
      await completeWizardConfiguration(base, config);
      await completeWizardFinalize(base);
      await waitForJellyfinWizardComplete(base, 120_000);
    }
  }

  await retryAsync(() => jellyfinAuthToken(base, config), {
    attempts: 5,
    delayMs: 3_000,
  });
  console.log('==> Jellyfin: admin user verified');
}

async function ensureJellyfinLibraries(config: BootstrapConfig): Promise<void> {
  const base = config.urls.jellyfin.replace(/\/$/, '');
  const token = await jellyfinAuthToken(base, config);
  const existing = await getJellyfinVirtualFolders(base, token);

  for (const library of JELLYFIN_LIBRARIES) {
    const match = findJellyfinLibrary(existing, library);
    if (match) {
      console.log(`==> Jellyfin: library "${library.name}" already present`);
      continue;
    }

    const response = await http(
      'POST',
      `${base}/Library/VirtualFolders?name=${encodeURIComponent(library.name)}&collectionType=${library.collectionType}&paths=${encodeURIComponent(library.path)}&refreshLibrary=true`,
      {
        headers: {
          'X-Emby-Token': token,
          'Content-Type': 'application/json',
        },
        body: {
          LibraryOptions: jellyfinLibraryOptions(library.path),
        },
      },
    );
    if (!response.ok && response.status !== 204) {
      throw new Error(`Jellyfin library "${library.name}" failed: HTTP ${response.status}`);
    }
    console.log(`==> Jellyfin: added library "${library.name}" at ${library.path}`);
  }

  await ensureJellyfinLibraryRealtimeMonitor(base, token);
}

async function getJellyfinVirtualFolders(
  base: string,
  token: string,
): Promise<JellyfinVirtualFolder[]> {
  return httpJson<JellyfinVirtualFolder[]>('GET', `${base}/Library/VirtualFolders`, {
    headers: { 'X-Emby-Token': token },
  });
}

function findJellyfinLibrary(
  libraries: JellyfinVirtualFolder[],
  library: (typeof JELLYFIN_LIBRARIES)[number],
): JellyfinVirtualFolder | undefined {
  return libraries.find(
    (item) =>
      item.Name === library.name ||
      item.Locations?.some((location) => location.replace(/\/$/, '') === library.path),
  );
}

async function ensureJellyfinLibraryRealtimeMonitor(base: string, token: string): Promise<void> {
  const existing = await getJellyfinVirtualFolders(base, token);

  for (const library of JELLYFIN_LIBRARIES) {
    const match = findJellyfinLibrary(existing, library);
    if (!match) {
      throw new Error(`Jellyfin library "${library.name}" missing after setup`);
    }
    if (!match.ItemId) {
      // Renaming a virtual folder can briefly leave its ItemId unset until
      // Jellyfin rescans or restarts. Do not fail the whole bootstrap for a
      // cosmetic monitoring option; the library itself exists at the path.
      console.log(
        `WARN: Jellyfin: library "${library.name}" has no ItemId yet; skipping real-time monitor`,
      );
      continue;
    }

    const currentOptions = match.LibraryOptions ?? {};
    if (currentOptions.EnableRealtimeMonitor === true) {
      console.log(`==> Jellyfin: library "${library.name}" real-time monitor enabled`);
      continue;
    }

    const libraryOptions = {
      ...currentOptions,
      ...jellyfinLibraryOptions(library.path),
    };
    const response = await http('POST', `${base}/Library/VirtualFolders/LibraryOptions`, {
      headers: {
        'X-Emby-Token': token,
        'Content-Type': 'application/json',
      },
      body: {
        Id: match.ItemId,
        LibraryOptions: libraryOptions,
      },
    });
    if (!response.ok && response.status !== 204) {
      const body = await response.text();
      throw new Error(
        `Jellyfin library "${library.name}" real-time monitor failed: HTTP ${response.status} ${body}`,
      );
    }
    console.log(`==> Jellyfin: enabled real-time monitor for "${library.name}"`);
  }
}

interface JellyfinScheduledTask {
  Id?: string;
  Key?: string;
  Triggers?: Array<{ Type?: string; IntervalTicks?: number }>;
}

// setLibraryScanInterval shortens Jellyfin's scheduled library scan. The default
// is every 12 hours, so a freshly downloaded movie can stay invisible to Jellyfin
// (and therefore to Seerr) for hours. NFS does not deliver the inotify events
// that real-time monitoring relies on, so a shorter interval is the reliable fix.
async function setLibraryScanInterval(base: string, config: BootstrapConfig): Promise<void> {
  const token = await jellyfinAuthToken(base, config);
  const tasks = await httpJson<JellyfinScheduledTask[]>('GET', `${base}/ScheduledTasks`, {
    headers: { 'X-Emby-Token': token },
  });
  const task = tasks.find((t) => t.Key === 'RefreshLibrary');
  if (!task?.Id) {
    return;
  }
  // .NET TimeSpan ticks (100 ns).
  const wanted = config.jellyfinLibraryScanMinutes * 60 * 10_000_000;
  if (task.Triggers?.some((trigger) => trigger.Type === 'IntervalTrigger' && trigger.IntervalTicks === wanted)) {
    return;
  }
  await httpJson('POST', `${base}/ScheduledTasks/${task.Id}/Triggers`, {
    headers: { 'X-Emby-Token': token, 'Content-Type': 'application/json' },
    body: [{ Type: 'IntervalTrigger', IntervalTicks: wanted }],
  });
  console.log(`==> Jellyfin: library scan interval set to ${config.jellyfinLibraryScanMinutes}m`);
}

function jellyfinAuthHeaders(): Record<string, string> {
  return {
    'X-Emby-Authorization':
      'MediaBrowser Client="servarr-bootstrap", Device="bootstrap", DeviceId="bootstrap-job", Version="1.0.0"',
    'Content-Type': 'application/json',
  };
}

async function jellyfinAuthToken(base: string, config: BootstrapConfig): Promise<string> {
  const auth = await httpJson<{ AccessToken?: string }>('POST', `${base}/Users/AuthenticateByName`, {
    body: {
      Username: config.jellyfinAdminUser,
      Pw: config.jellyfinAdminPassword,
    },
    headers: jellyfinAuthHeaders(),
  });
  if (!auth.AccessToken) {
    throw new Error('Jellyfin auth failed; cannot configure libraries');
  }
  return auth.AccessToken;
}

export async function waitForJellyfinWizardComplete(
  base: string,
  timeoutMs = 60_000,
): Promise<void> {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    const info = await httpJson<JellyfinPublicInfo>('GET', `${base}/System/Info/Public`);
    if (info.StartupWizardCompleted) {
      console.log('==> Jellyfin: startup wizard confirmed complete');
      return;
    }
    await sleep(3_000);
  }
  throw new Error('Timed out waiting for Jellyfin startup wizard to complete');
}

async function completeWizardViaApi(base: string, config: BootstrapConfig): Promise<void> {
  console.log('==> Jellyfin: completing startup wizard via API');

  // Jellyfin's StartupController expects the wizard configuration before it
  // creates the first local user. Calling Startup/User first returns HTTP 500
  // ("Sequence contains no elements") on a completely fresh database.
  await completeWizardConfiguration(base, config);

  // Startup/User only updates the first user; it does not initialize one on
  // Jellyfin 10.10.7. GET /Startup/User calls UserManager.InitializeAsync(),
  // which creates the implicit administrator that the POST then updates.
  const firstUser = await http('GET', `${base}/Startup/User`);
  if (!firstUser.ok) {
    throw new Error(`Startup/User initialization failed: HTTP ${firstUser.status}`);
  }

  await retryAsync(
    async () => {
      const userResponse = await http('POST', `${base}/Startup/User`, {
        headers: { 'Content-Type': 'application/json' },
        body: {
          Name: config.jellyfinAdminUser,
          Password: config.jellyfinAdminPassword,
        },
      });
      if (userResponse.ok || userResponse.status === 204 || userResponse.status === 409) {
        return;
      }
      if (await canAuthenticate(base, config)) {
        return;
      }
      const body = await userResponse.text();
      throw new Error(`Startup/User failed: HTTP ${userResponse.status} ${body}`);
    },
    { attempts: 6, delayMs: 5_000 },
  );

  await completeWizardRemoteAccess(base);
  await completeWizardFinalize(base);
  console.log('==> Jellyfin: startup wizard completed via API');
}

async function completeWizardConfiguration(base: string, config: BootstrapConfig): Promise<void> {
  const configResponse = await http('POST', `${base}/Startup/Configuration`, {
    headers: { 'Content-Type': 'application/json' },
    body: {
      UICulture: config.jellyfinLanguage,
      MetadataCountryCode: 'HU',
      PreferredMetadataLanguage: config.jellyfinLanguage.split('-')[0] ?? 'en',
    },
  });
  if (!configResponse.ok && configResponse.status !== 204) {
    throw new Error(`Startup/Configuration failed: HTTP ${configResponse.status}`);
  }
}

async function completeWizardRemoteAccess(base: string): Promise<void> {
  const response = await http('POST', `${base}/Startup/RemoteAccess`, {
    headers: { 'Content-Type': 'application/json' },
    body: {
      EnableRemoteAccess: true,
      EnableAutomaticPortMapping: false,
    },
  });
  if (!response.ok && response.status !== 204) {
    throw new Error(`Startup/RemoteAccess failed: HTTP ${response.status}`);
  }
}

async function completeWizardFinalize(base: string): Promise<void> {
  const completeResponse = await http('POST', `${base}/Startup/Complete`, {
    headers: { 'Content-Type': 'application/json' },
    body: {},
  });
  if (!completeResponse.ok && completeResponse.status !== 204) {
    throw new Error(`Startup/Complete failed: HTTP ${completeResponse.status}`);
  }
}

async function ensureWizardMarkedComplete(base: string): Promise<void> {
  const info = await httpJson<JellyfinPublicInfo>('GET', `${base}/System/Info/Public`);
  if (info.StartupWizardCompleted) {
    return;
  }

  await completeWizardFinalize(base);
  await waitForJellyfinWizardComplete(base, 30_000);
  console.log('==> Jellyfin: startup wizard flag set to complete');
}

export async function ensureJellyfinReadyForSeerr(base: string): Promise<void> {
  const info = await httpJson<JellyfinPublicInfo>('GET', `${base}/System/Info/Public`);
  if (info.StartupWizardCompleted) {
    return;
  }

  await ensureWizardMarkedComplete(base);
}

async function completeWizardViaPlaywright(config: BootstrapConfig): Promise<void> {
  const base = config.urls.jellyfin.replace(/\/$/, '');
  await mkdir(config.artifactsDir, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();

  try {
    // Jellyfin keeps a WebSocket open; `networkidle` never settles and turns a
    // recoverable first-run page into a two-minute timeout.
    await page.goto(`${base}/web/`, { waitUntil: 'domcontentloaded', timeout: 120_000 });
    await page.waitForSelector('input[type="text"], input[type="password"]', {
      timeout: 30_000,
    }).catch(() => undefined);
    await completeWizard(page, config);
  } catch (error) {
    await page.screenshot({
      path: `${config.artifactsDir}/jellyfin-failure.png`,
      fullPage: true,
    });
    throw error;
  } finally {
    await browser.close();
  }
}

async function completeWizard(page: Page, config: BootstrapConfig): Promise<void> {
  console.log('==> Jellyfin: running first-run wizard (admin user only)');

  await clickWizardButton(page, /next|tovább|continue|folytatás/i, 5_000);

  const languageSelect = page.locator('select').first();
  if (await languageSelect.isVisible({ timeout: 5_000 }).catch(() => false)) {
    await languageSelect.selectOption(config.jellyfinLanguage).catch(() => undefined);
    await clickWizardButton(page, /next|tovább/i, 5_000);
  }

  await page.getByLabel(/username/i).fill(config.jellyfinAdminUser).catch(async () => {
    await page.locator('input[name="username"], input[type="text"]').first().fill(config.jellyfinAdminUser);
  });
  await page.getByLabel(/password/i).first().fill(config.jellyfinAdminPassword).catch(async () => {
    await page.locator('input[type="password"]').first().fill(config.jellyfinAdminPassword);
  });
  const confirm = page.locator('input[type="password"]').nth(1);
  if (await confirm.isVisible({ timeout: 2_000 }).catch(() => false)) {
    await confirm.fill(config.jellyfinAdminPassword);
  }
  await clickWizardButton(page, /next|tovább/i, 10_000);
  await clickWizardButton(page, /next|tovább|finish|befejezés|done|kész/i, 5_000);
  await clickWizardButton(page, /finish|befejezés|done|kész/i, 10_000);

  console.log('==> Jellyfin: Playwright wizard steps completed');
}

async function clickWizardButton(page: Page, label: RegExp, timeout: number): Promise<void> {
  const locator = page.getByRole('button', { name: label }).first();
  try {
    await locator.click({ timeout });
  } catch {
    // Wizard step may already be past this button.
  }
}

async function createApiKeyViaApi(config: BootstrapConfig): Promise<string | undefined> {
  const base = config.urls.jellyfin.replace(/\/$/, '');
  const token = await jellyfinAuthToken(base, config);

  const response = await http('POST', `${base}/Auth/Keys?app=Seerr`, {
    headers: {
      'X-Emby-Token': token,
      'Content-Type': 'application/json',
    },
    body: {},
  });
  if (!response.ok && response.status !== 204) {
    throw new Error(`Jellyfin API key creation failed: HTTP ${response.status}`);
  }

  const keys = await httpJson<{ Items?: Array<{ AppName?: string; AccessToken?: string }> }>(
    'GET',
    `${base}/Auth/Keys`,
    {
      headers: { 'X-Emby-Token': token },
    },
  );
  const apiKey =
    keys.Items?.find((item) => item.AppName === 'Seerr')?.AccessToken ??
    keys.Items?.at(-1)?.AccessToken;
  if (apiKey) {
    await writeFile(config.jellyfinApiKeyFile, apiKey, 'utf8');
    console.log('==> Jellyfin: API key created for Seerr');
  }
  return apiKey;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
