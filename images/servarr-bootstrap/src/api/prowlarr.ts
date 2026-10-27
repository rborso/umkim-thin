import { BootstrapConfig } from '../config.js';
import { enableFormsAuthV1 } from './arr-host.js';
import { httpJson, httpJsonWithRetry } from '../lib/http.js';
import { inClusterHttpBase } from '../lib/service-url.js';
import {
  buildProwlarrDownloadClientBody,
  buildQBittorrentFields,
} from './qbittorrent-client.js';

interface Tag {
  id: number;
  label: string;
}

interface NamedResource {
  id?: number;
  name: string;
  implementation?: string;
}

export async function configureProwlarr(
  config: BootstrapConfig,
  apiKey: string,
  sonarrApiKey: string | undefined,
  radarrApiKey: string | undefined,
): Promise<void> {
  const base = config.urls.prowlarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  await enableFormsAuthV1(base, headers, config, 'Prowlarr');

  let flaresolverrTagId: number | undefined;
  if (config.components.flaresolverr) {
    flaresolverrTagId = await ensureTag(base, headers, 'flaresolverr');
    await ensureFlareSolverrProxy(base, headers, config, flaresolverrTagId);
  }
  // Indexers are a user choice and are added in Prowlarr (or through its API);
  // the bootstrap only wires the download client, the proxy and the *arr apps.
  if (config.components.qbittorrent) {
    await ensureQBittorrentClient(base, headers, config);
  }
  if (config.components.sonarr && sonarrApiKey) {
    await ensureSonarrApp(base, headers, config, sonarrApiKey);
  }
  if (config.components.radarr && radarrApiKey) {
    await ensureRadarrApp(base, headers, config, radarrApiKey);
  }
}

export async function syncProwlarrIndexers(
  config: BootstrapConfig,
  apiKey: string,
  sonarrApiKey: string | undefined,
  radarrApiKey: string | undefined,
): Promise<void> {
  const base = config.urls.prowlarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  // Indexers are manual. Do not spend 90 seconds waiting for a sync when
  // Prowlarr has nothing to sync; the stack is valid but unconfigured.
  const indexers = await httpJson<Array<{ id?: number }>>('GET', `${base}/api/v1/indexer`, { headers });
  if (indexers.length === 0) {
    console.log('==> Prowlarr: no indexers configured; skipping app indexer sync');
    return;
  }

  await syncApplications(base, headers);
  await verifySyncedIndexers(config, sonarrApiKey, radarrApiKey);
}

// ApplicationIndexerSync is asynchronous: it returns as soon as the command is
// queued, so a single fixed-delay check reported "no Prowlarr-synced indexers"
// for a sync that was still running (the verification step later in the run
// then passed). Poll until the indexer appears instead.
const SYNCED_INDEXER_TIMEOUT_MS = 90_000;
const SYNCED_INDEXER_POLL_MS = 5_000;

async function waitForSyncedIndexers(
  base: string,
  apiKey: string,
  label: string,
): Promise<void> {
  const headers = { 'X-Api-Key': apiKey };
  const deadline = Date.now() + SYNCED_INDEXER_TIMEOUT_MS;
  for (;;) {
    const indexers = await httpJson<Array<{ name: string }>>('GET', `${base}/api/v3/indexer`, {
      headers,
    });
    if (indexers.some((indexer) => indexer.name.includes('Prowlarr'))) {
      console.log(`==> Prowlarr: ${label} indexer sync verified`);
      return;
    }
    if (Date.now() >= deadline) {
      console.log(`WARN: ${label} has no Prowlarr-synced indexers after ApplicationIndexerSync`);
      return;
    }
    await sleep(SYNCED_INDEXER_POLL_MS);
  }
}

