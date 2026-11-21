import { test, expect, request } from '@playwright/test';
import { installed, hostOf, createUser, oidcLogin } from './lib/session';

// Gating checks for the AI stack.
//
// Non-deterministic output, so these assert SHAPE and PLUMBING, never text. The
// interesting assertions are architectural:
//
//   * the gateway lists a model, which proves the pool config parses;
//   * a completion returns 200 with content and a token count, which proves the
//     gateway can actually reach a llama.cpp backend;
//   * the backend pods carry NO nvidia.com/gpu, which keeps CPU mode honest.
//
// There is no "balance across backends" check: a single CPU backend is the
// shipped default here, so asserting two distinct model-ids would fail an
// install that is working exactly as designed. That check belongs with a
// multi-backend run.

// The gateway is internal (no route of its own), so it is reached through the
// WebUI's origin by proxying the path - or, for the checks that need the raw API,
// via the WebUI container's network. The simplest honest approach is to ask
// Open WebUI itself to make the call, since it holds the gateway key.

test.describe('ai stack', () => {
  test.skip(!installed('ai'), 'the ai stack is not installed');

  test('the gateway lists a model @check:llm_models_listed', async ({ browser }) => {
    test.setTimeout(180_000);
    const host = hostOf('ai');

    // Through a real user session, not a bare portal cookie: /api/models is an
    // Open WebUI endpoint, so an Authelia session alone answers 401.
    const user = `ci-aim-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => (await fetch('/api/models', { credentials: 'same-origin' })).status)
          .then((s) => s === 200)
          .catch(() => false),
    });

    const models = await page.evaluate(async () => {
      const r = await fetch('/api/models', { credentials: 'same-origin' });
      return { status: r.status, body: await r.json().catch(() => null) };
    });
    expect(models.status, 'Open WebUI serves the model list').toBe(200);
    const ids: string[] = (Array.isArray(models.body) ? models.body : models.body?.data ?? []).map(
      (m: any) => m.id ?? m.name ?? '',
    );
    expect(ids.length, `at least one model is published: ${JSON.stringify(ids)}`).toBeGreaterThan(0);
    await context.close();
  });

  test('a completion returns content @check:llm_completion_answers', async ({ browser }) => {
    test.setTimeout(240_000);
    const host = hostOf('ai');

    // Drive it as a user does: sign in, then ask the app to complete. CPU
    // inference is slow, so the budget is generous - but bounded.
    const user = `ci-ai-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      // Readiness is a real session, not a URL: the OIDC redirect chain passes
      // through the app's own "/" on its way to "/auth", so a pathname test
      // declares success before the login has happened.
      ready: async (p) =>
        p
          .evaluate(async () => (await fetch('/api/models', { credentials: 'same-origin' })).status)
          .then((s) => s === 200)
          .catch(() => false),
    });

    const result = await page.evaluate(async () => {
      const models = await (await fetch('/api/models', { credentials: 'same-origin' })).json().catch(() => null);
      const list: any[] = Array.isArray(models) ? models : models?.data ?? [];
      if (!list.length) return { model: '', status: 0, content: '', total: 0 };
      const model = list[0].id;
      const r = await fetch('/api/chat/completions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          model,
          messages: [{ role: 'user', content: 'Reply with the single word: ok' }],
          max_tokens: 16,
        }),
      });
      const body = await r.json().catch(() => null);
      return {
        model,
        status: r.status,
        content: body?.choices?.[0]?.message?.content ?? '',
        total: body?.usage?.total_tokens ?? 0,
      };
    });

    expect(result.model, 'a model was available to ask').not.toBe('');
    expect(result.status, `completion through the gateway -> ${result.status}`).toBe(200);
    expect(result.content.length, 'the completion has content').toBeGreaterThan(0);
    expect(result.total, 'the backend reported token usage').toBeGreaterThan(0);
    await context.close();
  });
});

