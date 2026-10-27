import { retryAsync } from './retry.js';

export class HttpError extends Error {
  constructor(
    message: string,
    public readonly status: number,
    public readonly body: string,
  ) {
    super(message);
    this.name = 'HttpError';
  }
}

export async function http(
  method: string,
  url: string,
  options: {
    headers?: Record<string, string>;
    body?: unknown;
    form?: Record<string, string>;
    timeoutMs?: number;
  } = {},
): Promise<Response> {
  const headers = new Headers(options.headers ?? {});
  let body: BodyInit | undefined;

  if (options.form) {
    headers.set('Content-Type', 'application/x-www-form-urlencoded');
    body = new URLSearchParams(options.form);
  } else if (options.body !== undefined) {
    if (!headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json');
    }
    body = JSON.stringify(options.body);
  }

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), options.timeoutMs ?? 90_000);

  try {
    return await fetch(url, {
      method,
      headers,
      body,
      signal: controller.signal,
    });
  } finally {
    clearTimeout(timeout);
  }
}

export async function httpJson<T>(
  method: string,
  url: string,
  options: {
    headers?: Record<string, string>;
    body?: unknown;
    timeoutMs?: number;
  } = {},
): Promise<T> {
  const response = await http(method, url, options);
  const text = await response.text();
  if (!response.ok) {
    throw new HttpError(`${method} ${url} failed (HTTP ${response.status})`, response.status, text);
  }
  if (!text) {
    return undefined as T;
  }
  return JSON.parse(text) as T;
}

export async function httpJsonWithRetry<T>(
  method: string,
  url: string,
  options: {
    headers?: Record<string, string>;
    body?: unknown;
    timeoutMs?: number;
    attempts?: number;
    delayMs?: number;
  } = {},
): Promise<T> {
  const { attempts, delayMs, ...httpOptions } = options;
  return retryAsync(() => httpJson<T>(method, url, httpOptions), {
    attempts: attempts ?? 5,
    delayMs: delayMs ?? 5_000,
  });
}
