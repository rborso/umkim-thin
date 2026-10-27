import { BootstrapConfig } from '../config.js';
import { http, httpJson } from '../lib/http.js';
import { retryAsync } from '../lib/retry.js';
import { hostnameFromServiceUrl, portFromServiceUrl } from '../lib/service-url.js';
import { ensureJellyfinReadyForSeerr } from '../ui/jellyfin.setup.js';
import { qualityProfileName } from './arr-profiles.js';

const JELLYFIN_MEDIA_SERVER_TYPE = 2;

// Jellyseerr/Seerr permission bitflags.
const PERM_ADMIN = 2;
const PERM_REQUEST = 32;
const PERM_AUTO_APPROVE = 128;
const PERM_REQUEST_ADVANCED = 8192;
const REQUEST_STATUS_PENDING = 1;

interface SeerrSonarrServer {
  id: number;
  name: string;
  isDefault?: boolean;
  activeProfileId?: number;
  activeProfileName?: string;
  [key: string]: unknown;
}

interface SeerrRadarrServer {
  id: number;
  name: string;
  isDefault?: boolean;
  activeProfileId?: number;
  activeProfileName?: string;
  [key: string]: unknown;
}

interface SeerrPublicSettings {
  initialized?: boolean;
}

interface SeerrMainSettings {
  originalLanguage?: string;
  defaultPermissions?: number;
}

interface SeerrUser {
  id?: number;
  displayName?: string;
  permissions?: number;
}

interface SeerrRequest {
  id?: number;
  status?: number;
}

interface SeerrList<T> {
  results?: T[];
}

interface SeerrLibrary {
  id?: string;
  name?: string;
  enabled?: boolean;
  type?: string;
}

export function parseCookieFromResponse(response: Response): string {
  const headers = response.headers as Headers & { getSetCookie?: () => string[] };
  if (typeof headers.getSetCookie === 'function') {
    const cookies = headers.getSetCookie();
    if (cookies.length > 0) {
      return cookies
        .map((part) => part.split(';')[0]?.trim())
        .filter(Boolean)
        .join('; ');
    }
  }
  const setCookie = response.headers.get('set-cookie');
  if (!setCookie) {
    return '';
  }
  return setCookie
    .split(/,(?=\s*[^;]+=[^;]+)/)
    .map((part) => part.split(';')[0]?.trim())
    .filter(Boolean)
    .join('; ');
}

async function loginWithJellyfin(
  base: string,
  config: BootstrapConfig,
  options: { configureHostname?: boolean } = {},
): Promise<string> {
  const configureHostname = options.configureHostname ?? true;
  const body: Record<string, unknown> = {
    username: config.jellyfinAdminUser,
    password: config.jellyfinAdminPassword,
    serverType: JELLYFIN_MEDIA_SERVER_TYPE,
    email: `${config.jellyfinAdminUser}@servarr.local`,
  };
  if (configureHostname) {
    body.hostname = hostnameFromServiceUrl(config.urls.jellyfin);
    body.port = portFromServiceUrl(config.urls.jellyfin, 8096);
    body.useSsl = false;
    body.urlBase = '';
  }

  const response = await http('POST', `${base}/api/v1/auth/jellyfin`, { body });

  if (!response.ok) {
    const responseBody = await response.text();
    throw new Error(`Seerr Jellyfin auth failed: HTTP ${response.status} ${responseBody}`);
  }

  const cookie = parseCookieFromResponse(response);
  if (!cookie) {
    throw new Error('Seerr Jellyfin auth succeeded but no session cookie was returned');
  }

  return cookie;
}

export async function getSeerrAuthCookie(
  base: string,
  config: BootstrapConfig,
): Promise<string> {
  const publicSettings = await httpJson<SeerrPublicSettings>(
    'GET',
    `${base}/api/v1/settings/public`,
  ).catch(() => ({}) as SeerrPublicSettings);

  if (publicSettings.initialized) {
    return retryAsync(
      () => loginWithJellyfin(base, config, { configureHostname: false }),
      { attempts: 3, delayMs: 5_000 },
    );
  }

  try {
    return await retryAsync(() => loginWithJellyfin(base, config, { configureHostname: true }), {
      attempts: 3,
      delayMs: 5_000,
    });
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    if (!message.includes('hostname already configured')) {
      throw error;
    }
    console.log('==> Seerr: Jellyfin hostname already configured, logging in without hostname');
    return retryAsync(
      () => loginWithJellyfin(base, config, { configureHostname: false }),
      { attempts: 3, delayMs: 5_000 },
    );
  }
}

