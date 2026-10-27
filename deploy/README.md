# deploy — provisioning a umkim-thin host

Ansible that takes a minimal RHEL-family host to a running umkim-thin install:
prerequisites, firewall, k3s, the ingress/Traefik wiring, and the app stack.
Every role is idempotent.

For the full operator walkthrough — prerequisites, DNS, ports, first login,
invitations, app installs and the HA differences — see
[../docs/install.md](../docs/install.md).

## Use

```sh
# single node
cp deploy/provision-single.env.example deploy/provision-single.env
PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh

# three-node HA: prompted environment, preflight, then run
./install-ha.sh
# or edit and run manually:
./verify-ha.sh deploy/provision-ha.env
PROVISION_ENV=deploy/provision-ha.env ./deploy/provision.sh

./deploy/provision.sh --tags k3s       # a subset
./deploy/provision.sh --check          # Ansible check mode
```

`deploy/provision.env.example` remains the generic single-node example for
backward compatibility; the `-single` and `-ha` files are the supported
starting points.

`deploy/provision.env` is gitignored: it holds addresses, a key path and the
site's domain. The roles under `deploy/ansible/` are generic.

The runner builds the console binary, stages the identity chart and catalog as
`deploy/ansible/artifacts/`, ships the whole tree to the machine named by `JUMP`,
and runs `ansible-playbook` there. `JUMP` is only the Ansible executor; it can
be the first/bootstrap server after the first-run reboot. The `common` role may
reboot the target nodes to load kernel modules; if `JUMP` is one of them, that
reboot kills the Ansible process. Use a separate management host for the first
run, or expect to rerun after the nodes come back. `JUMP` needs `ansible` and
passwordless SSH to every server, including itself.

To run the playbook by hand from a host that can reach the target:

```sh
cd deploy/ansible
ansible-galaxy collection install ansible.posix kubernetes.core   # once
ansible-playbook -i inventory.ini site.yml
```

## Roles

| Role | What |
|---|---|
| `common` | base packages (including the stock kernel, iptables and conntrack), swap off, `ip_forward`, chrony, k3s SELinux policy, and a reboot onto a kernel with netfilter (`xt_*`) support if the running one lacks it |
| `firewall` | firewalld: published NodePorts + WireGuard UDP, flannel/kubelet, API restricted to the lab subnet, cluster networks trusted |
| `k3s` | single-node k3s or an HA control plane; installs Helm and the `/usr/bin/kubectl` shim for sudo |
| `calico` | Calico CNI, pinned pod CIDR, native routing and node readiness |
| `nfs` | NFS provisioner and the shared media StorageClass (`umkim-thin-nfs`, or legacy `nfs`) |
| `longhorn` / `longhorn_host` | HA Longhorn storage classes and host iSCSI prerequisites |
| `identity` | LLDAP + Authelia + the Traefik middleware Secret, from `charts/identity` |
| `traefik` | Traefik on the published NodePorts, ACME HTTP-01, and a file provider for the identity middleware |
| `console` | the console Deployment on a PVC, behind forward-auth; the image is built/imported by `provision.sh` |
| `netpol` | default-deny ingress NetworkPolicies for app namespaces |

## Why a reboot may happen

Some lab images run a stripped kernel with no `xt_*`/`ip_tables` modules, which
leaves iptables (and therefore flannel and kube-proxy) unable to program rules.
The `common` role detects this and reboots into the stock kernel that is
normally installed alongside it.

## Ports

The external NAT publishes `pub_web`/`pub_tls`/`pub_wg` to the host's
`web_nodeport`/`tls_nodeport`/`wg_port`. The host listens only on the internal
ports; nothing here assumes the host is reachable from the internet directly.

## Single node vs HA

The default is a **single node**. For an **HA control plane** (3 servers,
embedded etcd) use `deploy/provision-ha.env.example` and set:

```sh
servers="192.0.2.101 192.0.2.102 192.0.2.103"   # primary first
k3s_control_plane_vip=192.0.2.100
```

The first host initialises the cluster (`cluster-init`); the rest join with
k3s's node-token, read from the first host. `common`, `firewall` and `k3s` run
on every server; the platform roles run once, on the primary (`site.yml` has two
plays). kube-vip is provisioned by the `kube_vip` role; add any extra API DNS
names to `k3s_tls_san`. The servers join through the VIP, so the API has one
stable address (put the external NAT or a load balancer on it too).

HA also changes what the platform assumes:

- **Calico is the CNI** (k3s runs with `flannel-backend: none`), so it also
  enforces the NetworkPolicies — the same engine umkim uses.
- **Storage must be cluster-wide.** `storage_backend` selects it:
  - `local` — `local-path` only (single node; no RWX, so apps like servarr
    can't install);
  - `nfs` — the NFS provisioner (a server on the node, or `nfs_mode=external`);
  - `longhorn` — Longhorn (replicated; HA-capable), 3 replicas by default.
  Set it in the group vars (or leave it empty to keep the explicit classes:
  `local-path` for config, NFS for shared media).
  ```sh
  storage_backend=longhorn      # or nfs / local
  longhorn_replica_count=3      # set 2 on a small lab
  # No NFS yet: media stays on Longhorn RWX.
  storage_class_media=longhorn-rwx
  nfs_storage_class=umkim-thin-nfs
  ```
  External NFS is optional at provision time; add it in the console later, or
  configure it here:
  ```sh
  storage_class_media=umkim-thin-nfs
  nfs_mode=external
  nfs_server=<nfs-host>
  nfs_export=/srv/nfs/umkim-thin
  ```
- **The console is PVC-backed in both modes.** Both single and HA use the same
  chart: a PVC for state, a ConfigMap config, a Secret seed, and the
  ServiceAccount kubeconfig — no hostPath state, no host network. Single adds
  only the host storage-agent socket (`storage_agent=true`,
  `storage_socket=/run/umkim-thin`); HA sets both off.
- **WireGuard has a stable endpoint.** The gateway runs as a per-node
  DaemonSet, so the node holding the kube-vip VIP has a local UDP 51820
  listener. Forward UDP 51820 to the VIP (or a stable UDP load balancer), not
  to a chosen pod. Failover interrupts existing sessions; clients re-handshake.
- **The external NAT needs a VIP/LB** across the nodes for `80/443/51820`;
  otherwise it targets one node (the kube-vip address is a natural choice).


## Teardown

For a single host, run the repository-root teardown:

```sh
sudo ./teardown.sh                 # keep state, NFS content and Longhorn data
sudo ./teardown.sh --dry-run       # inspect the plan first
sudo ./teardown.sh --purge-data    # also remove host state and Longhorn data
```

For HA, use the wrapper so the primary is always last:

```sh
./uninstall-ha.sh --env deploy/provision-ha.env --dry-run
./uninstall-ha.sh --env deploy/provision-ha.env
./uninstall-ha.sh --env deploy/provision-ha.env --purge-data --yes
```

Both paths uninstall the Helm releases, stop the storage agent, remove the
umkim NFS exports, run k3s's uninstall scripts, remove a stale control-plane
VIP (HA wrapper), clean up CNI/WireGuard/firewall state, and leave storage
filesystems intact.
`install.sh --uninstall` delegates to `teardown.sh`. See
[`docs/quickstart/09-uninstall.md`](../docs/quickstart/09-uninstall.md).

## Secrets

Generated once on first run and stored on the target at
`state_dir/secrets.yml` (mode 0600); re-runs reuse them. They populate LLDAP's
admin/JWT secrets and Authelia's session/storage/reset secrets.
