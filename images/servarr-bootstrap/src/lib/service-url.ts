/** Parse service base URLs from Helm-injected env (release-prefixed cluster DNS). */

export function hostnameFromServiceUrl(raw: string): string {
  return new URL(raw).hostname;
}

export function portFromServiceUrl(raw: string, fallback = 80): number {
  const url = new URL(raw);
  if (url.port) {
    return Number(url.port);
  }
  return fallback;
}

/** Prowlarr FlareSolverr proxy and similar fields expect http://host:port */
export function inClusterHttpBase(raw: string): string {
  const url = new URL(raw);
  const port = url.port ? `:${url.port}` : '';
  return `${url.protocol}//${url.hostname}${port}`;
}
