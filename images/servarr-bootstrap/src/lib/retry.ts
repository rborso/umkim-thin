export interface RetryOptions {
  attempts?: number;
  delayMs?: number;
  backoffFactor?: number;
  retryOn?: (error: unknown) => boolean;
}

const defaultRetryOn = (error: unknown): boolean => {
  if (error instanceof Error && error.name === 'AbortError') {
    return true;
  }
  if (
    error instanceof Error &&
    error.name === 'HttpError' &&
    'status' in error &&
    typeof (error as { status: number }).status === 'number' &&
    (error as { status: number }).status >= 500
  ) {
    return true;
  }
  if (error instanceof TypeError) {
    return true;
  }
  return false;
};

export async function retryAsync<T>(
  fn: () => Promise<T>,
  options: RetryOptions = {},
): Promise<T> {
  const attempts = options.attempts ?? 5;
  const delayMs = options.delayMs ?? 5_000;
  const backoffFactor = options.backoffFactor ?? 1;
  const retryOn = options.retryOn ?? defaultRetryOn;

  let lastError: unknown;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      return await fn();
    } catch (error) {
      lastError = error;
      if (attempt >= attempts || !retryOn(error)) {
        throw error;
      }
      const wait = delayMs * backoffFactor ** (attempt - 1);
      await sleep(wait);
    }
  }
  throw lastError;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