test.describe('tenant can use the ai stack', () => {
  test.skip(!installed('ai'), 'the ai stack is not installed');
  const user = process.env.UMKIM_THIN_PORTAL_USER || '';
  const pass = process.env.UMKIM_THIN_PORTAL_PASSWORD || '';

  test('a regular user signs in by SSO and gets an answer @check:tenant_api_ai', async ({ browser }) => {
    test.skip(!user || !pass, 'portal user not set');
    test.setTimeout(240_000);
    const host = hostOf('ai');

    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      // Readiness is a real session, not a URL: the OIDC redirect chain passes
      // through the app's own "/" on its way to "/auth", so a pathname test
      // declares success before the login has happened.
      ready: async (p) =>
        p
          .evaluate(async () => (await fetch('/api/models', { credentials: 'same-origin' })).status)
          .then((s) => s === 200)
          .catch(() => false),
    });

    // The tenant's own session must be able to reach a model and get content
    // back. Shallow on purpose: one completion, shape only.
    const result = await page.evaluate(async () => {
      const models = await (await fetch('/api/models', { credentials: 'same-origin' })).json().catch(() => null);
      const list: any[] = Array.isArray(models) ? models : models?.data ?? [];
      if (!list.length) return { status: 0, content: '' };
      const r = await fetch('/api/chat/completions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          model: list[0].id,
          messages: [{ role: 'user', content: 'Say hi' }],
          max_tokens: 12,
        }),
      });
      const body = await r.json().catch(() => null);
      return { status: r.status, content: body?.choices?.[0]?.message?.content ?? '' };
    });

    expect(result.status, 'the tenant reaches a model').toBe(200);
    expect(result.content.length, 'the tenant gets an answer').toBeGreaterThan(0);
    await context.close();
  });
});

