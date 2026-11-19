#!/usr/bin/env python3
"""Prune the chart mirror to what the catalog advertises.

The mirror is seeded into a ConfigMap (256 KiB annotation / 1 MiB object cap), so
superseded tarballs cannot accumulate. It must not be wiped either: the catalog's
versions[] is the upgrade/rollback set the console resolves through chartRef, and
a version listed there with no tarball sends Helm a GitHub tree URL instead.

Usage: prune-chart-mirror.py <mirror-dir> <catalog-dir> [staged-list]

Keeps a tarball when <chart>-<version>.tgz is named by a catalog entry's
chart.version or chart.versions[].version, plus every name listed in
<staged-list> - the platform charts (identity, umkim-thin, wireguard-gateway) the
playbook installs, which the catalog does not list. Everything else is removed.

<staged-list> is a file of tarball names, one per line, written by the playbook
from the artifacts directory *on the controller* (playbook_dir does not exist on
the target, so it cannot be passed as a path). Keeping this run's staged set,
rather than "any file whose name starts with a platform chart", is what lets a
bumped platform chart retire its predecessor instead of accumulating one tarball
per release.

If <staged-list> is given but missing or empty, nothing is pruned: an unreadable
keep-set must not be mistaken for "the platform charts are not wanted" - that
dropped identity and umkim-thin from a live mirror once.

Parsing uses PyYAML, which the provisioning image guarantees; a regex fallback is
deliberately not used, because a partial parse would prune the rollback set.
"""
import pathlib
import sys


def load_yaml(path: pathlib.Path):
    import yaml  # imported here so a missing module fails loudly below

    return yaml.safe_load(path.read_text()) or {}


def catalog_refs(catalog_dir: pathlib.Path):
    """Yield (chart_name, version) pairs the catalog advertises."""
    for path in sorted(catalog_dir.glob("*.yaml")):
        d = load_yaml(path)
        c = d.get("chart") or {}
        ref = str(c.get("ref") or "").rstrip("/")
        chart = ref.split("/")[-1] if ref else (d.get("name") or path.stem)
        if c.get("version"):
            yield chart, str(c["version"])
        for v in c.get("versions") or []:
            if isinstance(v, dict) and v.get("version"):
                yield chart, str(v["version"])




def main() -> int:
    if len(sys.argv) not in (3, 4):
        print("usage: prune-chart-mirror.py <mirror-dir> <catalog-dir> [staged-dir]",
              file=sys.stderr)
        return 2
    mirror, catalog = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    staged = pathlib.Path(sys.argv[3]) if len(sys.argv) == 4 else None
    if not mirror.is_dir():
        print(f"prune-chart-mirror: no mirror directory at {mirror}", file=sys.stderr)
        return 2
    if not catalog.is_dir():
        print(f"prune-chart-mirror: no catalog directory at {catalog}", file=sys.stderr)
        return 2

    try:
        keep = {f"{chart}-{ver}.tgz" for chart, ver in catalog_refs(catalog)}
    except Exception as exc:  # PyYAML missing or unreadable: never prune blind
        print(f"prune-chart-mirror: cannot read the catalog ({exc}); leaving the mirror alone",
              file=sys.stderr)
        return 1

    if not keep:
        print("prune-chart-mirror: catalog advertised no versions; leaving the mirror alone",
              file=sys.stderr)
        return 1

    if staged is not None:
        try:
            names = [ln.strip() for ln in staged.read_text().splitlines() if ln.strip()]
        except Exception as exc:
            print(f"prune-chart-mirror: cannot read the staged list ({exc}); "
                  "leaving the mirror alone", file=sys.stderr)
            return 1
        if not names:
            print("prune-chart-mirror: the staged list is empty; leaving the mirror alone",
                  file=sys.stderr)
            return 1
        for name in names:
            keep.add(pathlib.PurePosixPath(name).name)


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
