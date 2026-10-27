export interface BootstrapComponents {
  sonarr: boolean;
  radarr: boolean;
  lidarr: boolean;
  jellyfin: boolean;
  seerr: boolean;
  prowlarr: boolean;
  qbittorrent: boolean;
  flaresolverr: boolean;
}

export interface BootstrapConfig {
  qbtUsername: string;
  qbtPassword: string;
  jellyfinAdminUser: string;
  jellyfinAdminPassword: string;
  jellyfinLanguage: string;
  servarrUsername: string;
  servarrPassword: string;
  defaultMediaLanguage: string;
  defaultQualityCutoff: string;
  seerrAutoApprove: boolean;
  jellyfinLibraryScanMinutes: number;
  components: BootstrapComponents;
  urls: {
    qBittorrent: string;
    prowlarr: string;
    sonarr: string;
    radarr: string;
    lidarr: string;
    jellyfin: string;
    seerr: string;
    flaresolverr: string;
  };
  configPaths: {
    prowlarr: string;
    sonarr: string;
    radarr: string;
    lidarr: string;
  };
  apiKeys: {
    prowlarr: string;
    sonarr: string;
    radarr: string;
    lidarr: string;
  };
  artifactsDir: string;
  jellyfinApiKeyFile: string;
}

function required(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`Missing required environment variable: ${name}`);
  }
  return value;
}

function optionalWhen(enabled: boolean, name: string, fallback = ''): string {
  if (!enabled) {
    return fallback;
  }
  return required(name);
}

function parseBool(name: string, defaultValue: boolean): boolean {
  const value = process.env[name]?.trim().toLowerCase();
  if (!value) {
    return defaultValue;
  }
  return value === 'true' || value === '1' || value === 'yes';
}

function loadComponents(): BootstrapComponents {
  return {
    sonarr: parseBool('BOOTSTRAP_ENABLE_SONARR', true),
    radarr: parseBool('BOOTSTRAP_ENABLE_RADARR', true),
    // Lidarr is not part of the default stack; it is only configured when the
    // chart explicitly sets BOOTSTRAP_ENABLE_LIDARR=true (the local wrapper does
    // for every app it manages).
    lidarr: parseBool('BOOTSTRAP_ENABLE_LIDARR', false),
    jellyfin: parseBool('BOOTSTRAP_ENABLE_JELLYFIN', true),
    seerr: parseBool('BOOTSTRAP_ENABLE_SEERR', true),
    prowlarr: parseBool('BOOTSTRAP_ENABLE_PROWLARR', true),
    qbittorrent: parseBool('BOOTSTRAP_ENABLE_QBITTORRENT', true),
    flaresolverr: parseBool('BOOTSTRAP_ENABLE_FLARESOLVERR', true),
  };
}

export function loadConfig(): BootstrapConfig {
  const components = loadComponents();
  const mediaLanguage = (process.env.DEFAULT_MEDIA_LANGUAGE?.trim() || 'en').toLowerCase();
  const needsJellyfinCreds = components.jellyfin || components.seerr;
  const needsArrPassword =
    components.prowlarr || components.sonarr || components.radarr || components.lidarr;

  return {
    qbtUsername: process.env.QBITTORRENT_USERNAME?.trim() || 'admin',
    qbtPassword: optionalWhen(components.qbittorrent || components.prowlarr, 'QBITTORRENT_PASSWORD'),
    jellyfinAdminUser: optionalWhen(needsJellyfinCreds, 'JELLYFIN_ADMIN_USER', 'admin'),
    jellyfinAdminPassword: optionalWhen(needsJellyfinCreds, 'JELLYFIN_ADMIN_PASSWORD'),
    jellyfinLanguage: process.env.JELLYFIN_LANGUAGE?.trim() || mediaLanguage,
    servarrUsername: process.env.SERVARR_USERNAME?.trim() || 'admin',
    servarrPassword: optionalWhen(needsArrPassword, 'SERVARR_PASSWORD'),
    defaultMediaLanguage: mediaLanguage,
    defaultQualityCutoff: process.env.DEFAULT_QUALITY_CUTOFF?.trim() || '720p',
    seerrAutoApprove: process.env.SEERR_AUTO_APPROVE === 'true',
    jellyfinLibraryScanMinutes: parseInt(process.env.JELLYFIN_LIBRARY_SCAN_MINUTES ?? '15', 10) || 15,
    components,
    urls: {
      qBittorrent:
        process.env.QBITTORRENT_URL ??
        'http://servarr-qbittorrent-web.servarr.svc.cluster.local:8080',
      prowlarr:
        process.env.PROWLARR_URL ??
        'http://servarr-prowlarr.servarr.svc.cluster.local:9696',
      sonarr:
        process.env.SONARR_URL ?? 'http://servarr-sonarr.servarr.svc.cluster.local',
      radarr:
        process.env.RADARR_URL ?? 'http://servarr-radarr.servarr.svc.cluster.local',
      lidarr:
        process.env.LIDARR_URL ?? 'http://servarr-lidarr.servarr.svc.cluster.local',
      jellyfin:
        process.env.JELLYFIN_URL ??
        'http://servarr-jellyfin.servarr.svc.cluster.local:8096',
      seerr:
        process.env.SEERR_URL ?? 'http://servarr-seerr.servarr.svc.cluster.local:5055',
      flaresolverr:
        process.env.FLARESOLVERR_URL ??
        'http://servarr-flaresolverr.servarr.svc.cluster.local:8191',
    },
    configPaths: {
      prowlarr: process.env.PROWLARR_CONFIG_PATH ?? '/config/prowlarr/config.xml',
      sonarr: process.env.SONARR_CONFIG_PATH ?? '/config/sonarr/config.xml',
      radarr: process.env.RADARR_CONFIG_PATH ?? '/config/radarr/config.xml',
      lidarr: process.env.LIDARR_CONFIG_PATH ?? '/config/lidarr/config.xml',
    },
    // Preferred source for the *arr API keys. The chart generates them, seeds
    // each app's config.xml with them and hands them over as env vars, so the
    // bootstrap needs no config-volume mount (which is what forced the config
    // volumes to be ReadWriteMany). configPaths stays as the fallback for a
    // standalone run that does mount the volumes.
    apiKeys: {
      prowlarr: process.env.PROWLARR_API_KEY?.trim() ?? '',
      sonarr: process.env.SONARR_API_KEY?.trim() ?? '',
      radarr: process.env.RADARR_API_KEY?.trim() ?? '',
      lidarr: process.env.LIDARR_API_KEY?.trim() ?? '',
    },
    artifactsDir: process.env.BOOTSTRAP_ARTIFACTS_DIR ?? '/tmp/bootstrap-artifacts',
    jellyfinApiKeyFile: process.env.JELLYFIN_API_KEY_FILE ?? '/tmp/jellyfin-api-key',
  };
}
