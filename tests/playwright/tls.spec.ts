import { test, expect } from '@playwright/test';
import { connect } from 'node:tls';

// Deterministic certificate check: every host the platform publishes must serve
// a real certificate, not Traefik's self-signed default. This catches a whole
// class of issues (e.g. an ACME-less zone) that the browser tests would
// otherwise paper over with ignoreHTTPSErrors.
//
// UMKIM_THIN_APPS   JSON array of {host, ...} (as written by ci/e2e.sh)
// UMKIM_THIN_AUTH_HOST   the portal host

type App = { name: string; host: string };

const apps: App[] = JSON.parse(process.env.UMKIM_THIN_APPS || '[]');
const authHost = process.env.UMKIM_THIN_AUTH_HOST || '';
const hosts = [...new Set([authHost, ...apps.map((a) => a.host)].filter(Boolean))];

// In a full run the staging roots are trusted (NODE_EXTRA_CA_CERTS), so the
// chain is verified for real instead of merely inspected.
const strict = process.env.UMKIM_THIN_STRICT_TLS === '1';

function peerCertificate(host: string): Promise<any> {
  return new Promise((resolve, reject) => {
    const socket = connect(
      { host, port: 443, servername: host, rejectUnauthorized: strict },
      () => {
        const cert = socket.getPeerCertificate(true);
        socket.end();
        resolve(cert);
      },
    );
    socket.on('error', reject);
    socket.setTimeout(15_000, () => socket.destroy(new Error('TLS connect timed out')));
  });
}

test.describe('tls', () => {
  for (const host of hosts) {
    test(`${host} serves a non-default certificate`, async () => {
      const cert = await peerCertificate(host);
      const issuer = [cert.issuer?.CN, cert.issuer?.O].filter(Boolean).join(' ');
      const subject = cert.subject?.CN ?? '';
      expect(issuer, `issuer for ${host}`).not.toContain('TRAEFIK DEFAULT CERT');
      expect(issuer.length, `issuer present for ${host}`).toBeGreaterThan(0);
      if (strict) {
        // A full run has no lab-CA fallback: the certificate must come from the
        // ACME staging environment, which is what proves issuance works.
        expect(issuer, `issuer for ${host} in full profile`).toMatch(/STAGING/i);
        expect(issuer, `issuer for ${host} must not be the lab CA`).not.toContain('umkim-thin lab CA');
      }
      // The leaf must actually cover the hostname it is served for.
      const names = (cert.subjectaltname ?? '')
        .split(',')
        .map((s) => s.trim().replace(/^DNS:/, ''));
      const covered = names.some((n) => n === host || n.replace(/^\*/, '') === host.replace(/^[^.]*/, ''));
      expect(covered || subject === host, `SAN for ${host}: ${cert.subjectaltname}`).toBeTruthy();
    });
  }
});
