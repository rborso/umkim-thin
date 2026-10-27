import { BootstrapConfig } from '../config.js';
import { httpJson, httpJsonWithRetry } from '../lib/http.js';

const HUNGARIAN_FORMAT_NAME = 'Hungarian (HuN)';

interface QualityItem {
  quality?: { id: number; name: string; resolution?: number };
  name?: string;
  items?: QualityItem[];
  allowed?: boolean;
}

interface QualityProfile {
  id: number;
  name: string;
  cutoff: number;
  items: QualityItem[];
  formatItems?: Array<{ format: number; name?: string; score: number }>;
  language?: { id: number; name: string };
}

interface CustomFormat {
  id: number;
  name: string;
  specifications?: Array<Record<string, unknown>>;
}

const HUNGARIAN_LANGUAGE_ID = 22;
const HUNGARIAN_FORMAT_SCORE = 10_000;

// Radarr's language ids are stable across the v3 API. The console offers the
// same set; keep the names in step with `internal/app/servarr.go`.
const LANGUAGE_IDS: Record<string, { id: number; name: string }> = {
  ar: { id: 31, name: 'Arabic' },
  cs: { id: 25, name: 'Czech' },
  da: { id: 6, name: 'Danish' },
  de: { id: 4, name: 'German' },
  el: { id: 20, name: 'Greek' },
  en: { id: 1, name: 'English' },
  es: { id: 3, name: 'Spanish' },
  fi: { id: 16, name: 'Finnish' },
  fr: { id: 2, name: 'French' },
  he: { id: 23, name: 'Hebrew' },
  hu: { id: HUNGARIAN_LANGUAGE_ID, name: 'Hungarian' },
  it: { id: 5, name: 'Italian' },
  ja: { id: 8, name: 'Japanese' },
  ko: { id: 21, name: 'Korean' },
  nl: { id: 7, name: 'Dutch' },
  no: { id: 15, name: 'Norwegian' },
  pl: { id: 12, name: 'Polish' },
  pt: { id: 18, name: 'Portuguese' },
  ro: { id: 27, name: 'Romanian' },
  ru: { id: 11, name: 'Russian' },
  sk: { id: 35, name: 'Slovak' },
  sv: { id: 14, name: 'Swedish' },
  tr: { id: 17, name: 'Turkish' },
  uk: { id: 32, name: 'Ukrainian' },
  zh: { id: 10, name: 'Chinese' },
};

const QUALITY_ORDER = ['480p', '720p', '1080p', '2160p'] as const;
type QualityCutoff = (typeof QUALITY_ORDER)[number];

const QUALITY_RESOLUTIONS: Record<QualityCutoff, number> = {
  '480p': 480,
  '720p': 720,
  '1080p': 1080,
  '2160p': 2160,
};

const CUTOFF_NAMES: Record<QualityCutoff, string[]> = {
  '480p': ['Bluray-480p', 'WEB 480p', 'WEBRip-480p', 'SDTV', 'DVD'],
  '720p': ['Bluray-720p', 'WEB 720p', 'WEBRip-720p', 'HDTV-720p'],
  '1080p': ['Bluray-1080p', 'WEB 1080p', 'WEBRip-1080p', 'HDTV-1080p'],
  '2160p': ['Bluray-2160p', 'WEB 2160p', 'WEBRip-2160p', 'HDTV-2160p'],
};

/** Normalizes a console value such as "1080" or "1080p" to a known target. */
export function normalizeQualityCutoff(value: string | undefined): QualityCutoff {
  const raw = (value ?? '').trim().toLowerCase();
  if (/(2160|4k|uhd)/.test(raw)) {
    return '2160p';
  }
  if (/1080/.test(raw)) {
    return '1080p';
  }
  if (/720/.test(raw)) {
    return '720p';
  }
  if (/(480|576|sd)/.test(raw)) {
    return '480p';
  }
  return '720p';
}

/** The profile name used in Sonarr/Radarr and Seerr. */
export function qualityProfileName(value: string | undefined): string {
  const quality = normalizeQualityCutoff(value);
  switch (quality) {
    case '480p':
      return 'SD-480p';
    case '2160p':
      return 'UHD-2160p';
    default:
      return `HD-${quality}`;
  }
}

