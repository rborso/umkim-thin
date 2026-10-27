import { BootstrapConfig } from '../config.js';
import { qBittorrentHostForArr } from './qbittorrent.js';

type SchemaField = { name: string; value?: unknown };
type DownloadSchema = Record<string, unknown> & {
  implementation: string;
  configContract: string;
  fields: SchemaField[];
};

export function buildQBittorrentFields(
  schemaFields: SchemaField[],
  config: BootstrapConfig,
  categoryField: string,
  categoryValue: string,
): SchemaField[] {
  return schemaFields.map((field) => {
    const next = { ...field };
    switch (next.name) {
      case 'host':
        next.value = qBittorrentHostForArr(config);
        break;
      case 'port':
        next.value = 8080;
        break;
      case 'username':
        next.value = config.qbtUsername;
        break;
      case 'password':
        next.value = config.qbtPassword;
        break;
      case categoryField:
        next.value = categoryValue;
        break;
      default:
        break;
    }
    return next;
  });
}

export function buildArrDownloadClientBody(
  schema: DownloadSchema,
  fields: SchemaField[],
  name: string,
): Record<string, unknown> {
  return {
    enable: true,
    name,
    implementation: schema.implementation,
    configContract: schema.configContract,
    protocol: schema.protocol ?? 'torrent',
    priority: 1,
    removeCompletedDownloads: true,
    removeFailedDownloads: true,
    fields,
    tags: [],
  };
}

export function buildProwlarrDownloadClientBody(
  schema: DownloadSchema,
  fields: SchemaField[],
  name: string,
): Record<string, unknown> {
  return {
    enable: true,
    name,
    implementation: schema.implementation,
    configContract: schema.configContract,
    protocol: schema.protocol ?? 'torrent',
    priority: 1,
    fields,
    tags: [],
    categories: [],
  };
}