async function verifySyncedIndexers(
  config: BootstrapConfig,
  sonarrApiKey: string | undefined,
  radarrApiKey: string | undefined,
): Promise<void> {
  if (config.components.sonarr && sonarrApiKey) {
    await waitForSyncedIndexers(config.urls.sonarr.replace(/\/$/, ''), sonarrApiKey, 'Sonarr');
  }

  if (config.components.radarr && radarrApiKey) {
    await waitForSyncedIndexers(config.urls.radarr.replace(/\/$/, ''), radarrApiKey, 'Radarr');
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function syncApplications(
  base: string,
  headers: Record<string, string>,
): Promise<void> {
  try {
    await httpJsonWithRetry('POST', `${base}/api/v1/command`, {
      headers,
      body: { name: 'ApplicationIndexerSync' },
    });
    console.log('==> Prowlarr: triggered indexer sync to Sonarr/Radarr');
  } catch (error) {
    console.log(
      `WARN: Prowlarr indexer sync failed (${error instanceof Error ? error.message : error})`,
    );
  }
}

async function ensureFlareSolverrProxy(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
  flaresolverrTagId: number,
): Promise<void> {
  const proxies = await httpJson<NamedResource[]>('GET', `${base}/api/v1/indexerProxy`, {
    headers,
  });
  if (proxies.some((proxy) => proxy.name === 'FlareSolverr')) {
    console.log('==> Prowlarr: FlareSolverr proxy already exists');
    return;
  }

  await httpJsonWithRetry('POST', `${base}/api/v1/indexerProxy`, {
    headers,
    body: {
      enable: true,
      name: 'FlareSolverr',
      implementation: 'FlareSolverr',
      configContract: 'FlareSolverrSettings',
      fields: [
        { name: 'host', value: inClusterHttpBase(config.urls.flaresolverr) },
        { name: 'requestTimeout', value: 60 },
      ],
      tags: [flaresolverrTagId],
    },
  });
  console.log('==> Prowlarr: added FlareSolverr proxy');
}

async function ensureTag(
  base: string,
  headers: Record<string, string>,
  label: string,
): Promise<number> {
  const tags = await httpJson<Tag[]>('GET', `${base}/api/v1/tag`, { headers });
  const existing = tags.find((tag) => tag.label === label);
  if (existing) {
    return existing.id;
  }
  const created = await httpJsonWithRetry<Tag>('POST', `${base}/api/v1/tag`, {
    headers,
    body: { label },
  });
  return created.id;
}

async function ensureQBittorrentClient(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
): Promise<void> {
  const clients = await httpJson<NamedResource[]>('GET', `${base}/api/v1/downloadclient`, {
    headers,
  });
  if (clients.some((client) => client.implementation === 'QBittorrent')) {
    console.log('==> Prowlarr: qBittorrent download client already exists');
    return;
  }

  const schema = await httpJson<Array<Record<string, unknown>>>(
    'GET',
    `${base}/api/v1/downloadclient/schema`,
    { headers },
  );
  const qbt = schema.find((item) => item.implementation === 'QBittorrent');
  if (!qbt) {
    throw new Error('Prowlarr qBittorrent download client schema not found');
  }

  const fields = buildQBittorrentFields(
    qbt.fields as Array<{ name: string; value?: unknown }>,
    config,
    'category',
    'prowlarr',
  );

  try {
    await httpJsonWithRetry('POST', `${base}/api/v1/downloadclient`, {
      headers,
      body: buildProwlarrDownloadClientBody(
        qbt as Parameters<typeof buildProwlarrDownloadClientBody>[0],
        fields,
        'qBittorrent',
      ),
    });
    console.log('==> Prowlarr: added qBittorrent download client');
  } catch (error) {
    console.log(
      `WARN: Prowlarr qBittorrent client save failed (${error instanceof Error ? error.message : error}); add manually in Prowlarr UI`,
    );
  }
}

async function ensureSonarrApp(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
  sonarrApiKey: string,
): Promise<void> {
  const apps = await httpJson<NamedResource[]>('GET', `${base}/api/v1/applications`, {
    headers,
  });
  if (apps.some((app) => app.name === 'Sonarr')) {
    console.log('==> Prowlarr: Sonarr app already exists');
    return;
  }

  await httpJsonWithRetry('POST', `${base}/api/v1/applications`, {
    headers,
    body: {
      enable: true,
      name: 'Sonarr',
      implementation: 'Sonarr',
      configContract: 'SonarrSettings',
      syncLevel: 'fullSync',
      fields: [
        { name: 'prowlarrUrl', value: inClusterHttpBase(config.urls.prowlarr) },
        { name: 'baseUrl', value: config.urls.sonarr },
        { name: 'apiKey', value: sonarrApiKey },
      ],
    },
  });
  console.log('==> Prowlarr: added Sonarr app');
}

async function ensureRadarrApp(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
  radarrApiKey: string,
): Promise<void> {
  const apps = await httpJson<NamedResource[]>('GET', `${base}/api/v1/applications`, {
    headers,
  });
  if (apps.some((app) => app.name === 'Radarr')) {
    console.log('==> Prowlarr: Radarr app already exists');
    return;
  }

  await httpJsonWithRetry('POST', `${base}/api/v1/applications`, {
    headers,
    body: {
      enable: true,
      name: 'Radarr',
      implementation: 'Radarr',
      configContract: 'RadarrSettings',
      syncLevel: 'fullSync',
      fields: [
        { name: 'prowlarrUrl', value: inClusterHttpBase(config.urls.prowlarr) },
        { name: 'baseUrl', value: config.urls.radarr },
        { name: 'apiKey', value: radarrApiKey },
      ],
    },
  });
  console.log('==> Prowlarr: added Radarr app');
}
