import { BootstrapConfig } from '../config.js';
import { enableFormsAuthV1 } from './arr-host.js';
import { httpJson, httpJsonWithRetry } from '../lib/http.js';

interface RootFolder {
  path: string;
}

export async function configureLidarr(
  config: BootstrapConfig,
  apiKey: string,
): Promise<void> {
  const base = config.urls.lidarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  await enableFormsAuthV1(base, headers, config, 'Lidarr');
  await ensureRootFolder(base, headers, '/media/music');
}

async function ensureRootFolder(
  base: string,
  headers: Record<string, string>,
  path: string,
): Promise<void> {
  const folders = await httpJson<RootFolder[]>('GET', `${base}/api/v1/rootfolder`, {
    headers,
  });
  if (folders.some((folder) => folder.path === path)) {
    console.log(`==> Lidarr: root folder ${path} already exists`);
    return;
  }

  const metadataProfiles = await httpJson<Array<{ id: number }>>(
    'GET',
    `${base}/api/v1/metadataprofile`,
    { headers },
  );
  const qualityProfiles = await httpJson<Array<{ id: number }>>(
    'GET',
    `${base}/api/v1/qualityprofile`,
    { headers },
  );
  const metadataProfileId = metadataProfiles[0]?.id ?? 1;
  const qualityProfileId = qualityProfiles[0]?.id ?? 1;

  await httpJsonWithRetry('POST', `${base}/api/v1/rootfolder`, {
    headers,
    body: {
      path,
      name: 'Music',
      defaultMetadataProfileId: metadataProfileId,
      defaultQualityProfileId: qualityProfileId,
      defaultMonitorOption: 'all',
      defaultTags: [],
      isCalibreLibrary: false,
    },
  });
  console.log(`==> Lidarr: added root folder ${path}`);
}
