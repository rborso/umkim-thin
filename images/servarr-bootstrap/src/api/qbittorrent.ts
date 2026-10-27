import { BootstrapConfig } from '../config.js';
import { http, httpJson } from '../lib/http.js';
import { hostnameFromServiceUrl } from '../lib/service-url.js';

export async function configureQBittorrent(config: BootstrapConfig): Promise<void> {
  const base = config.urls.qBittorrent.replace(/\/$/, '');
  await login(base, config.qbtUsername, config.qbtPassword);

  const prefs = await httpJson<Record<string, unknown>>(
    'GET',
    `${base}/api/v2/app/preferences`,
    { headers: authHeaders() },
  );

  const savePath = String(prefs.save_path ?? '');
  const updatedPrefs = {
    ...prefs,
    save_path: '/media/downloads',
    temp_path_enabled: false,
    bypass_local_auth: true,
    bypass_auth_subnet_whitelist_enabled: true,
    bypass_auth_subnet_whitelist: '172.16.0.0/12',
  };

  if (savePath !== '/media/downloads' || !prefs.bypass_local_auth) {
    const response = await http('POST', `${base}/api/v2/app/setPreferences`, {
      headers: authHeaders(),
      form: {
        json: JSON.stringify(updatedPrefs),
      },
    });
    if (!response.ok) {
      const body = await response.text();
      throw new Error(`qBittorrent setPreferences failed: HTTP ${response.status} ${body}`);
    }
    console.log('==> qBittorrent: updated preferences (save path / auth bypass)');
  } else {
    console.log('==> qBittorrent: preferences already configured');
  }

  const categories = await httpJson<Record<string, unknown>>(
    'GET',
    `${base}/api/v2/torrents/categories`,
    { headers: authHeaders() },
  );
  const categoryNames = new Set(Object.keys(categories));
  for (const category of ['prowlarr', 'tv', 'movies']) {
    if (!categoryNames.has(category)) {
      await http('POST', `${base}/api/v2/torrents/createCategory`, {
        headers: authHeaders(),
        form: { category, savePath: '/media/downloads' },
      });
      console.log(`==> qBittorrent: created category ${category}`);
    }
  }
}

let cookie = '';

async function login(base: string, username: string, password: string): Promise<void> {
  const response = await http('POST', `${base}/api/v2/auth/login`, {
    form: { username, password },
  });
  const text = await response.text();
  if (!response.ok || text === 'Fails.') {
    throw new Error(`qBittorrent login failed: HTTP ${response.status} ${text}`);
  }
  if (response.status !== 200 && response.status !== 204) {
    throw new Error(`qBittorrent login failed: HTTP ${response.status} ${text}`);
  }
  const setCookie = response.headers.getSetCookie?.() ?? [];
  if (setCookie.length > 0) {
    cookie = setCookie.map((entry) => entry.split(';')[0]).join('; ');
  } else {
    const legacy = response.headers.get('set-cookie');
    if (legacy) {
      cookie = legacy.split(';')[0] ?? '';
    }
  }
  console.log('==> qBittorrent: authenticated');
}

function authHeaders(): Record<string, string> {
  return cookie ? { Cookie: cookie } : {};
}

export function qBittorrentHostForArr(config: BootstrapConfig): string {
  return hostnameFromServiceUrl(config.urls.qBittorrent);
}
