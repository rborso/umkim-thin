import { test, expect } from '@playwright/test';
import { APPS, appToken } from './lib/session';

// Self-test for the per-user API token hook in lib/session.ts.
//
// The hook exists so an app whose API needs a token the user mints in their own
// profile (Vikunja, BookStack) can be checked for real instead of being called
// "verified" without evidence - the reason Miniflux could not be verified.
//
// Only one rule matters here, and it is cheap to assert without a cluster: a
// token request the harness cannot satisfy must fail. Returning '' instead
// would call the app unauthenticated, and an assertion against an
// unauthenticated app can pass for entirely the wrong reason.

test.describe('api token hook', () => {
  test('an app with no token adapter fails loudly instead of returning an empty token @check:token_adapter_fails_loudly', async () => {
    // Pick an app that does not declare a token adapter, so this exercises the
    // "no adapter" path whatever the run installed.
    const target = APPS.find((a) => !a.token?.adapter)?.name ?? 'ci-no-such-app';

    let message = '';
    let token: string | undefined;
    try {
      token = await appToken(target);
    } catch (err) {
      message = String(err);
    }

    expect(token, `appToken(${target}) must not resolve without an adapter`).toBeUndefined();
    expect(message, 'the failure names the hook so the fix is obvious').toMatch(/token adapter/);
    expect(message, 'the failure says where adapters live').toMatch(/session\.ts|apps\.json/);
  });
});