function itemName(item: QualityItem): string {
  return item.quality?.name ?? item.name ?? '';
}

function resolutionFromName(name: string): number {
  const lower = name.toLowerCase();
  if (/(2160|4k|uhd)/.test(lower)) {
    return 2160;
  }
  if (/1080/.test(lower)) {
    return 1080;
  }
  if (/720/.test(lower)) {
    return 720;
  }
  if (/(576|480|sd|dvd|regional|sdtv)/.test(lower)) {
    return 480;
  }
  return 0;
}

function itemResolution(item: QualityItem): number {
  return item.quality?.resolution || resolutionFromName(itemName(item));
}

function isAllowedQuality(item: QualityItem, target: QualityCutoff): boolean {
  const resolution = itemResolution(item);
  if (resolution > 0) {
    return resolution <= QUALITY_RESOLUTIONS[target];
  }
  // The old 720p profile accepted DVD/SD releases which report resolution 0 in
  // Radarr's schema. Keep that behaviour, but never enable unknown/cam/ts.
  return /dvd|sd|regional/i.test(itemName(item));
}

function withAllowed(items: QualityItem[], target: QualityCutoff): QualityItem[] {
  return items.map((item) => ({
    ...item,
    allowed: isAllowedQuality(item, target),
    ...(item.items ? { items: withAllowed(item.items, target) } : {}),
  }));
}

function chooseCutoff(items: QualityItem[], target: QualityCutoff): number {
  const targetResolution = QUALITY_RESOLUTIONS[target];
  for (const name of CUTOFF_NAMES[target]) {
    const match = items.find((item) => item.quality?.name === name && item.allowed);
    if (match?.quality) {
      return match.quality.id;
    }
  }

  const fallback =
    items.find(
      (item) =>
        item.quality &&
        item.allowed &&
        itemResolution(item) === targetResolution,
    ) ??
    items.find((item) => item.quality && item.allowed && itemResolution(item) > 0);
  if (fallback?.quality) {
    return fallback.quality.id;
  }
  throw new Error(`could not determine ${target} cutoff quality from schema`);
}

function preferredLanguage(
  config: BootstrapConfig,
  appLabel: string,
): { id: number; name: string } | undefined {
  if (appLabel !== 'Radarr' || !config.defaultMediaLanguage) {
    return undefined;
  }
  return LANGUAGE_IDS[config.defaultMediaLanguage.toLowerCase()];
}

export async function applyDefaultProfiles(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
  label: string,
): Promise<number> {
  const target = normalizeQualityCutoff(config.defaultQualityCutoff);
  const profileName = qualityProfileName(target);
  const profileId = await ensureQualityProfile(
    base,
    headers,
    label,
    config,
    target,
    profileName,
  );
  await ensureHungarianCustomFormat(
    base,
    headers,
    profileId,
    label,
    config.defaultMediaLanguage.toLowerCase() === 'hu',
  );
  return profileId;
}

