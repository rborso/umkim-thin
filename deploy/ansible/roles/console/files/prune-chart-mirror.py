#!/usr/bin/env python3
"""Prune the chart mirror to what the catalog advertises.

The mirror is seeded into a ConfigMap (256 KiB annotation / 1 MiB object cap), so
superseded tarballs cannot accumulate. It must not be wiped either: the catalog's
versions[] is the upgrade/rollback set the console resolves through chartRef, and
a version listed there with no tarball sends Helm a GitHub tree URL instead.

Usage: prune-chart-mirror.py <mirror-dir> <catalog-dir>

Keeps a tarball when <chart>-<version>.tgz is named by a catalog entry's
chart.version or chart.versions[].version, plus the platform charts the playbook
installs (identity, umkim-thin, wireguard-gateway), which the catalog does not
list. Everything else is removed.

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


PLATFORM = ("identity", "umkim-thin", "wireguard-gateway")


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: prune-chart-mirror.py <mirror-dir> <catalog-dir>", file=sys.stderr)
        return 2
    mirror, catalog = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
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

    # The platform charts are installed by the playbook, not the catalog, yet the
    # console resolves them from this mirror (UMKIM_THIN_IDENTITY_CHART etc.).
    for tgz in mirror.glob("*.tgz"):
        if tgz.name.startswith(PLATFORM):
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
