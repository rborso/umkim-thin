import { test, expect, request } from '@playwright/test';
import { installed, hostOf, portalSession, basic, appAdmin } from '../lib/session';

// Client simulation for Gitea: the git client and the web UI.
//
//   * a directory user signs in through Gitea (LDAP) - the gating check lives
//     in auth.spec.ts; this file covers the client workflow;
//   * a repository goes through the shape a real workflow does: create, branch,
//     commit on the branch, open a pull request, merge it, download the archive
//     and delete the repository;
//   * a release with an asset, which is how artifacts are published.
//
// The API sits behind the forward-auth, so the calls carry the Authelia session
// (like the browser) and authenticate with the Gitea account itself.

const { user: ADMIN_USER, pass: ADMIN_PASS } = appAdmin('gitea');

test.describe('gitea client', () => {
  test.skip(!installed('gitea'), 'gitea is not installed');
  test.skip(!ADMIN_PASS, 'UMKIM_THIN_GITEA_ADMIN_PASSWORD not set');

  const host = hostOf('gitea');
  const base = () => `https://${host}`;
  const auth = () => ({ Authorization: basic(ADMIN_USER, ADMIN_PASS) });
  const jsonHeaders = () => ({ ...auth(), 'Content-Type': 'application/json' });
  const api = (p: string) => `${base()}/api/v1${p}`;
  const repoOf = (name: string) => `${ADMIN_USER}/${name}`;

  async function createRepo(ctx: any, name: string) {
    const created = await ctx.post(api('/user/repos'), {
      headers: jsonHeaders(),
      data: { name, auto_init: true, private: true, default_branch: 'main' },
    });
    expect([200, 201], `create repo -> ${created.status()}`).toContain(created.status());
  }

  async function deleteRepo(ctx: any, name: string) {
    const del = await ctx.delete(api(`/repos/${repoOf(name)}`), { headers: auth() });
    expect([200, 204], `delete repo -> ${del.status()}`).toContain(del.status());
  }

  test('a client creates a repo, commits on a branch, merges a PR and archives it', async () => {
    const ctx = await portalSession();
    const repo = `ci-flow-${Date.now()}`;
    const branch = 'ci-branch';
    await createRepo(ctx, repo);

    // branch off main
    const br = await ctx.post(api(`/repos/${repoOf(repo)}/branches`), {
      headers: jsonHeaders(),
      data: { new_branch_name: branch, old_branch_name: 'main' },
    });
    expect([200, 201], `create branch -> ${br.status()}`).toContain(br.status());

    // commit a file onto it
    const content = 'hello from the ci client\n';
    const commit = await ctx.post(api(`/repos/${repoOf(repo)}/contents/branch.txt`), {
      headers: jsonHeaders(),
      data: { content: Buffer.from(content).toString('base64'), message: 'ci: add on branch', branch },
    });
    expect([200, 201], `commit on branch -> ${commit.status()}`).toContain(commit.status());

    // open a pull request and merge it
    const pr = await ctx.post(api(`/repos/${repoOf(repo)}/pulls`), {
      headers: jsonHeaders(),
      data: { title: 'CI: merge the branch', head: branch, base: 'main' },
    });
    expect([200, 201], `open PR -> ${pr.status()}`).toContain(pr.status());
    const index = (await pr.json()).number;
    expect(index, 'pull request number').toBeGreaterThan(0);

    // Gitea computes PR mergeability asynchronously after the PR is opened and
    // returns 405 until that check finishes. Poll the PR until it is mergeable,
    // then merge; keep a short retry for the same race on the merge call.
    let merged = await ctx.post(api(`/repos/${repoOf(repo)}/pulls/${index}/merge`), {
      headers: jsonHeaders(),
      data: { Do: 'merge', MergeMessageField: 'CI merge' },
    });
    for (let attempt = 0; attempt < 15 && !merged.ok(); attempt++) {
      if (merged.status() !== 405 && merged.status() !== 409) break;
      await new Promise((r) => setTimeout(r, 1000));
      const state = await ctx.get(api(`/repos/${repoOf(repo)}/pulls/${index}`), { headers: auth() });
      if (state.ok() && (await state.json()).merged) {
        merged = state;
        break;
      }
      merged = await ctx.post(api(`/repos/${repoOf(repo)}/pulls/${index}/merge`), {
        headers: jsonHeaders(),
        data: { Do: 'merge', MergeMessageField: 'CI merge' },
      });
    }
    expect([200, 201], `merge PR -> ${merged.status()}`).toContain(merged.status());

    // the file is now on the default branch, and the archive contains it
    const onMain = await ctx.get(api(`/repos/${repoOf(repo)}/contents/branch.txt?ref=main`), { headers: auth() });
    expect(onMain.status(), 'the merged file is on main').toBe(200);
    const archive = await ctx.get(`${base()}/api/v1/repos/${repoOf(repo)}/archive/main.tar.gz`, { headers: auth() });
    expect(archive.status(), 'download the archive').toBe(200);
    expect((await archive.body()).length, 'the archive has content').toBeGreaterThan(0);

    await deleteRepo(ctx, repo);
    await ctx.dispose();
  });

  test('a client publishes a release with an asset', async () => {
    const ctx = await portalSession();
    const repo = `ci-release-${Date.now()}`;
    await createRepo(ctx, repo);

    const tag = 'v1.0.0';
    const release = await ctx.post(api(`/repos/${repoOf(repo)}/releases`), {
      headers: jsonHeaders(),
      data: { tag_name: tag, name: 'CI release', draft: false, prerelease: false, target_commitish: 'main' },
    });
    expect([200, 201], `create release -> ${release.status()}`).toContain(release.status());
    const releaseId = (await release.json()).id;

    // upload an asset through the multipart endpoint the UI uses
    const asset = await ctx.post(
      `${base()}/api/v1/repos/${repoOf(repo)}/releases/${releaseId}/assets?name=ci.txt`,
      {
        headers: auth(),
        multipart: { attachment: { name: 'ci.txt', mimeType: 'text/plain', buffer: Buffer.from('release asset\n') } },
      },
    );
    expect([200, 201], `upload asset -> ${asset.status()}`).toContain(asset.status());

    const downloads = await ctx.get(
      `${base()}/api/v1/repos/${repoOf(repo)}/releases/${releaseId}`,
      { headers: auth() },
    );
    expect([200, 201]).toContain(downloads.status());
    const assets = (await downloads.json()).assets ?? [];
    expect(assets.map((a: any) => a.name), 'the asset is attached').toContain('ci.txt');

    await deleteRepo(ctx, repo);
    await ctx.dispose();
  });
});