async function addSonarrServer(
  base: string,
  cookie: string,
  config: BootstrapConfig,
  sonarrApiKey: string,
  profileId: number,
): Promise<void> {
  const profileName = qualityProfileName(config.defaultQualityCutoff);
  const headers = { Cookie: cookie };
  const existing = await retryAsync(
    () =>
      httpJson<SeerrSonarrServer[]>('GET', `${base}/api/v1/settings/sonarr`, {
        headers,
      }),
    { attempts: 3, delayMs: 3_000 },
  );
  const match =
    existing.find((server) => server.name === 'Sonarr' && server.isDefault) ??
    existing.find((server) => server.name === 'Sonarr');
  if (match) {
    if (
      match.isDefault &&
      match.activeProfileId === profileId &&
      match.activeProfileName === profileName
    ) {
      console.log('==> Seerr: Sonarr server already uses the default profile');
      return;
    }
    const { id: _ignored, ...current } = match;
    await retryAsync(
      () =>
        httpJson('PUT', `${base}/api/v1/settings/sonarr/${match.id}`, {
          headers,
          body: {
            ...current,
            activeProfileId: profileId,
            activeProfileName: profileName,
            isDefault: true,
          },
        }),
      { attempts: 3, delayMs: 3_000 },
    );
    console.log(`==> Seerr: updated Sonarr default profile to ${profileName}`);
    return;
  }

  await retryAsync(
    () =>
      httpJson('POST', `${base}/api/v1/settings/sonarr`, {
        headers,
        body: {
          name: 'Sonarr',
          hostname: hostnameFromServiceUrl(config.urls.sonarr),
          port: portFromServiceUrl(config.urls.sonarr, 80),
          apiKey: sonarrApiKey,
          useSsl: false,
          baseUrl: '',
          activeProfileId: profileId,
          activeProfileName: profileName,
          activeDirectory: '/media/tv',
          is4k: false,
          isDefault: true,
          enableSeasonFolders: true,
        },
      }),
    { attempts: 3, delayMs: 3_000 },
  );
  console.log('==> Seerr: added Sonarr server');
}

async function addRadarrServer(
  base: string,
  cookie: string,
  config: BootstrapConfig,
  radarrApiKey: string,
  profileId: number,
): Promise<void> {
  const profileName = qualityProfileName(config.defaultQualityCutoff);
  const headers = { Cookie: cookie };
  const existing = await retryAsync(
    () =>
      httpJson<SeerrRadarrServer[]>('GET', `${base}/api/v1/settings/radarr`, {
        headers,
      }),
    { attempts: 3, delayMs: 3_000 },
  );
  const match =
    existing.find((server) => server.name === 'Radarr' && server.isDefault) ??
    existing.find((server) => server.name === 'Radarr');
  if (match) {
    if (
      match.isDefault &&
      match.activeProfileId === profileId &&
      match.activeProfileName === profileName
    ) {
      console.log('==> Seerr: Radarr server already uses the default profile');
      return;
    }
    const { id: _ignored, ...current } = match;
    await retryAsync(
      () =>
        httpJson('PUT', `${base}/api/v1/settings/radarr/${match.id}`, {
          headers,
          body: {
            ...current,
            activeProfileId: profileId,
            activeProfileName: profileName,
            isDefault: true,
          },
        }),
      { attempts: 3, delayMs: 3_000 },
    );
    console.log(`==> Seerr: updated Radarr default profile to ${profileName}`);
    return;
  }

  await retryAsync(
    () =>
      httpJson('POST', `${base}/api/v1/settings/radarr`, {
        headers,
        body: {
          name: 'Radarr',
          hostname: hostnameFromServiceUrl(config.urls.radarr),
          port: portFromServiceUrl(config.urls.radarr, 80),
          apiKey: radarrApiKey,
          useSsl: false,
          baseUrl: '',
          activeProfileId: profileId,
          activeProfileName: profileName,
          activeDirectory: '/media/movies',
          is4k: false,
          isDefault: true,
          minimumAvailability: 'released',
        },
      }),
    { attempts: 3, delayMs: 3_000 },
  );
  console.log('==> Seerr: added Radarr server');
}

async function setOriginalLanguage(
  base: string,
  cookie: string,
  language: string,
): Promise<void> {
  const main = await httpJson<SeerrMainSettings>('GET', `${base}/api/v1/settings/main`, {
    headers: { Cookie: cookie },
  });
  if (main.originalLanguage === language) {
    return;
  }
  await httpJson('POST', `${base}/api/v1/settings/main`, {
    headers: { Cookie: cookie },
    body: {
      originalLanguage: language,
    },
  });
  console.log(`==> Seerr: set original language to ${language}`);
}

// enableJellyfinLibraries turns on library scanning in Seerr. Without an enabled
// library, Seerr never imports availability from Jellyfin, so approved requests
// stay stuck on "Requested" even after the media has been downloaded.
async function enableJellyfinLibraries(base: string, cookie: string): Promise<void> {
  const headers = { Cookie: cookie };
  const libraries = await retryAsync(
    async () => {
      const libs = await httpJson<SeerrLibrary[]>(
        'POST',
        `${base}/api/v1/settings/jellyfin/library/sync`,
        { headers, body: {} },
      );
      if (!Array.isArray(libs) || libs.length === 0) {
        throw new Error('Jellyfin libraries not available yet');
      }
      return libs;
    },
    { attempts: 5, delayMs: 5_000 },
  ).catch(() => [] as SeerrLibrary[]);
  for (const library of libraries) {
    if (!library.id || library.enabled) {
      continue;
    }
    await httpJson('PUT', `${base}/api/v1/settings/jellyfin/library/${library.id}`, {
      headers,
      body: { enabled: true },
    }).catch((error) => {
      console.log(`WARN: Seerr: could not enable Jellyfin library ${library.name ?? library.id}: ${error}`);
    });
    console.log(`==> Seerr: enabled Jellyfin library ${library.name ?? library.id}`);
  }
}

