import { readFile } from 'node:fs/promises';

export async function parseApiKey(configPath: string): Promise<string> {
  const xml = await readFile(configPath, 'utf8');
  const match = xml.match(/<ApiKey>([^<]+)<\/ApiKey>/);
  if (!match?.[1]) {
    throw new Error(`ApiKey not found in ${configPath}`);
  }
  return match[1];
}

/**
 * The *arr API key. The chart generates it, seeds it into the app's config.xml
 * and passes it as an env var, so the bootstrap does not mount config volumes.
 * Parsing the config file stays as a fallback for a standalone run that mounts
 * the volumes itself.
 */
export async function resolveApiKey(envKey: string, configPath: string): Promise<string> {
  const key = envKey?.trim();
  if (key) {
    return key;
  }
  return parseApiKey(configPath);
}
