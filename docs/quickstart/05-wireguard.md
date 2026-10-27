# 5. Optional: WireGuard

**Goal:** create a private tunnel to the cluster and a client config for a
laptop or phone.

WireGuard is optional. The single-node installer opens `51820/udp` when
`firewalld` is active; on a cloud host also allow UDP 51820 in the provider's
firewall, and forward it to the server if it is behind NAT.

## Install the gateway

1. Console → **Catalog** → **WireGuard gateway** → **Install**.
2. The gateway has no HTTP route. It runs as a workload with `hostNetwork` and
   binds UDP 51820.
3. **Applications** should show it as ready.

The gateway runs as a DaemonSet: one host-networked pod per node. In HA, the
node that owns the kube-vip VIP therefore already has a local UDP 51820
listener. Forward UDP 51820 to the VIP (or to a stable node address if you
prefer the node-pinned model). See the [HA quickstart](11-ha-install.md#6-wireguard-on-ha).

## Create a client

1. Console → **WireGuard clients**.
2. Enter a name (for example `laptop`) and click **Create client**.
3. Scan the QR code with the WireGuard app, or download the `.conf` file.
4. Activate the tunnel.

The client receives an address in `192.168.168.0/24` (the gateway is `.1`). Its
allowed IPs are the cluster pod and service networks, and its DNS is the cluster
DNS. That lets the client reach cluster services by their in-cluster names.

## What this gives you today

- A private path to cluster services and the DNS.
- A way to reach the console or an app service without exposing a new public
  port.

It does **not** automatically make a catalog app private. The catalog's app
routes are published as public hostnames (behind Authelia) on this path. Making
an individual app private is an explicit catalog choice: set its route
`exposure` to `vpn` (or install it with `--exposure vpn`), which publishes it
only on the internal VPN entrypoint. Use the VPN hostname and split DNS; the
public router does not publish it.

If that is your goal, read [`../design.md`](../design.md#wireguard-as-an-in-cluster-workload)
and the catalog entry's `route.mode` before changing it. A private route without
split DNS is a common way to lock yourself out.

## Revoke a client

Delete it in the console. The gateway reloads the peer list; the old key stops
working. Rotate by creating a new client and deleting the old one.

## Troubleshooting

| Symptom | Check |
|---|---|
| The client connects but nothing answers | The allowed IPs are the pod/service CIDRs; the target must be a cluster IP or a name that resolves to one |
| The handshake never completes | UDP 51820 is blocked by the cloud firewall, or the endpoint is wrong (set `WG_ENDPOINT` when installing) |
| It worked, then stopped | Check the node that owns the VIP and the gateway pod on that node; the DaemonSet should cover every node |

## Next

[Back up →](06-backup.md)
