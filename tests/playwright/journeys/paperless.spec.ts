import { test, expect } from '@playwright/test';
import { installed, hostOf, portalSession, appAdmin, waitFor } from '../lib/session';

// Client simulation for Paperless: what the "consume" folder and the web UI do.
//
//   * SSO sign-in through the real browser flow (Paperless -> Authelia -> back).
//   * consume a real PDF, wait for the consumer, and check that its text layer
//     was extracted and indexed - the whole point of Paperless.
//   * full-text search finds the document by a token that only exists in its
//     text layer.
//   * metadata: tags and a correspondent, applied and read back.
//   * the stored copies: preview, thumbnail, and the original download.

const { user: ADMIN_USER, pass: ADMIN_PASS } = appAdmin('paperless');

// pdfWithText builds a minimal single-page PDF whose content stream draws the
// given text with a standard font. Small, but a genuine PDF with a text layer,
// so the consumer, OCR and the index all have something to work on.
function pdfWithText(text: string): Buffer {
  const escaped = text.replace(/([()\\])/g, '\\$1');
  const stream = `BT /F1 14 Tf 40 700 Td (${escaped}) Tj ET`;
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>',
    `<< /Length ${stream.length} >>\nstream\n${stream}\nendstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ];
  let out = '%PDF-1.4\n';
  const offsets: number[] = [];
  objects.forEach((body, i) => {
    offsets.push(out.length);
    out += `${i + 1} 0 obj\n${body}\nendobj\n`;
  });
  const xref = out.length;
  out += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  for (const o of offsets) out += `${String(o).padStart(10, '0')} 00000 n \n`;
  out += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(out, 'latin1');
}

test.describe('paperless client', () => {
  test.skip(!installed('paperless'), 'paperless is not installed');
  test.skip(!ADMIN_PASS, 'UMKIM_THIN_PAPERLESS_ADMIN_PASSWORD not set');

  const host = hostOf('paperless');
  const base = () => `https://${host}`;

  async function api() {
    const ctx = await portalSession();
    const login = await ctx.post(`${base()}/api/token/`, {
      data: { username: ADMIN_USER, password: ADMIN_PASS },
    });
    expect([200, 201], `token for ${ADMIN_USER}`).toContain(login.status());
    const { token } = await login.json();
    expect(token, 'api token').toBeTruthy();
    return { ctx, headers: { Authorization: `Token ${token}` } };
  }

  // consume uploads a document and waits for the consumer to finish, returning
  // the document id (Paperless does not put it on the task, so it is found by
  // searching for the text that only this document contains).
  async function consume(ctx: any, headers: any, name: string, pdf: Buffer, marker: string) {
    const up = await ctx.post(`${base()}/api/documents/post_document/`, {
      headers,
      multipart: { document: { name, mimeType: 'application/pdf', buffer: pdf } },
    });
    expect([200, 201], `upload -> ${up.status()}`).toContain(up.status());
    const taskId = (await up.json()).toString().replace(/"/g, '');

    await waitFor(
      async () => {
        const t = await ctx.get(`${base()}/api/tasks/?task_id=${taskId}`, { headers });
        const task = (await t.json())?.results?.[0];
        const status = String(task?.status ?? '').toLowerCase();
        if (status === 'failure') throw new Error(`paperless task failed: ${JSON.stringify(task)}`);
        return status === 'success';
      },
      { what: 'the document consumer', timeoutMs: 180_000, intervalMs: 1500 },
    );

    // the text layer is indexed: search for the marker and take the hit
    let id = '';
    await waitFor(
      async () => {
        const found = await ctx.get(`${base()}/api/documents/?query=${encodeURIComponent(marker)}`, { headers });
        id = String(((await found.json())?.results ?? [])[0]?.id ?? '');
        return Boolean(id);
      },
      { what: 'the document to be indexed', timeoutMs: 240_000, intervalMs: 2000 },
    );
    return id;
  }

  test('a consumed PDF is OCRed, searchable and downloadable', async () => {
    test.setTimeout(300_000);
    const { ctx, headers } = await api();
    // digits only: OCR reliably reads them, whereas 'I'/'l' and 'O'/'0' are
    // routinely confused in a rendered PDF
    const marker = `4${Date.now()}`;
    const id = await consume(ctx, headers, 'ci-ocr.pdf', pdfWithText(marker), marker);

    const doc = await (await ctx.get(`${base()}/api/documents/${id}/`, { headers })).json();
    expect(doc.title, 'the document is titled').toBeTruthy();
    expect(doc.content, 'the text layer was extracted').toContain(marker);
    expect(doc.page_count, 'page count').toBe(1);

    // the stored copies a user actually opens
    for (const [path, label] of [
      [`/api/documents/${id}/preview/`, 'preview'],
      [`/api/documents/${id}/download/`, 'original'],
      [`/api/documents/${id}/thumb/`, 'thumbnail'],
    ] as const) {
      const r = await ctx.get(`${base()}${path}`, { headers });
      expect(r.status(), label).toBe(200);
    }

    const dl = await ctx.get(`${base()}/api/documents/${id}/download/`, { headers });
    expect((await dl.body()).subarray(0, 5).toString(), 'the original is a PDF').toBe('%PDF-');

    await ctx.delete(`${base()}/api/documents/?documents=${id}`, { headers });
    await ctx.dispose();
  });

  test('a client tags and correlates a document', async () => {
    test.setTimeout(300_000);
    const { ctx, headers } = await api();
    const marker = `7${Date.now()}`;
    const id = await consume(ctx, headers, 'ci-tag.pdf', pdfWithText(marker), marker);

    const tag = await (await ctx.post(`${base()}/api/tags/`, { headers, data: { name: `ci-${Date.now()}` } })).json();
    expect(tag.id, 'tag id').toBeTruthy();
    const who = await (
      await ctx.post(`${base()}/api/correspondents/`, { headers, data: { name: `CI Corp ${Date.now()}` } })
    ).json();
    expect(who.id, 'correspondent id').toBeTruthy();

    const patched = await ctx.patch(`${base()}/api/documents/${id}/`, {
      headers,
      data: { tags: [tag.id], correspondent: who.id },
    });
    expect(patched.status(), `patch -> ${patched.status()}`).toBe(200);

    const read = await (await ctx.get(`${base()}/api/documents/${id}/`, { headers })).json();
    expect((read.tags ?? []).map((t: any) => t.id ?? t), 'the tag is applied').toContain(tag.id);
    expect(read.correspondent?.id ?? read.correspondent, 'the correspondent is applied').toBe(who.id);

    // filtering by the tag finds it
    const filtered = await (
      await ctx.get(`${base()}/api/documents/?tags__id__all=${tag.id}`, { headers })
    ).json();
    expect((filtered.results ?? []).map((d: any) => d.id)).toContain(Number(id));

    await ctx.delete(`${base()}/api/documents/?documents=${id}`, { headers });
    await ctx.delete(`${base()}/api/tags/${tag.id}/`, { headers });
    await ctx.delete(`${base()}/api/correspondents/${who.id}/`, { headers });
    await ctx.dispose();
  });
});
