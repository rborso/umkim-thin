import { qualityProfileName } from './api/arr-profiles.js';
import { getSeerrAuthCookie } from './api/seerr.js';
import { BootstrapConfig } from './config.js';
import { http, httpJson } from './lib/http.js';

export async function verifyBootstrap(
  config: BootstrapConfig,
  keys: {
    prowlarr?: string;
    sonarr?: string;
    radarr?: string;
    jellyfinApiKey?: string;
  },
): Promise<void> {
  if (config.components.prowlarr && keys.prowlarr) {
    await verifyProwlarr(config, keys.prowlarr);
  }
  if (config.components.sonarr && keys.sonarr) {
    await verifySonarr(config, keys.sonarr);
  }
  if (config.components.radarr && keys.radarr) {
    await verifyRadarr(config, keys.radarr);
  }
  if (config.components.jellyfin) {
    await verifyJellyfin(config, keys.jellyfinApiKey);
  }
  if (config.components.seerr) {
    await verifySeerr(config);
  }
  console.log('==> Verification passed');
}

async function verifyProwlarr(config: BootstrapConfig, apiKey: string): Promise<void> {
  const base = config.urls.prowlarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  const host = await httpJson<{ authenticationMethod?: string }>(
    'GET',
    `${base}/api/v1/config/host`,
    { headers },
  );
  if (host.authenticationMethod?.toLowerCase() !== 'forms') {
    throw new Error('Verification failed: Prowlarr Forms authentication not enabled');
  }

  const clients = await httpJson<Array<{ implementation: string; enable: boolean }>>(
    'GET',
    `${base}/api/v1/downloadclient`,
    { headers },
  );
  if (!clients.some((client) => client.implementation === 'QBittorrent' && client.enable)) {
    console.log('WARN: Prowlarr qBittorrent client missing — add manually if downloads fail');
  }
}

async function verifySonarr(config: BootstrapConfig, apiKey: string): Promise<void> {
  const base = config.urls.sonarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  await verifyArrApp(base, headers, 'Sonarr', '/media/tv', config);
}

async function verifyRadarr(config: BootstrapConfig, apiKey: string): Promise<void> {
  const base = config.urls.radarr.replace(/\/$/, '');
  const headers = { 'X-Api-Key': apiKey };

  await verifyArrApp(base, headers, 'Radarr', '/media/movies', config);
}

async function verifyArrApp(
  base: string,
  headers: Record<string, string>,
  label: string,
  rootPath: string,
  config: BootstrapConfig,
): Promise<void> {
  const folders = await httpJson<Array<{ path: string }>>('GET', `${base}/api/v3/rootfolder`, {
    headers,
  });
  if (!folders.some((folder) => folder.path === rootPath)) {
    throw new Error(`Verification failed: ${label} root folder ${rootPath} missing`);
  }

  const indexers = await httpJson<Array<{ name: string }>>('GET', `${base}/api/v3/indexer`, {
    headers,
  });
  if (!indexers.some((indexer) => indexer.name.includes('Prowlarr'))) {
    // Indexers are deliberately manual (Prowlarr is the source of truth). A
    // fresh stack with no tracker configured must still finish bootstrap; it is
    // simply not ready to search until an indexer is added and synced.
    console.log(`WARN: ${label} has no Prowlarr-synced indexers; add one in Prowlarr when ready`);
  }

  const profileName = qualityProfileName(config.defaultQualityCutoff);
  const profiles = await httpJson<Array<{ name: string }>>('GET', `${base}/api/v3/qualityprofile`, {
    headers,
  });
  if (!profiles.some((profile) => profile.name === profileName)) {
    throw new Error(`Verification failed: ${label} missing ${profileName} quality profile`);
  }

  const host = await httpJson<{ authenticationMethod?: string }>(
    'GET',
    `${base}/api/v3/config/host`,
    { headers },
  );
  if (host.authenticationMethod?.toLowerCase() !== 'forms') {
    throw new Error(`Verification failed: ${label} Forms authentication not enabled`);
  }

  const clients = await httpJson<Array<{ implementation: string }>>(
    'GET',
    `${base}/api/v3/downloadclient`,
    { headers },
  );
  if (!clients.some((client) => client.implementation === 'QBittorrent')) {
    console.log(`WARN: ${label} qBittorrent client missing — add manually if needed`);
  }
}

