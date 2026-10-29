import path from 'node:path';
import { defineConfig } from '@playwright/test';

// CI runs against a real deployment whose certificates are Let's Encrypt
// *staging* (untrusted), so HTTPS errors are ignored. The app list comes from
// UMKIM_THIN_APPS (written by ci/e2e.sh); the Authelia credentials from the
// environment.
//
// UMKIM_THIN_REPORT_DIR is set by ci/test-existing.sh. In that mode the gate
// writes machine-readable JSON/JUnit plus a browsable HTML report; the normal
// e2e path keeps the compact list reporter.
const reportDir = process.env.UMKIM_THIN_REPORT_DIR;
const reporters: Parameters<typeof defineConfig>[0]['reporter'] = reportDir
  ? [
      ['list'],
      ['json', { outputFile: path.join(reportDir, 'gate.json') }],
      ['junit', { outputFile: path.join(reportDir, 'gate.xml') }],
      ['html', { outputFolder: path.join(reportDir, 'html'), open: 'never' }],
    ]
  : [['list']];

export default defineConfig({
  testDir: '.',
  testMatch: ['*.spec.ts'],
  // Journeys drive real apps: OCR consumers, Longhorn-backed writes and image
  // thumbnails can take longer than a UI test would. The journeys are
  // informational, so a generous cap is better than a false failure.
  timeout: 180_000,
  expect: { timeout: 15_000 },
  retries: 1,
  workers: 2,
  reporter: reporters,
  use: {
    // The default profile tolerates the lab/staging certs (the deterministic
    // check lives in tls.spec.ts). A full run verifies TLS for real: the staging
    // roots are trusted through NODE_EXTRA_CA_CERTS and Chromium's NSS store.
    ignoreHTTPSErrors: process.env.UMKIM_THIN_STRICT_TLS !== '1',
    headless: true,
    extraHTTPHeaders: { 'User-Agent': 'umkim-thin-ci' },
  },
});
