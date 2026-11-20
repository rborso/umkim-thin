import { test, expect } from '@playwright/test';
import { installed, hostOf, createUser, oidcLogin } from '../lib/session';

// AI stack client journey: INFORMATIONAL (excluded from the gate; see
// docs/apps/testing.md). The gate covers the stack through ai.spec.ts (model
// list, completion shape, tenant use) and ci/ai-check.sh (pod spec).
//
// This journey does what a person does: sign in, ask a question, and read the
// answer. CPU inference is slow, so the assertion is on shape, never text.

test.describe('ai stack', () => {
  test.skip(!installed('ai'), 'the ai stack is not installed');

  test('a user signs in and gets an answer to a question', async ({ browser }) => {
    test.setTimeout(300_000);
    const host = hostOf('ai');

    const user = `ci-aij-${Date.now()}`;
    const pass = `Pw${Math.random().toString(36).slice(2, 18)}`;
    await createUser(user, pass, { displayName: user });

    const { page, context } = await oidcLogin(browser, host, {
      credentials: { username: user, password: pass },
      ready: async (p) => {
        let u: URL;
        try {
          u = new URL(p.url());
        } catch {
          return false;
        }
        return u.hostname === host && !u.pathname.startsWith('/oauth') && u.pathname !== '/auth';
      },
    });

    // Ask one question and confirm an answer comes back with usage, which is the
    // evidence a real model ran rather than a stub.
    const result = await page.evaluate(async () => {
      const models = await (await fetch('/openai/models', { credentials: 'same-origin' })).json().catch(() => null);
      const list: any[] = models?.data ?? [];
      if (!list.length) return { status: 0, text: '', model: '' };
      const model = list[0].id;
      const r = await fetch('/openai/chat/completions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          model,
          messages: [{ role: 'user', content: 'What is 2 + 2? Answer with just the number.' }],
          max_tokens: 24,
        }),
      });
      const body = await r.json().catch(() => null);
      return {
        model,
        status: r.status,
        text: body?.choices?.[0]?.message?.content ?? '',
        tokens: body?.usage?.total_tokens ?? 0,
      };
    });

    expect(result.model, 'a model was offered').not.toBe('');
    expect(result.status, `completion -> ${result.status}`).toBe(200);
    expect(result.text.length, 'the answer has content').toBeGreaterThan(0);
    expect(result.tokens, 'the backend reported usage').toBeGreaterThan(0);
    await context.close();
  });
});
