import { BootstrapConfig } from '../config.js';
import { httpJson, httpJsonWithRetry } from '../lib/http.js';

interface HostConfig {
  id: number;
  authenticationMethod: string;
  authenticationRequired: string;
  username?: string;
  password?: string;
  passwordConfirmation?: string;
  [key: string]: unknown;
}

function isFormsAuthConfigured(config: HostConfig, username: string): boolean {
  const method = config.authenticationMethod?.toLowerCase();
  return method === 'forms' && config.username === username;
}

export async function enableFormsAuthV3(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
  label: string,
): Promise<void> {
  const hostConfig = await httpJson<HostConfig>('GET', `${base}/api/v3/config/host`, { headers });
  if (isFormsAuthConfigured(hostConfig, config.servarrUsername)) {
    console.log(`==> ${label}: Forms authentication already configured`);
    return;
  }

  await httpJsonWithRetry('PUT', `${base}/api/v3/config/host/${hostConfig.id}`, {
    headers,
    body: {
      ...hostConfig,
      authenticationMethod: 'forms',
      authenticationRequired: 'enabled',
      username: config.servarrUsername,
      password: config.servarrPassword,
      passwordConfirmation: config.servarrPassword,
    },
  });
  console.log(`==> ${label}: enabled Forms authentication`);
}

export async function enableFormsAuthV1(
  base: string,
  headers: Record<string, string>,
  config: BootstrapConfig,
  label: string,
): Promise<void> {
  const hostConfig = await httpJson<HostConfig>('GET', `${base}/api/v1/config/host`, { headers });
  if (isFormsAuthConfigured(hostConfig, config.servarrUsername)) {
    console.log(`==> ${label}: Forms authentication already configured`);
    return;
  }

  await httpJsonWithRetry('PUT', `${base}/api/v1/config/host/${hostConfig.id}`, {
    headers,
    body: {
      ...hostConfig,
      authenticationMethod: 'forms',
      authenticationRequired: 'enabled',
      username: config.servarrUsername,
      password: config.servarrPassword,
      passwordConfirmation: config.servarrPassword,
    },
  });
  console.log(`==> ${label}: enabled Forms authentication`);
}
