#!/usr/bin/env python3
"""Check the catalog against a packaged chart mirror.

Companion to ci/static.sh. docs/versioning-plan.md promises that the static check
enforces "the chart tarball for each advertised version exists in the local
mirror" and that the previous stable is kept for offline rollback.

Usage: check-mirror-versions.py <mirror-dir>

Prints one problem per line and exits 1 when there are any, so the caller can
show them all at once.

Problems reported:
  - a catalogued version (chart.version or chart.versions[].version) with no
    tarball in the mirror: the console would offer an install that fails;
  - an app chart with no packaged version other than the current one: an offline
    rollback to the previous release is not possible.
"""
import pathlib
import sys

try:
    import yaml
except Exception as exc:  # pragma: no cover - the CI image has PyYAML
    print(f"cannot import yaml ({exc}); cannot verify the mirror")
    sys.exit(1)


def catalog_entries():
    for path in sorted(pathlib.Path("catalog").glob("*.yaml")):
        d = yaml.safe_load(path.read_text()) or {}
        c = d.get("chart") or {}
        ref = str(c.get("ref") or "").rstrip("/")
        chart = ref.split("/")[-1] if ref else (d.get("name") or path.stem)
        versions = [v.get("version") for v in (c.get("versions") or []) if v.get("version")]
        if c.get("version"):
            versions.append(str(c["version"]))
        yield path.name, chart, versions


def current_version(chart: str) -> str:
    for base in ("charts/apps", "charts"):
        p = pathlib.Path(base, chart, "Chart.yaml")
        if p.exists():
            for line in p.read_text().splitlines():
                if line.startswith("version:"):
                    return line.split()[1]
    return ""


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: check-mirror-versions.py <mirror-dir>", file=sys.stderr)
        return 2
    mirror = pathlib.Path(sys.argv[1])
    if not mirror.is_dir():
        print(f"no mirror directory at {mirror}", file=sys.stderr)
        return 2
    packaged = {p.name for p in mirror.glob("*.tgz")}

    problems = []
    for name, chart, versions in catalog_entries():
        # Only check charts vendored in this tree; chart_repos charts are external.
        if not pathlib.Path("charts/apps", chart).is_dir() and not pathlib.Path("charts", chart).is_dir():
            continue
        for v in sorted(set(versions)):
            if f"{chart}-{v}.tgz" not in packaged:
                problems.append(f"{name}: {chart} {v} is catalogued but not packaged")

    # Offline rollback needs a second packaged version for every app chart.
    apps = pathlib.Path("charts/apps")
    if apps.is_dir():
        for d in sorted(apps.iterdir()):
            if not (d / "Chart.yaml").exists():
                continue
            cur = current_version(d.name)
            others = [n for n in packaged
                      if n.startswith(f"{d.name}-") and n != f"{d.name}-{cur}.tgz"]
            if not others:
                problems.append(
                    f"{d.name}: no previous version packaged; an offline rollback is unavailable")

    if problems:
        print("\n".join(problems))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
