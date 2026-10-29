import { test, expect } from '@playwright/test';
import { installed, hostOf, oidcLogin, png, USER, PASS } from '../lib/session';

// HomeBox client journey: create a location, an item in it, a tag, a photo and
// then find the item through search and the CSV export. The browser session is
// established through the real OIDC flow; the API calls use that session.
test.describe('homebox client', () => {
  test.skip(!installed('homebox'), 'homebox is not installed');
  test.skip(!PASS, 'UMKIM_THIN_PASSWORD not set');

  const host = hostOf('homebox');
  const base = `https://${host}`;
  const url = (path: string) => `${base}${path}`;

  test('creates a location and an item, attaches a photo, searches and exports', async ({ browser }) => {
    test.setTimeout(120_000);

    const { context } = await oidcLogin(browser, host, {
      credentials: { username: USER, password: PASS },
      ready: async (page) =>
        page
          .evaluate(async () => {
            const r = await fetch('/api/v1/users/self', { credentials: 'same-origin' });
            return r.ok;
          })
          .catch(() => false),
    });
    const api = context.request;
    const marker = `ci-${Date.now()}`;
    let locationId = '';
    let itemId = '';
    let tagId = '';

    try {
      const types = await api.get(url('/api/v1/entity-types'));
      expect(types.status(), 'entity types').toBe(200);
      const typeList = (await types.json()) as Array<{ id: string; name: string; isLocation: boolean }>;
      const locationType = typeList.find((t) => t.isLocation);
      const itemType = typeList.find((t) => !t.isLocation);
      expect(locationType, 'a location entity type').toBeTruthy();
      expect(itemType, 'an item entity type').toBeTruthy();

      const location = await api.post(url('/api/v1/entities'), {
        data: { name: `${marker} Location`, entityTypeId: locationType!.id },
      });
      expect(location.status(), `create location -> ${location.status()}`).toBe(201);
      locationId = (await location.json()).id;
      expect(locationId, 'location id').toBeTruthy();

      const tag = await api.post(url('/api/v1/tags'), {
        data: { name: marker },
      });
      expect(tag.status(), `create tag -> ${tag.status()}`).toBe(201);
      tagId = (await tag.json()).id;
      expect(tagId, 'tag id').toBeTruthy();

      const item = await api.post(url('/api/v1/entities'), {
        data: {
          name: `${marker} Item`,
          description: 'created by the HomeBox CI journey',
          entityTypeId: itemType!.id,
          parentId: locationId,
          quantity: 1,
          tagIds: [tagId],
        },
      });
      expect(item.status(), `create item -> ${item.status()}`).toBe(201);
      itemId = (await item.json()).id;
      expect(itemId, 'item id').toBeTruthy();

      const attachment = await api.post(url(`/api/v1/entities/${itemId}/attachments`), {
        multipart: {
          file: { name: 'ci.png', mimeType: 'image/png', buffer: png(32, 32) },
          name: 'ci.png',
          primary: 'true',
        },
      });
      expect(attachment.status(), `attach photo -> ${attachment.status()}`).toBe(201);

      const read = await api.get(url(`/api/v1/entities/${itemId}`));
      expect(read.status(), 'read the item back').toBe(200);
      const body = await read.json();
      expect(body.name).toBe(`${marker} Item`);
      expect((body.tags ?? []).map((t: any) => t.id), 'the tag is attached').toContain(tagId);
      expect((body.attachments ?? []).length, 'the photo is attached').toBeGreaterThan(0);

      const search = await api.get(url(`/api/v1/entities?q=${encodeURIComponent(marker)}`));
      expect(search.status(), 'search').toBe(200);
      const found = await search.json();
      expect(
        (found.items ?? []).map((i: any) => i.name),
        'search finds the new item',
      ).toContain(`${marker} Item`);

      const exported = await api.get(url('/api/v1/entities/export'));
      expect(exported.status(), 'CSV export').toBe(200);
      expect(await exported.text(), 'the export contains the new item').toContain(marker);
    } finally {
      if (itemId) await api.delete(url(`/api/v1/entities/${itemId}`)).catch(() => undefined);
      if (locationId) await api.delete(url(`/api/v1/entities/${locationId}`)).catch(() => undefined);
      if (tagId) await api.delete(url(`/api/v1/tags/${tagId}`)).catch(() => undefined);
      await context.close();
    }
  });
});
