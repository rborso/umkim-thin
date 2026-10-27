## Summary

<!-- What changed, and why? Link the issue if there is one. -->

## Test plan

<!-- Tick what you ran; say if a check is not applicable. -->

- [ ] `./ci/static.sh`
- [ ] single e2e (`./ci/e2e.sh single`)
- [ ] HA e2e (`./ci/e2e.sh ha`)
- [ ] new/updated Playwright acceptance or journey spec

## Scope and compatibility

- [ ] No secrets, real hostnames, lab addresses, or `local/` files are included.
- [ ] Chart versions, the catalog entry, and the packaged chart are in sync.
- [ ] Existing catalog entries still load.
- [ ] The change has an upgrade or rollback path (or is documentation-only).

## License and provenance

- [ ] This contribution is made under the repository's Apache-2.0 license.
- [ ] New third-party code, charts, or images are listed in
      [`docs/licenses.md`](../docs/licenses.md) and [`NOTICE`](../NOTICE).
- [ ] Any new catalog entry names the upstream project and its license.
- [ ] I have the right to contribute the code and did not copy incompatible
      source into the repository.

## Notes for reviewers

<!-- Anything risky, surprising, or intentionally deferred? -->
