# umkim-thin

The console/API chart: one Deployment for the `umkim-thin` binary, its
ServiceAccount, ConfigMap, Secret seed and a PersistentVolumeClaim for state.
The binary drives the catalog, directory, app routes, storage and the optional
WireGuard gateway.

## License

The chart and the console binary are Apache-2.0, part of umkim-thin. See
[`../../LICENSE`](../../LICENSE) and [`../../NOTICE`](../../NOTICE).

The image also contains Helm and kubectl (Apache-2.0) and Alpine base packages
under their own licenses. Locally built images keep the project license and
notice under `/usr/share/licenses/umkim-thin/`.
