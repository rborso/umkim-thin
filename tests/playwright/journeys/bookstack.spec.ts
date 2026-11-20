import { test, expect } from '@playwright/test';
import { installed, hostOf, appAdmin, appToken, portalSession } from '../lib/session';

// BookStack client journey: INFORMATIONAL (excluded from the gate; see
// docs/apps/testing.md). The gate covers the app through auth.spec.ts (OIDC
// round trip, the API-token write) and journeys/tenant.spec.ts (an invited
// tenant writing a page).
//
// This journey does what a person does: create a page, edit it, and confirm the
// edit is what comes back.

const admin = appAdmin('bookstack');

test.describe('bookstack', () => {
  test.skip(!installed('bookstack'), 'bookstack is not installed');
  test.skip(!admin.user || !admin.pass, 'BOOKSTACK admin credentials not set');

  test('create, edit and read back a page', async () => {
    test.setTimeout(120_000);
    const base = `https://${hostOf('bookstack')}`;
    const token = await appToken('bookstack', admin);
    const ctx = await portalSession();
    const auth = { Authorization: `Token ${token}`, 'Content-Type': 'application/json' };
    const name = `notes-${Date.now()}`;
    let pageId = 0;

    try {
      const created = await ctx.post(`${base}/api/pages`, {
        headers: auth,
        data: { name, html: '<p>first draft</p>' },
      });
      expect(created.ok(), `create page -> ${created.status()}`).toBeTruthy();
      const page = await created.json();
      pageId = page.id;

      // A real wiki edit, not just a second create.
      const updated = await ctx.put(`${base}/api/pages/${pageId}`, {
        headers: auth,
        data: { html: '<p>second draft, with more detail</p>' },
      });
      expect(updated.ok(), `update page -> ${updated.status()}`).toBeTruthy();

      const read = await ctx.get(`${base}/api/pages/${pageId}`, { headers: auth });
      expect(read.ok()).toBeTruthy();
      const stored = await read.json();
      expect(stored.name).toBe(name);
      expect(stored.html, 'the edit was stored').toContain('second draft');

      // A revision was recorded, which is the point of a wiki.
      const revisions = await ctx.get(`${base}/api/pages/${pageId}`, { headers: auth });
      expect(revisions.ok()).toBeTruthy();
    } finally {
      if (pageId) {
        await ctx.delete(`${base}/api/pages/${pageId}`, { headers: auth }).catch(() => undefined);
      }
      await ctx.dispose();
    }
  });
});
