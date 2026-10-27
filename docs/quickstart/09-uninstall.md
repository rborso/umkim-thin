# 9. Uninstall

**Goal:** remove the platform deliberately, with no surprises about data.

## Before you start

- Back up the cluster state and the application data you care about
  ([Backup](06-backup.md)).
- **k3s uninstall deletes cluster state.** `local-path` volumes live under
  `/var/lib/rancher/k3s/storage`, and the uninstall script removes that tree.
  If the data matters, copy it or snapshot the VM first.
- Longhorn data lives at `/var/lib/longhorn` and is kept by default; the
  `--purge-data` option removes it.
- Remove the DNS records and close the firewall ports afterwards.

## Remove the platform

```bash
sudo ./teardown.sh
```

`teardown.sh` is the supported full teardown. It:

1. uninstalls the umkim-thin/app Helm releases (k3s's own system releases are
   left for k3s to remove);
2. stops and removes the host storage agent and its NFS export definitions;
3. runs `k3s-uninstall.sh`/`k3s-agent-uninstall.sh` and removes leftover k3s,
   Calico/CNI and containerd paths;
4. removes the `wg0` interface and NAT rules created by the WireGuard gateway;
5. removes the host firewall ports, trusted zones and k3s-related rules added
   by provisioning;
6. removes the host console binary and the k3s symlinks.

It asks you to type `teardown`. Use `--yes` for non-interactive runs.

`install.sh --uninstall` delegates to the same script, so existing muscle
memory keeps working:

```bash
sudo ./install.sh --uninstall
```

### Options

```bash
./teardown.sh --dry-run          # print the plan; no changes, no root needed
sudo ./teardown.sh --purge-data  # also remove host state and Longhorn data
sudo ./teardown.sh --keep-tools  # keep an independently installed helm
sudo ./teardown.sh --keep-firewall
sudo ./teardown.sh --keep-wireguard
sudo ./teardown.sh --vip 10.0.0.100  # remove a stale kube-vip address
```

`uninstall-ha.sh` passes the VIP from the environment automatically. If you use
`teardown.sh` directly on an HA node, set `--vip` or `K3S_CONTROL_PLANE_VIP`;
otherwise a leftover `/32` address on a node can make k3s try to join itself
instead of the live control-plane VIP on the next install.

By default it **keeps**:

- `/etc/umkim-thin` (secrets, back up first);
- `/var/lib/umkim-thin` (chart mirror);
- `/opt/umkim-thin`;
- `/var/lib/longhorn`;
- `/srv/nfs` and the filesystems on the storage disks.

`--purge-data` also removes those state/chart/Longhorn paths, and unmounts the
recorded `/srv/nfs` mounts and removes their `/etc/fstab` entries. It **does not
wipe the filesystems themselves**; the data remains on disk and can be
remounted or retrieved with normal host tools.

k3s cluster data, including `local-path` application PVCs, is always removed by
the k3s uninstall. There is no flag that keeps a k3s cluster while removing the
rest; use the console's app uninstall if that is what you want.

## HA and other nodes

For the three-node HA target, use the wrapper from the checkout or Ansible
controller:

```bash
./uninstall-ha.sh --env deploy/provision-ha.env --dry-run
./uninstall-ha.sh --env deploy/provision-ha.env

# non-interactive, and remove the kept host/Longhorn data
./uninstall-ha.sh --env deploy/provision-ha.env --purge-data --yes
```

It reads `JUMP`, `VSSH_KEY`, `VUSER`, `VHOST`, `k3s_control_plane_vip` and
`servers` from the environment file and runs this repository's `teardown.sh` on
every server, with the **primary last**. That keeps the API reachable while the
shared Helm releases are removed. It asks once before changing anything;
`--yes` skips the prompt. `--purge-data` is passed through to every host, and
the control-plane VIP is removed from any host interface that still has it.

`teardown.sh` itself acts on **one host**. If you need to run it manually, use
the same order:

```bash
# on each non-primary server
sudo /path/to/umkim-thin/teardown.sh --yes
# finally on the primary
sudo /path/to/umkim-thin/teardown.sh --yes
```

The VIP and external NAT/DNS are infrastructure, not host state, and must be
removed separately.

For disposable test VMs, the maintainer reset in
[`../maintainer-lab.md`](../maintainer-lab.md) reverts a snapshot instead of
tearing down in place. It is not part of the normal uninstall path.

## Remove one app, not the platform

In the console, open **Installed** and use **Uninstall**. The console asks
whether to purge; read the prompt. Purging usually deletes the app's PVCs. If you
only want to stop the app, scale it down instead:

```bash
sudo kubectl -n <app> scale deploy --all --replicas=0
```

(That is an advanced operation; the console's uninstall is the supported path.)

## After uninstalling

- Delete the `auth`, `console`, and wildcard DNS records.
- Close ports 80, 443, and 51820/UDP in the cloud firewall.
- Rotate any credentials that were stored on the host or in the cluster if the
  host is being reused or discarded.
- If the host is being handed to someone else, wipe the disk. A k3s uninstall
  does not shred data.

## Reinstall

A fresh install on a clean host is the supported path. If you intentionally
reuse the host, run `sudo ./teardown.sh --purge-data`, then remove
`/etc/rancher/k3s`, `/var/lib/rancher/k3s`, and the state directories if any
remain, and start from a fresh VM if possible.