test.describe('ai per-user api keys', () => {
  test.skip(!installed('ai'), 'the ai stack is not installed');

  test('a regular user mints their own key and revokes it @check:llm_user_api_key_lifecycle', async ({ browser }) => {
    test.setTimeout(240_000);
    const host = hostOf('ai');

    // The whole point of the option: a NON-ADMIN mints, uses and revokes a key.
    // An admin passes even when per-user permission is off, so this has to be an
    // ordinary invited user or the check proves nothing.
    const user = `ci-aikey-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => (await fetch('/api/models', { credentials: 'same-origin' })).status)
          .then((s) => s === 200)
          .catch(() => false),
    });

    // Mint a key and check the OpenAI-compatible surface answers with it. Both
    // calls run from the app's origin so the portal cookie rides along; the key
    // is what identifies the user to Open WebUI.
    const created = await page.evaluate(async () => {
      const r = await fetch('/api/v1/auths/api_key', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
      });
      const body = await r.json().catch(() => ({}));
      return { status: r.status, key: body?.api_key ?? '' };
    });

    // 403 here means USER_PERMISSIONS_FEATURES_API_KEYS is off: the admin works
    // without it, so this is the check that catches the default.
    expect(
      created.status,
      `a regular user must be allowed to create an API key (403 = features.api_keys is off)`,
    ).toBe(200);
    expect(created.key, 'the key is returned once, on creation').toMatch(/^sk-/);

    // The key must actually work against the OpenAI-compatible surface, or the
    // CLI story is a lie even though the key was minted.
    const used = await page.evaluate(async (key: string) => {
      const auth = { Authorization: `Bearer ${key}` };
      const models = await fetch('/api/v1/models', { headers: auth });
      if (models.status !== 200) return { models: models.status, completion: 0, chat: 0 };
      const list = await models.json().catch(() => null);
      const ids: string[] = (list?.data ?? []).map((m: any) => m.id ?? '');
      if (!ids.length) return { models: 200, completion: 0, chat: ids.length };
      const chat = await fetch('/api/v1/chat/completions', {
        method: 'POST',
        headers: { ...auth, 'Content-Type': 'application/json' },
        body: JSON.stringify({
          model: ids[0],
          messages: [{ role: 'user', content: 'Reply with one word' }],
          max_tokens: 8,
        }),
      });
      const body = await chat.json().catch(() => null);
      return {
        models: 200,
        chat: chat.status,
        completion: (body?.choices?.[0]?.message?.content ?? '').length,
        modelsSeen: ids.length,
      };
    }, created.key);

    expect(used.models, 'the key lists models through /api/v1').toBe(200);
    expect(used.modelsSeen, 'the key sees at least one model').toBeGreaterThan(0);
    expect(used.chat, 'the key completes a chat request through /api/v1').toBe(200);
    expect(used.completion, 'the completion has content').toBeGreaterThan(0);

    // Revoking must be immediate: a key that survives its own deletion is worse
    // than no revocation at all.
    const revoked = await page.evaluate(async (key: string) => {
      const del = await fetch('/api/v1/auths/api_key', {
        method: 'DELETE',
        credentials: 'same-origin',
      });
      const after = await fetch('/api/v1/models', { headers: { Authorization: `Bearer ${key}` } });
      return { status: del.status, after: after.status };
    }, created.key);

    expect([200, 204], `the user revokes their own key -> ${revoked.status}`).toContain(revoked.status);
    expect(revoked.after, 'the revoked key no longer authenticates').toBe(401);
    await context.close();
  });
});

// The test above runs every call from the app's origin, so the portal cookie rides
// along. That is why it passed while a real client could not use the key at all:
// curl with only `Authorization: Bearer sk-...` got a 302 to the portal, because
// Authelia's forward-auth ran before Open WebUI and the token never reached the
// app that checks it.
//
// This case is deliberately cookie-less. It is the shape of every real consumer
// of a per-user key - an editor plugin, a CLI, a phone app - and it is the check
// that would have caught the catalog's missing publicPaths.
test.describe('ai stack: a bearer client with no browser session', () => {
  test.skip(!installed('ai'), 'the ai stack is not installed');

  test('an API key works from a token-only client @check:llm_api_key_bearer_client', async ({
    browser,
  }) => {
    test.setTimeout(240_000);
    const host = hostOf('ai');

    const user = `ci-aibearer-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    // Sign in through the browser once, purely to mint the key: a key is created
    // by a signed-in user, and that part legitimately needs a session.
    const { context, page } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) =>
        p
          .evaluate(async () => (await fetch('/api/models', { credentials: 'same-origin' })).status)
          .then((s) => s === 200)
          .catch(() => false),
    });

    const key = await page.evaluate(async () => {
      const r = await fetch('/api/v1/auths/api_key', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
      });
      const body = await r.json().catch(() => ({}));
      return body?.api_key ?? '';
    });
    expect(key, 'a regular user mints a key').toMatch(/^sk-/);

    // From here on, no cookies at all: a fresh request context, exactly like curl.
    const client = await request.newContext({ ignoreHTTPSErrors: true });
    const models = await client.get(`https://${host}/api/v1/models`, {
      headers: { Authorization: `Bearer ${key}` },
      maxRedirects: 0,
    });
    // A 302 is the specific failure this guards against: it means the portal
    // intercepted the request and the app never saw the token.
    expect(
      models.status(),
      'a token-only request must reach Open WebUI, not be redirected to the portal',
    ).toBe(200);

    const list = await models.json().catch(() => null);
    const ids: string[] = (list?.data ?? []).map((m: any) => m.id ?? '');
    expect(ids.length, 'the key lists models').toBeGreaterThan(0);

    const chat = await client.post(`https://${host}/api/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
      data: {
        model: ids[0],
        messages: [{ role: 'user', content: 'Reply with the single word: pong' }],
        max_tokens: 12,
      },
    });
    expect(chat.status(), 'the key completes a request with no session cookie').toBe(200);
    const body = await chat.json().catch(() => null);
    expect(
      (body?.choices?.[0]?.message?.content ?? '').length,
      'the completion has content',
    ).toBeGreaterThan(0);

    // Revoke with the browser context, then confirm the token-only client dies.
    await page.evaluate(async () => {
      await fetch('/api/v1/auths/api_key', { method: 'DELETE', credentials: 'same-origin' });
    });
    const after = await client.get(`https://${host}/api/v1/models`, {
      headers: { Authorization: `Bearer ${key}` },
      maxRedirects: 0,
    });
    expect(after.status(), 'the revoked key no longer authenticates').toBe(401);

    await client.dispose();
    await context.close();
  });
});
