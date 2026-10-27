# servarr-bootstrap (vendored)

SPDX-License-Identifier: Apache-2.0

The source is vendored from the author's `servarr-helm` project
(`bootstrap/`, at the commit recorded in `VERSION`; the chart may be newer). It
is part of this repository so the console image carries a **fixed, reviewable** build context:
the console never accepts a Dockerfile or a context from a caller, and nothing
is published to a registry.

The only change from upstream is the digest pin on the base image in the
`Dockerfile`. `VERSION` matches the chart version; the console also hashes the
context, so a source change always produces a new tag and a rebuild.

## License

This directory is part of umkim-thin and is licensed under the **Apache License,
Version 2.0**; see [`../../LICENSE`](../../LICENSE). The upstream `servarr-helm`
source is the same author's work and is published here under the same license.
`package.json` records the license for tooling.

The build-time base image is Microsoft's Playwright image
(`mcr.microsoft.com/playwright`, Apache-2.0), pinned by digest; its contents
remain under their own licenses.
