import { BootstrapConfig } from '../config.js';
import { enableFormsAuthV3 } from './arr-host.js';
import { applyDefaultProfiles } from './arr-profiles.js';
import { httpJson, httpJsonWithRetry } from '../lib/http.js';
import {
  buildArrDownloadClientBody,
  buildQBittorrentFields,
} from './qbittorrent-client.js';

interface RootFolder {
  path: string;
}

interface DownloadClient {
  name: string;
  implementation: string;
}

export async function configureRadarr(
  config: BootstrapConfig,
  apiKey: string,
): Promise<number> {
  const base = config.urls.radarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  await enableFormsAuthV3(base, headers, config, 'Radarr');
  await ensureRootFolder(base, headers, '/media/movies');
  await ensureQBittorrentClient(base, headers, config);
  await enableDownloadHandling(base, headers);
  await enableHardlinks(base, headers);
  return applyDefaultProfiles(base, headers, config, 'Radarr');
}

async function ensureRootFolder(
  base: string,
  headers: Record<string, string>,
  path: string,
): Promise<void> {
  const folders = await httpJson<RootFolder[]>('GET', `${base}/api/v3/rootfolder`, {
    headers,
  });
  if (folders.some((folder) => folder.path === path)) {
    console.log(`==> Radarr: root folder ${path} already exists`);
    return;
  }
  await httpJsonWithRetry('POST', `${base}/api/v3/rootfolder`, {
    headers,
    body: { path },
  });
  console.log(`==> Radarr: added root folder ${path}`);
}

async function ensureQBittorrentClient(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
): Promise<void> {
  const clients = await httpJson<DownloadClient[]>('GET', `${base}/api/v3/downloadclient`, {
    headers,
  });
  if (clients.some((client) => client.implementation === 'QBittorrent')) {
    console.log('==> Radarr: qBittorrent download client already exists');
    return;
  }

  const schema = await httpJson<Array<Record<string, unknown>>>(
    'GET',
    `${base}/api/v3/downloadclient/schema`,
    { headers },
  );
  const qbt = schema.find((item) => item.implementation === 'QBittorrent');
  if (!qbt) {
    throw new Error('Radarr qBittorrent schema not found');
  }

  const fields = buildQBittorrentFields(
    qbt.fields as Array<{ name: string; value?: unknown }>,
    config,
    'movieCategory',
    'movies',
  );

  try {
    await httpJsonWithRetry('POST', `${base}/api/v3/downloadclient`, {
      headers,
      body: buildArrDownloadClientBody(
        qbt as Parameters<typeof buildArrDownloadClientBody>[0],
        fields,
        'qBittorrent',
      ),
    });
    console.log('==> Radarr: added qBittorrent download client');
  } catch (error) {
    console.log(
      `WARN: Radarr qBittorrent client save failed (${error instanceof Error ? error.message : error}); add manually in Radarr UI`,
    );
  }
}

async function enableDownloadHandling(
  base: string,
  headers: Record<string, string>,
): Promise<void> {
  const downloadConfig = await httpJson<Record<string, unknown>>(
    'GET',
    `${base}/api/v3/config/downloadclient`,
    { headers },
  );
  if (downloadConfig.enableCompletedDownloadHandling && downloadConfig.autoRedownloadFailed) {
    console.log('==> Radarr: download handling already enabled');
    return;
  }
  await httpJsonWithRetry('PUT', `${base}/api/v3/config/downloadclient/${downloadConfig.id}`, {
    headers,
    body: {
      ...downloadConfig,
      enableCompletedDownloadHandling: true,
      autoRedownloadFailed: true,
    },
  });
  console.log('==> Radarr: enabled completed/failed download handling');
}

async function enableHardlinks(
  base: string,
  headers: Record<string, string>,
): Promise<void> {
  const mediaConfig = await httpJson<Record<string, unknown>>(
    'GET',
    `${base}/api/v3/config/mediamanagement`,
    { headers },
  );
  if (mediaConfig.copyUsingHardlinks === true) {
    console.log('==> Radarr: hardlinks already enabled');
    return;
  }
  await httpJsonWithRetry('PUT', `${base}/api/v3/config/mediamanagement/${mediaConfig.id}`, {
    headers,
    body: {
      ...mediaConfig,
      copyUsingHardlinks: true,
    },
  });
  console.log('==> Radarr: enabled hardlinks');
}
