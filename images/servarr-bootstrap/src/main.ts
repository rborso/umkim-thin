import { mkdir } from 'node:fs/promises';
import { configureLidarr } from './api/lidarr.js';
import { configureQBittorrent } from './api/qbittorrent.js';
import { configureProwlarr, syncProwlarrIndexers } from './api/prowlarr.js';
import { configureRadarr } from './api/radarr.js';
import { configureSonarr } from './api/sonarr.js';
import { loadConfig } from './config.js';
import { resolveApiKey } from './lib/parse-api-key.js';
import {
  waitForArrV3Api,
  waitForLidarrApi,
  waitForProwlarrApi,
  waitForServices,
} from './lib/wait-for-url.js';
import { setupJellyfin } from './ui/jellyfin.setup.js';
import { setupSeerr } from './ui/seerr.setup.js';
import { verifyBootstrap } from './verify.js';

async function main(): Promise<void> {
  const config = loadConfig();
  const { components } = config;
  await mkdir(config.artifactsDir, { recursive: true });

  console.log('==> Bootstrap components:', JSON.stringify(components));

  console.log('==> Phase 0: prerequisite checks');
  const serviceChecks = [];
  if (components.flaresolverr) {
    serviceChecks.push({
      url: `${config.urls.flaresolverr}/health`,
      label: 'FlareSolverr',
    });
  }
  if (components.qbittorrent) {
    serviceChecks.push({ url: `${config.urls.qBittorrent}/`, label: 'qBittorrent' });
  }
  if (components.prowlarr) {
    serviceChecks.push({ url: `${config.urls.prowlarr}/ping`, label: 'Prowlarr' });
  }
  if (components.sonarr) {
    serviceChecks.push({ url: `${config.urls.sonarr}/ping`, label: 'Sonarr' });
  }
  if (components.radarr) {
    serviceChecks.push({ url: `${config.urls.radarr}/ping`, label: 'Radarr' });
  }
  if (components.lidarr) {
    serviceChecks.push({ url: `${config.urls.lidarr}/ping`, label: 'Lidarr' });
  }
  if (components.jellyfin) {
    serviceChecks.push({ url: `${config.urls.jellyfin}/health`, label: 'Jellyfin' });
  }
  if (components.seerr) {
    serviceChecks.push({ url: `${config.urls.seerr}/`, label: 'Seerr' });
  }
  await waitForServices(serviceChecks);

  let prowlarrApiKey: string | undefined;
  let sonarrApiKey: string | undefined;
  let radarrApiKey: string | undefined;
  let lidarrApiKey: string | undefined;

  if (components.prowlarr) {
    prowlarrApiKey = await resolveApiKey(config.apiKeys.prowlarr, config.configPaths.prowlarr);
  }
  if (components.sonarr) {
    sonarrApiKey = await resolveApiKey(config.apiKeys.sonarr, config.configPaths.sonarr);
  }
  if (components.radarr) {
    radarrApiKey = await resolveApiKey(config.apiKeys.radarr, config.configPaths.radarr);
  }
  if (components.lidarr) {
    lidarrApiKey = await resolveApiKey(config.apiKeys.lidarr, config.configPaths.lidarr);
  }

  console.log('==> Phase 0b: *arr API readiness');
  if (components.prowlarr && prowlarrApiKey) {
    await waitForProwlarrApi(config.urls.prowlarr, prowlarrApiKey);
  }
  if (components.sonarr && sonarrApiKey) {
    await waitForArrV3Api(config.urls.sonarr, sonarrApiKey, 'Sonarr');
  }
  if (components.radarr && radarrApiKey) {
    await waitForArrV3Api(config.urls.radarr, radarrApiKey, 'Radarr');
  }
  if (components.lidarr && lidarrApiKey) {
    await waitForLidarrApi(config.urls.lidarr, lidarrApiKey);
  }

  let sonarrProfileId: number | undefined;
  let radarrProfileId: number | undefined;

  if (components.qbittorrent) {
    console.log('==> Phase 1: qBittorrent');
    await configureQBittorrent(config);
  } else {
    console.log('==> Phase 1: qBittorrent (skipped — disabled)');
  }

  if (components.prowlarr && prowlarrApiKey) {
    console.log('==> Phase 2: Prowlarr');
    await configureProwlarr(config, prowlarrApiKey, sonarrApiKey, radarrApiKey);
  } else {
    console.log('==> Phase 2: Prowlarr (skipped — disabled)');
  }

  if (components.sonarr && sonarrApiKey) {
    console.log('==> Phase 3: Sonarr');
    sonarrProfileId = await configureSonarr(config, sonarrApiKey);
  } else {
    console.log('==> Phase 3: Sonarr (skipped — disabled)');
  }

  if (components.radarr && radarrApiKey) {
    console.log('==> Phase 3: Radarr');
    radarrProfileId = await configureRadarr(config, radarrApiKey);
  } else {
    console.log('==> Phase 3: Radarr (skipped — disabled)');
  }

  if (components.prowlarr && prowlarrApiKey) {
    console.log('==> Phase 3a: Prowlarr indexer sync');
    await syncProwlarrIndexers(config, prowlarrApiKey, sonarrApiKey, radarrApiKey);
  }

  if (components.lidarr && lidarrApiKey) {
    console.log('==> Phase 3b: Lidarr');
    await configureLidarr(config, lidarrApiKey);
  } else {
    console.log('==> Phase 3b: Lidarr (skipped — disabled)');
  }

  let jellyfinApiKey: string | undefined;
  if (components.jellyfin) {
    console.log('==> Phase 5: Jellyfin');
    jellyfinApiKey = await setupJellyfin(config);
  } else {
    console.log('==> Phase 5: Jellyfin (skipped — disabled)');
  }

  if (components.seerr && sonarrApiKey && radarrApiKey && sonarrProfileId && radarrProfileId) {
    console.log('==> Phase 6: Seerr');
    await setupSeerr(
      config,
      sonarrApiKey,
      radarrApiKey,
      sonarrProfileId,
      radarrProfileId,
      jellyfinApiKey,
    );
  } else {
    console.log('==> Phase 6: Seerr (skipped — disabled or missing Sonarr/Radarr)');
  }

  console.log('==> Verification');
  await verifyBootstrap(config, {
    prowlarr: prowlarrApiKey,
    sonarr: sonarrApiKey,
    radarr: radarrApiKey,
    jellyfinApiKey,
  });

  console.log('==> Bootstrap completed successfully');
}

main().catch((error) => {
  console.error('Bootstrap failed:', error);
  process.exit(1);
});