async function ensureQualityProfile(
  base: string,
  headers: Record<string, string>,
  label: string,
  config: BootstrapConfig,
  target: QualityCutoff,
  profileName: string,
): Promise<number> {
  const profiles = await httpJson<QualityProfile[]>('GET', `${base}/api/v3/qualityprofile`, {
    headers,
  });
  const language = preferredLanguage(config, label);
  const existing = profiles.find((profile) => profile.name === profileName);
  if (existing) {
    const full = await httpJson<QualityProfile>(
      'GET',
      `${base}/api/v3/qualityprofile/${existing.id}`,
      { headers },
    );
    const items = withAllowed(full.items ?? [], target);
    const cutoff = chooseCutoff(items, target);
    const languageChanged = Boolean(language && full.language?.id !== language.id);
    const changed =
      full.cutoff !== cutoff ||
      JSON.stringify(full.items) !== JSON.stringify(items) ||
      languageChanged;
    if (changed) {
      await httpJsonWithRetry('PUT', `${base}/api/v3/qualityprofile/${existing.id}`, {
        headers,
        body: {
          ...full,
          items,
          cutoff,
          ...(language ? { language } : {}),
        },
      });
      console.log(
        `==> ${label}: updated quality profile ${profileName} (${target}${language ? `, ${language.name}` : ''})`,
      );
    } else {
      console.log(`==> ${label}: quality profile ${profileName} already exists`);
    }
    return existing.id;
  }

  const schema = await httpJson<QualityProfile>('GET', `${base}/api/v3/qualityprofile/schema`, {
    headers,
  });
  const items = withAllowed(schema.items ?? [], target);
  const cutoff = chooseCutoff(items, target);

  // Sonarr/Radarr reject a profile that omits any existing custom format. A
  // fresh profile starts with every format at score 0; the Hungarian one is
  // raised below when that language is selected.
  const formats = await httpJson<CustomFormat[]>('GET', `${base}/api/v3/customformat`, {
    headers,
  });
  const formatItems = formats.map((format) => ({
    format: format.id,
    name: format.name,
    score: 0,
  }));

  const created = await httpJsonWithRetry<QualityProfile>(
    'POST',
    `${base}/api/v3/qualityprofile`,
    {
      headers,
      body: {
        name: profileName,
        upgradeAllowed: true,
        cutoff,
        items,
        minFormatScore: 0,
        cutoffFormatScore: 0,
        formatItems,
        ...(language ? { language } : {}),
      },
    },
  );
  console.log(`==> ${label}: created quality profile ${profileName} (${target})`);
  return created.id;
}

async function ensureHungarianCustomFormat(
  base: string,
  headers: Record<string, string>,
  profileId: number,
  label: string,
  enabled: boolean,
): Promise<void> {
  const formats = await httpJson<CustomFormat[]>('GET', `${base}/api/v3/customformat`, {
    headers,
  });
  let format = formats.find((item) => item.name === HUNGARIAN_FORMAT_NAME);

  if (enabled && !format) {
    format = await httpJsonWithRetry<CustomFormat>('POST', `${base}/api/v3/customformat`, {
      headers,
      body: {
        name: HUNGARIAN_FORMAT_NAME,
        includeCustomFormatWhenRenaming: false,
        specifications: [
          {
            name: HUNGARIAN_FORMAT_NAME,
            implementation: 'ReleaseTitleSpecification',
            negate: false,
            required: false,
            fields: [
              {
                order: 0,
                name: 'value',
                value: '(?i)(\\bHuN\\b|Hungarian|\\bhun\\b)',
              },
            ],
          },
        ],
      },
    });
    formats.push(format);
    console.log(`==> ${label}: created custom format ${HUNGARIAN_FORMAT_NAME}`);
  }

  const profile = await httpJson<QualityProfile>(
    'GET',
    `${base}/api/v3/qualityprofile/${profileId}`,
    { headers },
  );
  const current = profile.formatItems ?? [];
  const byId = new Map(current.map((item) => [item.format, item]));
  const known = new Set(formats.map((item) => item.id));
  let changed =
    current.length !== formats.length ||
    current.some((item) => !known.has(item.format));

  // Every custom format must be present in the profile, even at score 0.
  // Only the Hungarian one changes score: 10000 when it is the chosen language,
  // 0 otherwise. Keeping the item (rather than deleting it) is required by the
  // *arr profile validator when custom formats exist.
  const formatItems = formats.map((item) => {
    const existing = byId.get(item.id);
    const score =
      format && item.id === format.id
        ? enabled
          ? HUNGARIAN_FORMAT_SCORE
          : 0
        : existing?.score ?? 0;
    if (!existing || existing.score !== score || existing.name !== item.name) {
      changed = true;
    }
    return { format: item.id, name: item.name, score };
  });

  if (!changed) {
    return;
  }
  await httpJsonWithRetry('PUT', `${base}/api/v3/qualityprofile/${profileId}`, {
    headers,
    body: {
      ...profile,
      formatItems,
    },
  });
  console.log(
    enabled
      ? `==> ${label}: attached Hungarian custom format to ${profile.name}`
      : `==> ${label}: disabled Hungarian custom format in ${profile.name}`,
  );
}
