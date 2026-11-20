import { test, expect } from '@playwright/test';
import { installed, hostOf, appAdmin, appToken, anon } from '../lib/session';

// Vikunja client journey: INFORMATIONAL (excluded from the gate; see
// docs/apps/testing.md). The gate itself covers the app through auth.spec.ts
// (OIDC round trip, the API token check) and journeys/tenant.spec.ts (an invited
// tenant's first use).
//
// This journey does what a person does with the app: make a list, put something
// on it, tick it off, and give it a due date - then confirm it is still there.

const admin = appAdmin('vikunja');

test.describe('vikunja', () => {
  test.skip(!installed('vikunja'), 'vikunja is not installed');
  test.skip(!admin.user || !admin.pass, 'VIKUNJA admin credentials not set');

  test('create a project, add and complete a task, then find it again', async () => {
    test.setTimeout(120_000);
    const base = `https://${hostOf('vikunja')}`;
    const token = await appToken('vikunja', admin);
    const ctx = await anon();
    const auth = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
    const title = `groceries-${Date.now()}`;
    let projectId = 0;

    try {
      const project = await ctx.put(`${base}/api/v1/projects`, { headers: auth, data: { title } });
      expect(project.ok(), `create project -> ${project.status()}`).toBeTruthy();
      projectId = (await project.json()).id;

      // A task with a due date, which is the thing a to-do app is for.
      const task = await ctx.put(`${base}/api/v1/projects/${projectId}/tasks`, {
        headers: auth,
        data: { title: 'milk', due_date: new Date(Date.now() + 86_400_000).toISOString() },
      });
      expect(task.ok(), `create task -> ${task.status()}`).toBeTruthy();
      const taskId = (await task.json()).id;

      // Mark it done: a state change, not just a write.
      const done = await ctx.post(`${base}/api/v1/tasks/${taskId}`, {
        headers: auth,
        data: { done: true },
      });
      expect(done.ok(), `complete task -> ${done.status()}`).toBeTruthy();

      const read = await ctx.get(`${base}/api/v1/tasks/${taskId}`, { headers: auth });
      expect(read.ok()).toBeTruthy();
      const stored = await read.json();
      expect(stored.title).toBe('milk');
      expect(stored.done, 'the task stayed completed').toBe(true);
      expect(stored.due_date, 'the due date survived the round trip').toBeTruthy();

      // The project list shows it to the user who owns it.
      const list = await ctx.get(`${base}/api/v1/projects`, { headers: auth });
      expect(list.ok()).toBeTruthy();
      expect((await list.json()).map((p: any) => p.title)).toContain(title);
    } finally {
      if (projectId) {
        await ctx.delete(`${base}/api/v1/projects/${projectId}`, { headers: auth }).catch(() => undefined);
      }
      await ctx.dispose();
    }
  });
});
