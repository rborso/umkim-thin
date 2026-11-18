#!/usr/bin/env python3
"""Prune the chart mirror to what the catalog advertises.

The mirror is seeded into a ConfigMap (256 KiB annotation / 1 MiB object cap), so
superseded tarballs cannot accumulate. It must not be wiped either: the catalog's
versions[] is the upgrade/rollback set the console resolves through chartRef, and
a version listed there with no tarball sends Helm a GitHub tree URL instead.

Usage: prune-chart-mirror.py <mirror-dir> <catalog-dir>

Keeps a tarball when its <chart>-<version>.tgz is named by any catalog entry's
chart.version or chart.versions[].version. Chart identity comes from the ref's
last path segment, matching the console's chartRef. Anything else is removed.
"""
import pathlib
import re
import sys


def catalog_refs(catalog_dir: pathlib.Path):
    """Yield (chart_name, version) pairs the catalog advertises."""
    for path in sorted(catalog_dir.glob("*.yaml")):
        text = path.read_text(errors="replace")
        name_m = re.search(r"^name:\s*(\S+)", text, re.M)
        ref_m = re.search(r"^chart:\s*\n(?:.*\n)*?\s+ref:\s*(\S+)", text, re.M)
        name = name_m.group(1) if name_m else path.stem
        if ref_m:
            chart = ref_m.group(1).rstrip("/").split("/")[-1]
        else:
            chart = name
        block_m = re.search(r"^chart:\s*\n((?:.*\n)*?)(?=^\S)", text, re.M)
        block = block_m.group(1) if block_m else text
        cur = re.search(r"^  version:\s*(\S+)", block, re.M)
        if cur:
            yield chart, cur.group(1)
        for v in re.findall(r"^\s+- version:\s*(\S+)", block, re.M):
            yield chart, v


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: prune-chart-mirror.py <mirror-dir> <catalog-dir>", file=sys.stderr)
        return 2
    mirror, catalog = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    if not mirror.is_dir() or not catalog.is_dir():
        print("prune-chart-mirror: missing mirror or catalog directory", file=sys.stderr)
        return 2

    keep = {f"{chart}-{ver}.tgz" for chart, ver in catalog_refs(catalog)}
    if not keep:
        # Never prune against an unreadable/empty catalog: that would delete the
        # rollback set the console needs.
        print("prune-chart-mirror: catalog advertised no versions; leaving the mirror alone",
              file=sys.stderr)
        return 1

    # The platform charts are installed by the playbook, not the catalog, yet the
    # console resolves them from this mirror (UMKIM_THIN_IDENTITY_CHART etc.). Keep
    # whatever is packaged now regardless of the catalog.
    for tgz in mirror.glob("*.tgz"):
        if re.match(r"^(identity|umkim-thin|wireguard-gateway)-", tgz.name):
            keep.add(tgz.name)

    removed = 0
    for tgz in sorted(mirror.glob("*.tgz")):
        if tgz.name not in keep:
            tgz.unlink()
            removed += 1
            print(f"  pruning {tgz.name} (not catalogued)")
    print(f"prune-chart-mirror: kept {len(list(mirror.glob('*.tgz')))}, removed {removed}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
