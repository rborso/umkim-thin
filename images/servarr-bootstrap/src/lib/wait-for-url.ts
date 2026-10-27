import { http, httpJson } from './http.js';

export async function waitForUrl(
  url: string,
  label: string,
  timeoutMs = 120_000,
): Promise<void> {
  const started = Date.now();
  let lastError = '';

  while (Date.now() - started < timeoutMs) {
    try {
      const response = await http('GET', url, { timeoutMs: 10_000 });
      if (response.ok || response.status < 500) {
        console.log(`==> ${label} is reachable (${url})`);
        return;
      }
      lastError = `HTTP ${response.status}`;
    } catch (error) {
      lastError = error instanceof Error ? error.message : String(error);
    }
    await sleep(3_000);
  }

  throw new Error(`Timed out waiting for ${label} at ${url}: ${lastError}`);
}

export async function waitForProwlarrApi(
  base: string,
  apiKey: string,
  timeoutMs = 180_000,
): Promise<void> {
  const url = `${base.replace(/\/$/, '')}/api/v1/indexer`;
  const started = Date.now();
  let lastError = '';

  while (Date.now() - started < timeoutMs) {
    try {
      await httpJson('GET', url, {
        headers: { 'X-Api-Key': apiKey },
        timeoutMs: 30_000,
      });
      console.log(`==> Prowlarr API is ready (${url})`);
      return;
    } catch (error) {
      lastError = error instanceof Error ? error.message : String(error);
    }
    await sleep(5_000);
  }

  throw new Error(`Timed out waiting for Prowlarr API: ${lastError}`);
}

export async function waitForLidarrApi(
  base: string,
  apiKey: string,
  timeoutMs = 180_000,
): Promise<void> {
  const url = `${base.replace(/\/$/, '')}/api/v1/system/status`;
  const started = Date.now();
  let lastError = '';

  while (Date.now() - started < timeoutMs) {
    try {
      await httpJson('GET', url, {
        headers: { 'X-Api-Key': apiKey },
        timeoutMs: 30_000,
      });
      console.log(`==> Lidarr API is ready (${url})`);
      return;
    } catch (error) {
      lastError = error instanceof Error ? error.message : String(error);
    }
    await sleep(5_000);
  }

  throw new Error(`Timed out waiting for Lidarr API: ${lastError}`);
}

export async function waitForArrV3Api(
  base: string,
  apiKey: string,
  label: string,
  timeoutMs = 180_000,
): Promise<void> {
  const url = `${base.replace(/\/$/, '')}/api/v3/system/status`;
  const started = Date.now();
  let lastError = '';

  while (Date.now() - started < timeoutMs) {
    try {
      await httpJson('GET', url, {
        headers: { 'X-Api-Key': apiKey },
        timeoutMs: 30_000,
      });
      console.log(`==> ${label} API is ready (${url})`);
      return;
    } catch (error) {
      lastError = error instanceof Error ? error.message : String(error);
    }
    await sleep(5_000);
  }

  throw new Error(`Timed out waiting for ${label} API: ${lastError}`);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export async function waitForServices(
  checks: Array<{ url: string; label: string }>,
): Promise<void> {
  for (const check of checks) {
    await waitForUrl(check.url, check.label);
  }
}
