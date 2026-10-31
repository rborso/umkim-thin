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

The public endpoint in the generated clients defaults to `wg.<zone>:51820`
(`--wg-endpoint` on `install.sh`, `wg_endpoint` in the Ansible environment).
Set it to the name your DNS points at and that UDP 51820 is forwarded to; the
port is appended only when you leave it out. The console shows the endpoint it
will use next to the client form, and refuses to create a client while it is
unset or the gateway is not installed.

The gateway runs as a DaemonSet: one host-networked pod per node. In HA, the
node that owns the kube-vip VIP therefore already has a local UDP 51820
listener. Forward UDP 51820 to the VIP (or to a stable node address if you
prefer the node-pinned model). Set the public endpoint used in generated
clients to the public DNS name (for example `wg.<zone>:51820`), never the
private VIP. See the [HA quickstart](11-ha-install.md#6-wireguard-on-ha).

## Create a client

1. Console → **WireGuard clients**.
2. Enter a name (for example `laptop`) and click **Create client**.
3. Scan the QR code with the WireGuard app, or download the `.conf` file.
4. Activate the tunnel.

The client receives an address in `192.168.168.0/24` (the gateway is `.1`). Its
allowed IPs are the cluster pod and service networks. Its DNS is the dedicated
VPN DNS service (`10.43.200.53`), not the gateway host address. That service
answers the opted-in app hosts with the gateway address and forwards every
other name to the cluster resolver (which forwards to the node's upstreams).
To use a specific resolver — for example a LAN DNS with split-horizon names —
set the chart's `vpnDNSUpstream` to a comma-separated list.

## Same app URL on the VPN

Same-host access is opt-in. Without `UMKIM_THIN_TRUSTED_ACCESS_ENABLED=true`
the tunnel only carries the pod/service networks: an app hostname still
resolves to its public address, so opening it goes around the tunnel and lands
on the public (Authelia) route. Enable trusted access, then reinstall the app
so its VPN route and the split-DNS entry are published.

With trusted access enabled, an app keeps its public URL inside the tunnel:

```text
https://nextcloud.<zone>
```

The client resolves the app host to the gateway, the gateway DNATs only that
destination to Traefik's internal `vpn` entrypoint, and the route uses the
`vpn-only` source allowlist with no Authelia forward-auth. The app's own LDAP,
OIDC, local login or API token still applies. `auth.<zone>`, `console.<zone>`
and `home.<zone>` are not mapped to the gateway and keep their normal routing.

The trusted route targets the internal `vpn` entrypoint (NodePort 30444) and
uses the `vpn-only` source allowlist. The gateway DNATs the tunnel's :443
there. The dedicated VPN DNS service (`10.43.200.53`) resolves the app hosts;
the WireGuard client receives that address in its `DNS` line.

Trusted access is explicit and off by default:

```text
UMKIM_THIN_TRUSTED_ACCESS_ENABLED=true
UMKIM_THIN_TRUSTED_MANAGEMENT_CIDRS=172.16.10.0/24
```

The management path uses Traefik `ClientIP` on `websecure` and no Authelia;
the public path remains `websecure` + Authelia. The catalog marks apps with
`trusted.management`/`trusted.vpn` and `appAuth`; the app's own login is the
only login on those paths.

The old `.vpn.<zone>` hostname remains available as a compatibility fallback
when a catalog route sets `trusted.sameHost: false` (or when an app is installed
with `exposure: vpn` and no trusted metadata). Do not require it for new native
clients.

Android Private DNS/DoH can bypass the tunnel DNS. If a native app still sees
the Authelia page over the VPN, disable Private DNS for that network or allow
the VPN's DNS. The certificate must also be trusted: use a public certificate
in production, or install the lab CA on the device.

## What this gives you today

- A private path to cluster services and the DNS.
- Same-host app URLs for native Android clients, with no `.vpn` hostname.
- A management-network bypass for operators, using `ClientIP` rather than a
  spoofable header.

It does **not** make the home page or console trusted: home has no app login
and the console has its own login, so both remain behind Authelia.

If you are changing the route itself, read
[`../design.md`](../design.md#wireguard-as-an-in-cluster-workload) and the
catalog entry's `route.mode` before changing it. A private route without split
DNS is a common way to lock yourself out.

## Revoke a client

Delete it in the console. The gateway reloads the peer list; the old key stops
working. Rotate by creating a new client and deleting the old one.

## Troubleshooting

| Symptom | Check |
|---|---|
| The client connects but nothing answers | The allowed IPs are the pod/service CIDRs; the target must be a cluster IP or a name that resolves to one |
| The config imports as `something.conf.txt` | Download it again from the console; the name is sanitized and served as an opaque file now |
| The config has no `Endpoint`, or `Endpoint = :51820` | The endpoint is unset or malformed; set `wg_endpoint`/`UMKIM_THIN_WG_ENDPOINT` to the public host and restart the console |
| An app returns 403 over the VPN | The request reached Traefik but not from the tunnel address; check the gateway pod's iptables (`POSTROUTING` RETURN for the Traefik target, `FORWARD -i wg0`) on a 0.1.5 gateway |
| An app host resolves to the public IP over VPN | The app is not in the trusted VPN host list, or Android Private DNS is bypassing the tunnel DNS |
| The VPN app URL shows Authelia | Split DNS did not map the app host, or the route was installed without `trusted.vpn` |
| The handshake never completes | UDP 51820 is blocked by the cloud firewall, or the endpoint is wrong (set `--wg-endpoint` when installing) |
| It worked, then stopped | Check the node that owns the VIP and the gateway pod on that node; the DaemonSet should cover every node |

## Next

[Back up →](06-backup.md)
