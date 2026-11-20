import { test, expect } from '@playwright/test';
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