// applyRequestPermissions gives regular users the advanced request controls
// (server/profile/language choices) while preserving the operator's
// auto-approve choice. Seerr gates the Advanced section on REQUEST_ADVANCED;
// without it only admins can see the quality-profile picker.
async function applyRequestPermissions(
  base: string,
  cookie: string,
  autoApprove: boolean,
): Promise<void> {
  const headers = { Cookie: cookie };
  const wanted = PERM_REQUEST | PERM_REQUEST_ADVANCED | (autoApprove ? PERM_AUTO_APPROVE : 0);

  const main = await httpJson<SeerrMainSettings>('GET', `${base}/api/v1/settings/main`, { headers });
  const current = main.defaultPermissions ?? 0;
  if ((current & wanted) !== wanted) {
    await httpJson('POST', `${base}/api/v1/settings/main`, {
      headers,
      body: { defaultPermissions: current | wanted },
    });
    console.log(
      `==> Seerr: enabled advanced requests for new users${autoApprove ? ' and auto-approve' : ''}`,
    );
  }

  const users = await httpJson<SeerrList<SeerrUser>>('GET', `${base}/api/v1/user?take=100`, { headers }).catch(
    () => ({}) as SeerrList<SeerrUser>,
  );
  for (const user of users.results ?? []) {
    const perms = user.permissions ?? 0;
    if (user.id == null || (perms & PERM_ADMIN) !== 0 || (perms & wanted) === wanted) {
      continue;
    }
    await httpJson('PUT', `${base}/api/v1/user/${user.id}`, {
      headers,
      body: { permissions: perms | wanted },
    }).catch((error) => {
      console.log(`WARN: Seerr: could not grant advanced requests to ${user.displayName ?? user.id}: ${error}`);
    });
    console.log(`==> Seerr: granted advanced requests to ${user.displayName ?? user.id}`);
  }

  if (!autoApprove) {
    return;
  }
  const requests = await httpJson<SeerrList<SeerrRequest>>(
    'GET',
    `${base}/api/v1/request?take=100&filter=all`,
    { headers },
  ).catch(() => ({}) as SeerrList<SeerrRequest>);
  for (const request of requests.results ?? []) {
    if (request.id == null || request.status !== REQUEST_STATUS_PENDING) {
      continue;
    }
    await httpJson('POST', `${base}/api/v1/request/${request.id}/approve`, { headers, body: {} }).catch(
      (error) => {
        console.log(`WARN: Seerr: could not approve request ${request.id}: ${error}`);
      },
    );
    console.log(`==> Seerr: approved pending request ${request.id}`);
  }
}

export async function setupSeerrViaApi(
  config: BootstrapConfig,
  sonarrApiKey: string,
  radarrApiKey: string,
  sonarrProfileId: number,
  radarrProfileId: number,
): Promise<void> {
  const base = config.urls.seerr.replace(/\/$/, '');
  const jellyfinBase = config.urls.jellyfin.replace(/\/$/, '');

  await ensureJellyfinReadyForSeerr(jellyfinBase);

  const publicSettings = await httpJson<SeerrPublicSettings>(
    'GET',
    `${base}/api/v1/settings/public`,
  );
  const initialized = publicSettings.initialized ?? false;

  const cookie = await getSeerrAuthCookie(base, config);

  // Apply the user-facing request permission before touching the *arr server
  // settings: a transient failure in those settings reads must not leave
  // regular users without the Advanced section. It is retried after
  // initialization below for a fresh instance.
  try {
    await applyRequestPermissions(base, cookie, config.seerrAutoApprove);
  } catch (error) {
    console.log(
      `WARN: Seerr: could not grant advanced request permission yet (${error instanceof Error ? error.message : error})`,
    );
  }

  await addSonarrServer(base, cookie, config, sonarrApiKey, sonarrProfileId);
  await addRadarrServer(base, cookie, config, radarrApiKey, radarrProfileId);

  if (!initialized) {
    await retryAsync(async () => {
      const initResponse = await http('POST', `${base}/api/v1/settings/initialize`, {
        headers: { Cookie: cookie },
        body: {},
      });
      if (!initResponse.ok) {
        const body = await initResponse.text();
        throw new Error(`Seerr initialize failed: HTTP ${initResponse.status} ${body}`);
      }
    }, {
      attempts: 3,
      delayMs: 5_000,
    });
    console.log('==> Seerr: initialized via API');
  } else {
    console.log('==> Seerr: already initialized; ensured Sonarr/Radarr servers');
  }

  await setOriginalLanguage(base, cookie, config.defaultMediaLanguage);
  await enableJellyfinLibraries(base, cookie);
  await applyRequestPermissions(base, cookie, config.seerrAutoApprove);
  console.log('==> Seerr: setup completed via API');
}