async function verifyJellyfin(
  config: BootstrapConfig,
  jellyfinApiKey?: string,
): Promise<void> {
  const base = config.urls.jellyfin.replace(/\/$/, '');
  const response = await http('GET', `${base}/System/Info/Public`);
  if (!response.ok) {
    throw new Error('Verification failed: Jellyfin public info unavailable');
  }
  const auth = await httpJson<{ AccessToken?: string }>('POST', `${base}/Users/AuthenticateByName`, {
    body: {
      Username: config.jellyfinAdminUser,
      Pw: config.jellyfinAdminPassword,
    },
    headers: {
      'X-Emby-Authorization':
        'MediaBrowser Client="servarr-bootstrap", Device="bootstrap", DeviceId="bootstrap-job", Version="1.0.0"',
      'Content-Type': 'application/json',
    },
  });
  if (!auth.AccessToken) {
    throw new Error('Verification failed: Jellyfin login with bootstrap credentials failed');
  }
  const libraries = await httpJson<
    Array<{
      Name?: string;
      Locations?: string[];
      LibraryOptions?: { EnableRealtimeMonitor?: boolean };
    }>
  >('GET', `${base}/Library/VirtualFolders`, {
    headers: { 'X-Emby-Token': auth.AccessToken },
  });
  for (const expected of [
    { name: 'Movies', path: '/media/movies' },
    { name: 'Series', path: '/media/tv' },
  ]) {
    const found = libraries.find(
      (library) =>
        library.Name === expected.name ||
        library.Locations?.some((location) => location.replace(/\/$/, '') === expected.path),
    );
    if (!found) {
      throw new Error(
        `Verification failed: Jellyfin library "${expected.name}" (${expected.path}) missing`,
      );
    }
    if (!found.LibraryOptions?.EnableRealtimeMonitor) {
      throw new Error(
        `Verification failed: Jellyfin library "${expected.name}" real-time monitor disabled`,
      );
    }
  }

  if (!jellyfinApiKey) {
    console.log('WARN: Jellyfin API key not captured; Seerr may need manual API key');
  }
}

async function verifySeerr(config: BootstrapConfig): Promise<void> {
  const base = config.urls.seerr.replace(/\/$/, '');
  const response = await http('GET', `${base}/api/v1/status`);
  if (!response.ok) {
    throw new Error('Verification failed: Seerr status endpoint unavailable');
  }
  const status = (await response.json()) as { version?: string };
  if (!status.version) {
    throw new Error('Verification failed: Seerr not reporting version');
  }

  const settings = await httpJson<{ initialized?: boolean }>(
    'GET',
    `${base}/api/v1/settings/public`,
  );
  if (!settings.initialized) {
    throw new Error('Verification failed: Seerr setup wizard not completed');
  }

  const cookie = await getSeerrAuthCookie(base, config);
  const authHeaders = await seerrSettingsHeaders(base, cookie);
  const expectedProfile = qualityProfileName(config.defaultQualityCutoff);
  const radarrServers = await httpJson<Array<{
    name: string;
    isDefault?: boolean;
    activeProfileName?: string;
  }>>('GET', `${base}/api/v1/settings/radarr`, { headers: authHeaders });
  if (
    !radarrServers.some(
      (server) =>
        server.name === 'Radarr' &&
        server.isDefault &&
        server.activeProfileName === expectedProfile,
    )
  ) {
    throw new Error(`Verification failed: Seerr default Radarr profile is not ${expectedProfile}`);
  }

  const sonarrServers = await httpJson<Array<{
    name: string;
    isDefault?: boolean;
    activeProfileName?: string;
  }>>('GET', `${base}/api/v1/settings/sonarr`, { headers: authHeaders });
  if (
    !sonarrServers.some(
      (server) =>
        server.name === 'Sonarr' &&
        server.isDefault &&
        server.activeProfileName === expectedProfile,
    )
  ) {
    throw new Error(`Verification failed: Seerr default Sonarr profile is not ${expectedProfile}`);
  }
}

async function seerrSettingsHeaders(
  base: string,
  cookie: string,
): Promise<Record<string, string>> {
  try {
    const radarrProbe = await http('GET', `${base}/api/v1/settings/radarr`, {
      headers: { Cookie: cookie },
    });
    if (radarrProbe.ok) {
      return { Cookie: cookie };
    }
  } catch {
    // fall through to API key auth
  }

  const main = await httpJson<{ apiKey?: string }>('GET', `${base}/api/v1/settings/main`, {
    headers: { Cookie: cookie },
  });
  if (!main.apiKey) {
    throw new Error('Verification failed: Seerr settings auth unavailable');
  }
  return { 'X-Api-Key': main.apiKey };
}
