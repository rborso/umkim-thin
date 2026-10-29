# wireguard-gateway

An optional host-networked WireGuard gateway. It runs one pod per node so the
node that owns the kube-vip VIP already has a local UDP 51820 listener. Peers
come from a Kubernetes Secret; app routes can be published with
`exposure: vpn`.

The chart also runs a dedicated cluster-network `vpn-dns` deployment and
service (`10.43.200.53` by default). The installer writes the
`wireguard-vpn-dns` ConfigMap with the public hostnames of routes that opted
into `trusted.vpn`; the DNS pod answers those names with the gateway address
and forwards every other name to the node resolver. Clients receive this
service address in their WireGuard `DNS` line. The gateway's `:443` DNAT is
scoped to the gateway address and targets the Traefik `vpn` entrypoint, so
full-tunnel clients can still reach public hostnames normally. The legacy
`<host>.vpn.<zone>` wildcard remains available through `vpnZone`.

## License

The chart source is Apache-2.0, part of umkim-thin. The WireGuard implementation
and tools retain their upstream **GPL-2.0** license. See
[`../../docs/licenses.md`](../../docs/licenses.md).

## Upgrade note

Chart 0.1.2 changes the workload from a Deployment to a DaemonSet. A fresh
install and a single-node install are unaffected. When upgrading an existing
0.1.1 release, delete the old Deployment before running the Helm upgrade so two
host-network listeners do not race for UDP 51820:

```bash
kubectl -n wireguard delete deploy wireguard-gateway --ignore-not-found
```
