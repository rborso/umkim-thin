# GPU agent and driver install (Option B)

Status: **design — nothing implemented.** Last reviewed 2026-11-20.
Parent: [`apps-plan.md`](apps-plan.md) (the AI stack this unblocks).
Companions: [`security-plan.md`](security-plan.md), [`install.md`](install.md),
[`console.md`](console.md) (the storage tab whose pattern this copies).

The goal: a GPU node becomes *usable* — driver installed, container runtime
configured, `nvidia.com/gpu` schedulable — without an operator that wants to own
the kernel, and without anyone SSHing in to run `nvidia-smi` to find out why a pod
sits in `CreateContainerError`.

This document chooses **Option B** from `apps-plan.md` § "GPU discovery and driver
install": a root host agent mirroring the storage agent, plus the NVIDIA device
plugin for cluster-side advertisement.

## Why not the GPU Operator

Recorded so the decision is not relitigated:

- It supports Rocky 9, but **RHEL/Rocky 9 drivers do not work on 10** — they are
  built per major, and this platform supports both Rocky 9 and 10.
- `driver.enabled=true` requires a matching driver container **per kernel version**
  (precompiled) or per OS (compiled), so every kernel update is a new artifact.
- It is a large, privileged, fast-moving dependency for a platform whose whole
  selling point is a small pinned surface.

It stays an option later with `driver.enabled=false toolkit.enabled=false`, purely
for DCGM/validator, once the host side works. Not before.

## What already exists to copy

The storage agent is the template, deliberately:

| Storage (`internal/storage`, `cmd/umkim-thin/storage.go`) | GPU equivalent |
|---|---|
| root systemd unit `/etc/systemd/system/umkim-thin-storage.service` | `umkim-thin-gpu.service` |
| host-only Unix socket `/run/umkim-thin/storage.sock`, mode 0660, root + gid 65532 | `/run/umkim-thin/gpu.sock`, same ownership |
| console talks to it via `storageAPI` over a Unix http.Client | `gpuAPI`, same client |
| `GET /healthz`, `GET /disks`, `GET /state` | `GET /healthz`, `GET /gpus`, `GET /state` |
| `POST /plan` returns a validated, human-reviewable `Plan` | `POST /plan` |
| `POST /apply` re-plans, then performs; destructive ops need `Confirm` (`"FORMAT"` for disks) | `Confirm: "INSTALL"` |
| `available()` checks the socket exists; every console call degrades when absent | same |

Two properties of that model are worth keeping verbatim:

1. **The operation set is fixed.** No arbitrary command execution, no shell
   passthrough. The agent exposes verbs; the console cannot ask it to run anything
   else. That is what makes a root socket acceptable.
2. **Plan before apply.** `Apply` re-plans internally, so the reviewed plan and the
   executed plan cannot diverge. Destructive operations require an explicit token
   in the request, and a missing token is an error rather than a default.

## The agent

### Endpoints

```
GET  /healthz          -> "ok"
GET  /gpus             -> discovery: PCI devices, driver, CUDA, runtime, plugin
GET  /state            -> what this agent has installed/changed (state file)
POST /plan             -> validated plan, no side effects
POST /apply            -> performs it; requires {"confirm":"INSTALL"}
POST /remove           -> reverts what /state says this agent did
```

### `GET /gpus` — discovery (the first deliverable)

Read-only. No install, no reboot. This is what makes the rest safe to build, and it
alone fixes "why does my pod not start".

```json
{
  "node": "thin-ha-0",
  "os": "rocky",
  "osMajor": 9,
  "kernel": "5.14.0-427.el9.x86_64",
  "gpus": [
    { "pci": "10de:2684", "name": "NVIDIA GeForce RTX 4090",
      "memoryMiB": 24564, "driver": "570.133.20" }
  ],
  "driver": { "installed": true, "version": "570.133.20", "managed": "dkms",
              "matchesKernel": true },
  "toolkit": { "installed": true, "version": "1.17.4" },
  "runtime": { "name": "nvidia", "configuredInContainerd": true },
  "advertised": { "resource": "nvidia.com/gpu", "count": 1, "devicePlugin": false },
  "problems": [
    "driver installed but nvidia.com/gpu is not advertised: no device plugin running"
  ]
}
```

Everything here is obtained without side effects:

| Field | Source |
|---|---|
| `gpus[].pci` / `name` | `lspci -Dn -d 10de:`, or `/sys/bus/pci/devices/*/vendor` |
| `gpus[].memoryMiB` / `driver` | `nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader` (absent → driver not loaded) |
| `driver.matchesKernel` | `modinfo nvidia` vs `uname -r`, plus `lsmod` entry |
| `toolkit` | `nvidia-container-toolkit --version` / rpm query |
| `runtime` | `/etc/containerd/config.toml` contains an `nvidia` runtime with `nvidia-container-runtime` |
| `advertised` | read the node object via the kubeconfig: does `status.allocatable` carry `nvidia.com/gpu`, and is a device-plugin pod Running |

The `problems` list is the actual product value. It turns a wedged pod into a
sentence: *driver installed but the resource is not advertised — no device plugin*.

### `POST /plan` → `POST /apply`

Plan is pure validation. For this agent the only mutations are driver/toolkit
installation and a possible reboot, so the request is small:

```json
{ "action": "install",
  "driverBranch": "570",
  "method": "dnf",
  "allowReboot": true,
  "confirm": "INSTALL" }
```

`Plan` returns the reviewable steps, the repo it would enable, the packages it
would install, whether a reboot is required, and warnings:

```json
{ "steps": [
    "enable repo: cuda-rhel9 (from https://developer.download.nvidia.com/...)",
    "dnf module install nvidia-driver:570-dkms",
    "dnf install nvidia-container-toolkit",
    "nvidia-ctk runtime configure --runtime=containerd  (writes /etc/containerd/config.toml)",
    "restart containerd",
    "verify: nvidia-smi -L lists 1 device" ],
  "rebootRequired": true,
  "warnings": ["containerd restart will restart all pods on this node"] }
```

**Hard rules, matching the storage agent's discipline:**

- `apply` **re-runs plan** and refuses if the plan would differ from what was
  reviewed (the caller passes the plan hash; a mismatch is a 409).
- `apply` without `{"confirm":"INSTALL"}` is an error.
- The agent **never** edits `config.toml` blindly: it checks for an existing
  `nvidia` runtime first, and writes a backup before changing anything.
- **DKMS, not precompiled**, so a kernel update does not break the driver. That is
  the single biggest reliability choice here; it trades a slower first install for
  not having to rebuild per kernel.
- Reboot only when the plan says so *and* `allowReboot` is true. Without it, the
  agent reports `rebootRequired` and exits cleanly — the Ansible role can then do
  the reboot with the existing, tested block.

### Detection before action

The agent must be idempotent and must refuse to act on a machine it does not
understand. Order of checks, each producing a clear error rather than a partial
install:

1. Is this the distro we expect (Rocky/RHEL, major 9 or 10)? If the major is one we
   have no driver repo for, **stop** — this is exactly the Rocky-10 gap.
2. Is `lspci` present and does it report a `10de:` device? No GPU → refuse to
   install a driver.
3. Is a driver already present? Then do not reinstall; report the version, and check
   `matchesKernel`.
4. Is Secure Boot on? If so, the DKMS module must be signed; without MOK enrolment
   the module will not load. **Report this as a blocker** rather than installing a
   driver that silently fails to load on next boot.

That last one is the failure mode that wastes the most time, so it is checked
first and reported plainly.

## Host-side: the Ansible role

A `gpu` role, modelled on `common`'s kernel handling. The relevant precedent
already in the tree:

```yaml
- name: boot a kernel that can do iptables
  when: running_xt.matched | int == 0
  block:
    - ansible.builtin.debug: msg="..."
    - ansible.builtin.reboot: { reboot_timeout: 900, post_reboot_delay: 15 }
    - ansible.builtin.setup:          # re-gather facts
    - ansible.builtin.find: ...       # confirm it worked
```

The GPU role is the same shape with `nvidia-smi` as the assertion:

| Step | Detail |
|---|---|
| gather | `lspci -d 10de:`; skip the role entirely when no GPU |
| repo | `dnf config-manager --add-repo` for `cuda-rhel9` / `cuda-rhel10` by major |
| driver | `dnf module install nvidia-driver:<branch>/latest-dkms` |
| toolkit | `dnf install nvidia-container-toolkit` |
| runtime | `nvidia-ctk runtime configure --runtime=containerd` |
| restart | `systemctl restart containerd` — node-local, brief |
| reboot | only if a new kernel landed; reuse the `common` reboot block |
| verify | `nvidia-smi -L` non-empty, `nvidia-ctk` config present, module loaded for the **running** kernel |

Two constraints from the existing tree that the role must respect:

