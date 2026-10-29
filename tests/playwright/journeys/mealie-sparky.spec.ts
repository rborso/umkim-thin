import { test, expect } from '@playwright/test';
import {
  installed,
  hostOf,
  portalSession,
  consoleSession,
  oidcLogin,
  appAdmin,
  CONSOLE_HOST,
  USER,
  PASS,
} from '../lib/session';

// Cross-app journey: create a recipe in Mealie, run the console's one-click
// Mealie integration, then use SparkyFitness's own Mealie provider to find the
// recipe. This is opt-in because it changes the SparkyFitness provider state.
test.describe('mealie to sparkyfitness integration', () => {
  test.skip(process.env.UMKIM_THIN_INTEGRATIONS !== '1', 'set --integrations to run cross-app journeys');
  test.skip(!installed('mealie') || !installed('sparkyfitness'), 'mealie and sparkyfitness are both required');
  test.skip(!PASS, 'UMKIM_THIN_PASSWORD not set');

  const mealieHost = hostOf('mealie');
  const sparkyHost = hostOf('sparkyfitness');
  const admin = appAdmin('mealie');
  test.skip(!admin.pass, 'UMKIM_THIN_MEALIE_ADMIN_PASSWORD not set');

  test('the one-click provider makes a Mealie recipe searchable in SparkyFitness', async ({ browser }) => {
    test.setTimeout(180_000);

    const marker = `ci-recipe-${Date.now()}`;
    let slug = '';
    let mealieToken = '';
    let mealie: Awaited<ReturnType<typeof portalSession>> | undefined;
    let consoleCtx: Awaited<ReturnType<typeof consoleSession>> | undefined;
    let sparky: Awaited<ReturnType<typeof oidcLogin>> | undefined;

    try {
      mealie = await portalSession();
      const login = await mealie.post(`https://${mealieHost}/api/auth/token`, {
        form: { username: admin.user, password: admin.pass },
      });
      expect([200, 201], `Mealie admin login -> ${login.status()}`).toContain(login.status());
      mealieToken = (await login.json()).access_token;
      expect(mealieToken, 'Mealie access token').toBeTruthy();
      const headers = { Authorization: `Bearer ${mealieToken}` };

      const created = await mealie.post(`https://${mealieHost}/api/recipes`, {
        headers,
        data: { name: marker },
      });
      expect([200, 201], `create Mealie recipe -> ${created.status()}`).toContain(created.status());
      const createdBody = await created.json();
      slug = typeof createdBody === 'string' ? createdBody : createdBody.slug;
      expect(slug, 'Mealie recipe slug').toBeTruthy();

      // The console action reads the Mealie admin Secret and SparkyFitness's
      // encryption key; no API key or browser input is needed.
      consoleCtx = await consoleSession();
      const connected = await consoleCtx.post(
        `https://${CONSOLE_HOST}/api/apps/sparkyfitness/integrations/mealie`,
      );
      expect([200, 201], `console integration -> ${connected.status()}`).toContain(connected.status());
      const result = await connected.json();
      expect(result.connected, 'the provider is connected').toBeTruthy();

      sparky = await oidcLogin(browser, sparkyHost, {
        credentials: { username: USER, password: PASS },
        ready: async (page) =>
          page
            .evaluate(async () => {
              const r = await fetch('/api/auth/get-session', { credentials: 'same-origin' });
              if (!r.ok) return false;
              const body = await r.json().catch(() => null);
              return Boolean(body?.user?.email);
            })
            .catch(() => false),
      });

      const providers = await sparky.context.request.get(`https://${sparkyHost}/api/external-providers`);
      expect(providers.status(), 'SparkyFitness provider list').toBe(200);
      const list = (await providers.json()) as Array<{ id: string; provider_type: string; base_url?: string }>;
      const provider = list.find((p) => p.provider_type === 'mealie');
      expect(provider, 'a Mealie provider is visible in SparkyFitness').toBeTruthy();

      const found = await sparky.context.request.get(
        `https://${sparkyHost}/api/foods/mealie/search?query=${encodeURIComponent(marker)}`,
        { headers: { 'x-provider-id': provider!.id } },
      );
      expect(found.status(), `Mealie search through SparkyFitness -> ${found.status()}`).toBe(200);
      const foods = await found.json();
      const names = (Array.isArray(foods) ? foods : foods.items ?? []).map((f: any) => String(f.name ?? ''));
      expect(names.some((name: string) => name.includes(marker)), 'the recipe is searchable in SparkyFitness').toBeTruthy();
    } finally {
      if (slug && mealie && mealieToken) {
        await mealie
          .delete(`https://${mealieHost}/api/recipes/${encodeURIComponent(slug)}`, {
            headers: { Authorization: `Bearer ${mealieToken}` },
          })
          .catch(() => undefined);
      }
      if (sparky) await sparky.context.close().catch(() => undefined);
      if (consoleCtx) await consoleCtx.dispose().catch(() => undefined);
      if (mealie) await mealie.dispose().catch(() => undefined);
    }
  });
});
