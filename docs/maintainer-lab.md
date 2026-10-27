# Maintainer lab

This page is for maintainers of the project's disposable test lab. It is not
part of the normal install path and is not needed by public users.

The real host addresses, jumphost keys and VM-to-hypervisor mapping live only in
gitignored files: `local/`, `ci/lab.env`, and the filled-in
`deploy/provision*.env` files. Never copy those files into a public issue, PR or
archive.

## Lab VM preparation

The maintainer base image is a stripped Rocky Linux image. Before the first
install, install the stock kernel and netfilter tooling, then reboot:

```bash
sudo dnf -y install kernel kernel-modules iptables iptables-nft conntrack-tools git
sudo reboot
```

The lab router does not forward public 80/443 to the VM, so:

- pass `--acme-staging`; the portal gets Traefik's self-signed default
  certificate, which is expected — use `-k` or ignore the browser warning;
- public names do not hairpin. For checks, add them to `/etc/hosts` on the VM
  (`127.0.0.1 auth.<zone> console.<zone> lldap.<zone> nextcloud.<zone>`) or use
  `curl -k --resolve <host>:443:127.0.0.1`;
- run the doctor with `--insecure`:
  `sudo ./install.sh --doctor --insecure`.

## Reset and provision

Copy `ci/lab.env.example` to `ci/lab.env` on a host that can reach the
hypervisors, then:

```bash
./ci/reset.sh single          # or: ./ci/reset.sh ha
PROVISION_ENV=deploy/provision-single.env ./deploy/provision.sh
./ci/assert.sh single         # run on the lab runner, not the workstation
```

The single-node zone resolves publicly; the HA zone resolves to the VIP only on
the runner (or with `/etc/hosts`), because its public record does not hairpin.
Use the addresses from `local/ci-env.md`, not from the public guides.

The e2e runner and its keys are described in [`ci/README.md`](../ci/README.md).
The Tier 2 run is manual; do not wire it to a public PR workflow.