- **SELinux is `Enforcing` on the lab hosts** (verified with `getenforce` on
  dev-single; `container-selinux` and `k3s-selinux` are installed and the playbook
  manages policy). The driver and toolkit must work under enforcing, not by
  disabling it — and note the playbook has no `setenforce`/`SELINUX=` anywhere, so
  switching to permissive would be a deliberate new change, not an existing default.
  If a step only works permissive, that is a finding to fix, not a workaround to
  ship.
- The role must be **idempotent and safe to skip**: a CPU-only node runs the whole
  playbook unchanged, exactly as `storage_agent | default(false)` gates the storage
  unit today.

## Cluster-side: advertising the resource

The host work does **not** create `nvidia.com/gpu`. That needs a device plugin:

- **NVIDIA device plugin DaemonSet** (small, unprivileged-ish, `nodeSelector` on
  `feature.node.kubernetes.io/pci-10de.present=true`), which publishes
  `nvidia.com/gpu` in `status.allocatable`.
- Optionally **NFD** for that label and for `nvidia.com/gpu.present`. Without NFD the
  plugin can still be pinned with an explicit `nodeSelector` on the node name, which
  is simpler for a single-GPU-node install.
- Optional: **DCGM exporter** if metrics are wanted — out of scope here.

Then a chart asks for it the plain way:

```yaml
resources:
  limits:
    nvidia.com/gpu: 1        # NOT also setting runtimeClassName
```

**Never both** `nvidia.com/gpu` and `runtimeClassName: nvidia` in the same pod. One
mechanism per cluster; mixing them yields pods that schedule and then cannot see a
GPU — the exact class of failure this document exists to remove.

## What the console does with it

1. `GET /gpus` on the target node when an AI app is being installed.
2. If `advertised.devicePlugin` is false → **refuse the install** with the
   `problems[]` text, rather than starting a pod that will wedge.
3. If the driver is installed but the kernel does not match → report
   `rebootRequired`, do not install.
4. Show `GET /gpus` in the console as a node capability, so GPU nodes are visible
   next to storage.

This is the same shape as the storage tab: capability discovered from the host, an
explicit plan, then a mutation with a confirmation token.

## Testing

The agent is host-root and touches the kernel, so the tests are the safety net:

| Level | What |
|---|---|
| unit | `Plan` for each distro/kernel/GPU combination, from **fixture** `lspci`/`nvidia-smi`/`config.toml` outputs — no GPU needed. This is the bulk of the coverage and runs in `ci/static.sh` via `go test`. |
| unit | `Apply` refuses without `confirm=INSTALL`; refuses when the plan hash differs; refuses on an unsupported distro major. |
| unit | idempotence: plan on an already-installed node returns "nothing to do" |
| lab | one GPU node: `POST /plan`, review, `POST /apply`, then `nvidia-smi -L` and a trivial pod requesting `nvidia.com/gpu: 1` actually schedules |
| gate | `ci/assert.sh` pre-flight: node advertises `nvidia.com/gpu` and `nvidia-smi` inside a pod lists a device. Without it, "AI app will not start" is indistinguishable from a bad image |

The unit tests are what make this reviewable without hardware: fixture the command
outputs and assert the plan. A GPU is only needed for the last two rows.

## Order of work

1. `GET /gpus` only, read-only, no install — with fixtures and unit tests. Ship this
   first: zero risk, and it immediately explains GPU problems.
2. Console surface for it (read-only).
3. `POST /plan` + `POST /apply` with the confirmation and plan-hash rules.
4. Ansible `gpu` role (reuses the reboot/verify pattern); `POST /apply` and the role
   must not both do it — the role is the supported path for a fresh install, the
   agent is for a running node.
5. Device plugin DaemonSet; `ci/assert.sh` pre-flight.
6. Only then the AI stack charts.

## Open questions

1. **DKMS vs precompiled** — DKMS is recommended above for kernel-update survival,
   but it needs `kernel-devel`/`gcc`/`make` on the host and lengthens the install.
   Confirm that trade on a real Rocky 9 and a real Rocky 10 node.
2. **Secure Boot** — if the lab hosts have it on, MOK enrolment is a manual step
   (`mokutil --import`), which is not automatable without physical/console access.
   Decide whether to support it or require Secure Boot off.
3. **Who reboots** — the Ansible role does it today; the agent would need to. Pick
   one owner to avoid a reboot loop, and make the other refuse.
4. **Rocky 10 support** — confirm whether NVIDIA now publishes `cuda-rhel10` drivers
   for the branch we want. If not, Rocky 10 GPU nodes are unsupported and the role
   must say so rather than half-installing.
5. **Marketplace/interconnect** — not relevant to a single-GPU node; listed only so
   Phase C (multi-node tensor parallel) is not confused with this work.
