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

**Target is Rocky Linux 10 only.** All four lab hosts are Rocky Linux 10.2 on
`6.12.0-211.el10_2` (verified), and the platform supports nothing else in practice.
That removes the EL9/EL10 branching from the design: there is one install path, and
this document specifies it. EL9 support, if ever needed, is a catalog entry — not a
code path.

## Why not the GPU Operator

Recorded so the decision is not relitigated:

- Its `driver.enabled=true` path needs a matching driver container **per kernel
  version** (precompiled) or per OS (compiled), so every kernel update is a new
  artifact to track — for no benefit over `dnf` on a single-OS fleet.
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
  "osMajor": 10,
  "kernel": "6.12.0-211.49.1.el10_2.x86_64",
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
    "enable repo: cuda-rhel10 (from https://developer.download.nvidia.com/...)",
    "dnf install nvidia-driver nvidia-driver-cuda kmod-nvidia-open-dkms",
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

1. Is this Rocky/RHEL **major 10**? If not, stop: the platform targets EL10 only,
   and a silent EL9 install would produce a driver for the wrong kernel ABI.
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

The GPU role is the same shape with `nvidia-smi` as the assertion. The concrete
steps are not guessed — see "What the umkim project already does" below for the
version arrived at on real hardware, which this role should follow almost verbatim:

| Step | Detail |
|---|---|
| gather | `lspci -nn`, assert a `10de:` device; read `mokutil --sb-state` |
| catalog | look up PCI id → driver branch + per-OS overrides (not a fixed branch) |
| repo | `cuda-rhel10.repo` (EL10 has no DNF module stream) |
| prereqs | `kernel-devel-$(uname -r)`, `kernel-headers-$(uname -r)`, `gcc`, `make`, `pciutils`, `elfutils-libelf-devel`, EPEL (for dkms) |
| nouveau | blacklist + rebuild initramfs |
| driver | direct packages from `cuda-rhel10` + `nvidia-driver-selinux`, `crb`, EGL libs, and per-GPU open-vs-proprietary |
| dkms | `dkms autoinstall` after the packages |
| reboot | only when the install changed something |
| verify | `nvidia-smi -L` with retries, then **pin the boot kernel** with `grubby` |
| secure boot | import the DKMS MOK key only if SB is on **and** `nvidia-smi` failed; print the manual enrolment step |

The toolkit and runtime are **post-cluster**, not part of this role's driver phase:
`nvidia-container-toolkit`, `nvidia-ctk runtime configure --runtime=containerd
--nvidia-set-as-default`, persist `default-runtime: nvidia` in
`/etc/rancher/k3s/config.yaml`, restart k3s.

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

- **NVIDIA device plugin DaemonSet**, which publishes `nvidia.com/gpu` in
  `status.allocatable`. The working configuration (from the prior art) is a small
  ConfigMap + DaemonSet with `deviceDiscoveryStrategy: nvml`,
  `failOnInitError: false`, tolerating `nvidia.com/gpu:NoSchedule`,
  `priorityClassName: system-node-critical`, and a pinned image
  (`nvcr.io/nvidia/k8s-device-plugin:v0.19.3`).
- **NFD is not required** if the cluster default runtime is already `nvidia` and the
  DaemonSet simply runs on every node — which is what the prior art does. Adding NFD
  only matters if workloads need `pci-10de.present` labels to select nodes.
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

## What the umkim project already does

`rborso/umkim@devel` has a working `gpu_host` Ansible role with a hardware catalog.
Its answers to this document's questions are worth recording, because they were
arrived at on real hardware and they settle most of what would otherwise be
guesswork here. Summarised, not pinned to a commit — re-check before relying on a
specific value.

### Hardware catalog, not a hardcoded branch

`data/gpu/catalog.yaml` maps **PCI device id → driver branch + vram + per-OS
overrides**, keyed by `10de:XXXX`:

```yaml
cuda_repo:
  url: https://developer.download.nvidia.com/compute/cuda/repos/rhel10/x86_64/cuda-rhel10.repo
  filename: cuda-rhel10.repo

pci_devices:
  # Turing and newer: the open module on the current branch.
  "10de:2684":
    marketing_name: NVIDIA GeForce RTX 4090
    vram_gb: 24
    kmod_flavor: open
  # Pascal-era mobile: not supported by the open module, and the proprietary
  # branch is capped, so the version must be pinned.
  "10de:1c8c":
    marketing_name: NVIDIA GeForce GTX 1050 Ti
    vram_gb: 4
    kmod_flavor: proprietary
    driver_version: "580.196.07"
```

That is the answer to "which branch": **not one number, a lookup** — per GPU and per
PCI id. A single hardcoded branch is what breaks the moment a second GPU model
appears — or, as here, when a mode switches to the newest hardware branch.

### EL10: there is no DNF module stream

The single structural difference from the older path, and the one that trips people
up:

- **EL9** used `dnf module enable nvidia-driver:<branch>-dkms`.
- **EL10 has no modules.** You install packages directly from the `cuda-rhel10`
  repo. Anything that runs `dnf module ... nvidia-driver` on Rocky 10 either does
  nothing or fails, because there is no such stream.

Verified against the live repo (`cuda-rhel10.repo` returns 200, gpgcheck on):

| Package | Present on `rhel10/x86_64` |
|---|---|
| `nvidia-driver`, `nvidia-driver-cuda`, `nvidia-driver-libs`, `nvidia-driver-cuda-libs`, `nvidia-driver-common` | yes |
| `nvidia-driver-selinux` | yes — relevant, SELinux is enforcing |
| `nvidia-container-toolkit`, `-base` | yes |
| `kmod-nvidia-open-dkms` (open kernel module) | yes |
| `kmod-nvidia-latest-dkms` (proprietary kernel module) | yes |

Current versions in the repo at the time of writing: driver **620.70.06**,
open DKMS up to **620.70.06**, and the **proprietary** DKMS tops out at
**610.57.04**. That gap is the whole reason the catalog carries a per-GPU flavour
flag — a card needing the proprietary module cannot be on the newest branch.

### Rocky 10 needs more than the driver package

Beyond `nvidia-driver*`, an EL10 install needs:

- `kernel-modules-$(uname -r)` and `kernel-modules-extra-$(uname -r)` — k3s and the
  driver both want netfilter/drm modules that the minimal EL10 image omits;
- `kernel-devel-$(uname -r)`, `kernel-headers-$(uname -r)`, `gcc`, `make`,
  `elfutils-libelf-devel` — DKMS builds against the **running** kernel;
- **CRB** enabled (`dnf config-manager --set-enabled crb`) and **EPEL** for `dkms`;
- `egl-gbm`, `egl-wayland2`, `egl-x11` — the driver libs link against these on EL10;
- `nvidia-driver-selinux` — with SELinux enforcing, this is not optional.

### Open vs proprietary is per-GPU, and it is a real fork

- **Open** (`kmod-nvidia-open-dkms`) tracks the newest branch (620.x) and is the
  default. It does **not** support pre-Turing hardware.
- **Proprietary** (`kmod-nvidia-latest-dkms`) is required for Pascal and older, and
  is pinned to an explicit version (e.g. `580.196.07` or `610.57.04`) rather than
  "latest", because the branch caps out.

So the catalog entry per PCI id carries `kmod_flavor` and, when proprietary, an
explicit `driver_version`. Guessing "open on latest" is what produces a driver that
installs cleanly and then fails to find the card.

### The operational details that matter

From `roles/gpu_host/tasks/driver.yml`, each of which is a problem someone already
hit:

| Step | Why |
|---|---|
| `kernel-devel-$(uname -r)`, `kernel-headers-$(uname -r)`, `gcc`, `make`, `elfutils-libelf-devel` | DKMS builds against the **running** kernel; the unversioned packages install for the newest, which may not be running |
| blacklist `nouveau` + rebuild initramfs | the open driver otherwise claims the card first and the proprietary module will not load |
| `dkms autoinstall` after package install | the RPM's own hook does not always fire on a running system |
| **pin the boot kernel** (`grubby --set-default /boot/vmlinuz-$(uname -r)`) | otherwise the next reboot may come up on a kernel the module was not built for |
| `epel-release` when using DKMS | `dkms` lives in EPEL |
| reboot only when the install changed something | avoids a needless reboot on every idempotent run |
| `nvidia-smi -L` with retries (12 × 10 s) after reboot | the module takes a moment to load |
| `mokutil --sb-state`, and only import the MOK key when Secure Boot is on **and** `nvidia-smi` failed | MOK import is only needed in that case, and it is a password-bearing command (`no_log: true`) |

### Secure Boot + MOK: it is automatable, with one manual step

The role does `mokutil --import /var/lib/dkms/mok.pub` with a vault password when
Secure Boot is enabled and `nvidia-smi` failed. MOK enrolment still needs one
interactive confirmation at the blue MOK screen on next boot — the role prints
exactly that instruction. So: **partially automatable**; the design must surface it
as a known manual step, not pretend it is unattended.

### Runtime: default runtime, not RuntimeClass

This is the detail that differs from what I would have chosen:

- `nvidia-ctk runtime configure --runtime=containerd --nvidia-set-as-default` against
  `/var/lib/rancher/k3s/agent/etc/containerd/config.toml`
- **plus** `default-runtime: nvidia` persisted in `/etc/rancher/k3s/config.yaml`, so
  a k3s restart does not lose it
- then restart k3s and wait for the API

The device plugin is deployed with `deviceDiscoveryStrategy: nvml` and
`failOnInitError: false`, tolerating `nvidia.com/gpu:NoSchedule`, as
`system-node-critical`, pinned to `nvcr.io/nvidia/k8s-device-plugin:v0.19.3`. It sets
`runtimeClassName` **only if configured to**, defaulting to false *because the
cluster default runtime is already nvidia*.

**So the platform should follow that**: make `nvidia` the **default runtime** and
leave `runtimeClassName` off. It avoids the "resource and runtimeClass both set"
trap from earlier in this document by removing the choice.

### Order of operations, as actually implemented

1. Host driver, **before** the cluster is bootstrapped (or `--force` on a Ready
   cluster). Driver first, because k3s coming up mid-install while the module
   changes is worse.
2. Reboot, verify `nvidia-smi`.
3. Cluster bootstrap.
4. Container toolkit + device plugin, then a capability flag
   (`fleet.capability/gpu=true`) so workloads can require a GPU-capable node.

That is a cleaner sequence than "agent can do all of it": driver is pre-cluster,
runtime is post-cluster, and the two never race.

## What this changes in the design above

- **Drop the single `driverBranch` field** from the plan request. The agent takes
  the PCI id and **looks it up** in a catalog shipped with the platform, exactly like
  the storage agent resolves storage classes. On EL10 the OS major is a constant, so
  the only variable is the card.
- **Drop the EL9/EL10 branching** — one install path, direct packages.
- **Drop "DKMS vs precompiled"** — DKMS against `kernel-devel-$(uname -r)` is the
  answer; precompiled-per-kernel is the thing to avoid.
- **Drop "who reboots"** — the driver install reboots (it must; NVIDIA says so and
  `dnf needs-restarting -r` confirms). The agent reports `rebootRequired` and the
  console cordons/drains around it.
- **Secure Boot** is a *known manual step* with an automated MOK import attempt.
- **Runtime**: `nvidia` as the containerd **default**, not `runtimeClassName`.
- **Add** `grubby` boot-kernel pinning, `nouveau` blacklisting, `nvidia-driver-selinux`,
  CRB, EPEL, the EGL libs, and `kernel-modules-$(uname -r)`.

## Decisions taken for Rocky 10

| Question | Decision |
|---|---|
| OS support | **EL10 only.** Anything else is refused by the agent, not half-installed. |
| Driver source | `cuda-rhel10` repo, direct `dnf install` — no DNF module stream. |
| Kernel module | **DKMS**, built against the running kernel, with `grubby` pinning that kernel as the default boot entry. |
| Open vs proprietary | Per-PCI-id `kmod_flavor` in the catalog. Open by default; proprietary pinned to an explicit version for pre-Turing cards. |
| SELinux | Enforcing stays. `nvidia-driver-selinux` is part of the install set, not an afterthought. |
| Container runtime | `nvidia` as containerd's default (`--nvidia-set-as-default`), persisted in `/etc/rancher/k3s/config.yaml`. `runtimeClassName` stays unset everywhere. |
| Device plugin | Pinned DaemonSet, `deviceDiscoveryStrategy: nvml`, `failOnInitError: false`, tolerating `nvidia.com/gpu:NoSchedule`. NFD not required. |
| Driver install timing | **Before any app is installed on that node**, with cordon + drain around the reboot. Reboot is required, not conditional. |
| Multi-GPU | Supported. `GET /gpus` returns every card; llama.cpp splits with `--split-mode layer` (default) or `tensor`, sized by `--tensor-split`. |
| CPU-only | **First-class.** `:server` image, `-ngl 0`, no GPU resource. The AI stack must install and work on a cluster with zero GPUs. |

## Does it really need a reboot?

**Yes — and this is not a superstition, it is stated by NVIDIA and checkable.**

From the NVIDIA driver installation guide (§ Reboot Notification): *"The kernel
modules and the user space components of the driver must always be of the same
version. After a driver installation or upgrade, the system must be rebooted to load
the new kernel modules."*

The driver packages register themselves with DNF as reboot-requiring, the same way
the kernel does. So the requirement is machine-checkable rather than inferred:

```sh
dnf needs-restarting -r          # exit 1 = reboot required, exit 0 = not
```

That command comes from `dnf-plugins-core`, which **is installed** on the lab hosts
(verified: `dnf-plugins-core-4.7.0-10.el10`), though the binary itself is not on
`dev-single` today. `nvidia-driver` and `nvidia-driver-cuda` drop
`nvidia-driver.conf` / `nvidia-driver-cuda.conf` into
`/etc/dnf/plugins/needs-restarting.d/`, which is what makes the check meaningful.

Could it be avoided? Technically sometimes — `modprobe nvidia` works if nothing has
claimed the card, and you can unbind the console, `rmmod nouveau`, drop and re-scan
the PCI bus. That is what the "install without rebooting" recipes do, and they are
fragile: they require no nouveau claim, no X/Wayland, and no in-flight CUDA process,
and they do not update the user-space/kernel-space version pairing the docs insist
on. **On a node running k3s, do not do this.** The supportable path is reboot.

So the design treats reboot as **required**, not conditional on a kernel change:

| Situation | Action |
|---|---|
| Fresh driver install | reboot required, always |
| Driver version change | reboot required (user/kernel-space mismatch otherwise) |
| Nothing changed (idempotent run) | `dnf needs-restarting -r` decides; usually no reboot |
| Kernel updated, driver rebuilt by DKMS | reboot **and** verify the new kernel is the one booted |

The `plan` therefore reports `rebootRequired` from `dnf needs-restarting -r` rather
than guessing, and the agent's `apply` refuses to close out with `rebootRequired`
still true.

## Ordering: drain, then reboot, and only before app install

The decision: **cordon + drain → reboot → uncordon**, and the docs and console
should insist it happens **before any app is installed on that node**.

```sh
kubectl cordon <node>
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data --timeout=600s
# install driver, reboot
kubectl uncordon <node>
```

Why before app install, and why the warning is not optional:

- Draining guarantees nothing important is running, but it cannot guarantee nothing
  *will be* — if an app schedules onto the node mid-reboot it lands on a node whose
  kubelet is about to disappear, and the pod enters a confusing CrashLoop or
  `NodeLost` rather than a clean reschedule.
- A reboot on a single-node install is a **full platform outage**, not a rolling one.
  On `dev-single` there is nowhere for the workloads to go, so "drain" is a courtesy
  and the reboot is an outage. The console must say that plainly instead of implying
  a graceful migration.
- On HA the drain is real: two other nodes absorb the pods.

So the warning has three distinct texts, and the agent should pick by cluster shape:

| Cluster | Warning |
|---|---|
| single node | "Reboot will stop **every** workload on this node. There is no other node to move them to. Do this before installing apps." |
| multi-node, other nodes healthy | "Workloads will move to other nodes. GPU workloads will stop until the reboot finishes." |
| any node with GPU apps installed | "A GPU app is installed. Prepare the driver **before** installing it, or expect an outage." |

The console should refuse to start a driver install while any app is installed on
that node unless the operator explicitly acknowledges — a second confirmation, not
just the `INSTALL` token.

## Multi-GPU per host

Supported, and it needs almost nothing from the platform — it is a chart-value
problem, which is the good kind.

**llama.cpp does the work.** Verified from upstream `docs/multi-gpu.md`:

| Mode (`--split-mode`) | Behaviour | Use |
|---|---|---|
| `layer` (**default**) | pipeline parallel: each GPU holds a contiguous slice of layers, KV lives with its layer | default; tolerant of slow interconnect |
| `tensor` (experimental) | splits weights **and** KV across GPUs; multiple cross-GPU reductions per layer | fastest token generation, but bottlenecked by interconnect |
| `none` | one GPU only, chosen by `--main-gpu` | pin a model to one card |

Relevant flags: `--tensor-split N0,N1,...` (proportions, e.g. `3,1` = 75/25, ordered
by `--device`), `--main-gpu`, `--n-gpu-layers` (`all`/`999` to offload everything),
`--list-devices` to enumerate.

So the chart's contract is:

```yaml
resources:
  limits:
    nvidia.com/gpu: 2          # as many as the node has
gpu:
  splitMode: layer             # or tensor
  tensorSplit: "1,1"           # optional; default splits by memory
  nGpuLayers: all
```

and the device plugin advertises `nvidia.com/gpu: 2` from the node, which it does
automatically once both cards are visible — that is a node-level property, not a
per-app one.

**What the agent must do for this:** report **all** cards from `lspci`, not the first
match. The prior art takes `gpu_host_pci_matches | unique | first`, which is fine for
validating one card but loses the second. `GET /gpus` returns an array with an index
per device, plus a `count`, and the catalog lookup runs **per card** — a mixed
node (e.g. one Turing + one Pascal) is legal, and each card gets its own
`kmod_flavor`/version.

Mixed flavours on one host are the one genuinely awkward case: you cannot load both
the open and proprietary modules for different cards on the same kernel in the usual
setup. The rule: **if any card requires proprietary, the whole node uses
proprietary**, and the plan says so with a warning naming the cards that forced it.

## CPU-only inference (no GPU at all)

Explicitly supported and required. llama.cpp's plain `server` image is the CPU
build; `server-cuda` is the GPU one. Verified from upstream's own docker example:

```sh
# CPU
ghcr.io/ggml-org/llama.cpp:server
# GPU
ghcr.io/ggml-org/llama.cpp:server-cuda --n-gpu-layers 99
```

So the platform has **one app with two modes**, not two apps:

| Node | Image | `-ngl` | Resource request | Notes |
|---|---|---|---|---|
| no GPU | `:server` | `0` (or omit) | none | works today, no device plugin, no driver |
| GPU | `:server-cuda` | `all` | `nvidia.com/gpu: N` | needs everything in this document |

Consequences worth building in:

- **The AI stack must install on a cluster with zero GPU work.** Phase A/B of
  `apps-plan.md` should not be blocked on the driver work at all — the same LiteLLM
  + Open WebUI deployment points at a CPU llama.cpp when no GPU exists.
- **The console must not refuse the install** just because `GET /gpus` returns
  nothing; it should offer CPU mode and say what that means for speed.
- **The pre-flight assert must not require a GPU** — it should assert "either a
  device plugin advertises `nvidia.com/gpu`, or the deployment is CPU-only", and
  fail only on the contradictory case (GPU resource requested, none advertised).
- CPU throughput is a few tokens/second for anything above ~3B; the readiness and
  gate timeouts must account for that, or the AI checks become flaky exactly where
  the hardware is weakest.

This also means the driver work is an **enhancement**, not a prerequisite, which is
a much better place for it in the roadmap: ship CPU-only inference first, add GPU
support without changing the app contract.

## Verified on real Pascal hardware

The first GPU run happened on the lab's only card, a **Pascal GTX 1050 Ti Mobile
(10de:1c8c, 4 GiB)** on the host `172.16.10.4`. It confirmed the parts of this
design that could not be tested without a card, and turned two predictions into
facts.

**The install order held.** The platform was installed first, on a bare Rocky
Linux 10.2 host, then the driver work ran with **no apps installed** — so the
single-node reboot was an outage of nothing. This is the sequence the document
argues for, and it was worth it: the driver install needed a kernel change.

**Pascal needs the proprietary, pinned branch, exactly as predicted.** The host
was on an old point kernel (`6.12.0-211.16.1.el10_2.0.1`) with no matching
`kernel-devel` in the repos, so DKMS could not build. The fix was a kernel update
to `el10_3` and a reboot, then:

```
kmod-nvidia-latest-dkms-580.196.07  nvidia-driver-580.196.07
nvidia-driver-cuda-580.196.07
```

The open module was not attempted on the card, consistent with the catalog schema
that flags it per PCI id.

**The EL10 extras this document listed are all required, not advisory.** The
first install attempt failed on missing `libglvnd-opengl`, `libvdpau`, `egl-gbm`,
`egl-wayland2` and `egl-x11`. Installing the driver packages alone is not enough
on EL10.

**Reboot: confirmed required.** `dnf needs-restarting -r` reported a pending
reboot, and `nvidia-smi` only appeared after the reboot onto the new kernel.

**The device plugin's `auto` strategy needs a default runtime.** With the nvidia
runtime merely *defined* in containerd, the plugin logged "Incompatible strategy
detected auto" and found no devices. Setting `default-runtime: nvidia` in
`/etc/rancher/k3s/config.yaml` (and restarting the plugin) made it register:

```
Registered device plugin for 'nvidia.com/gpu' with Kubelet
node capacity: nvidia.com/gpu: 1
```

A pod requesting `nvidia.com/gpu: 1` then ran `nvidia-smi` successfully.

**The AI stack on the card.** The chart's `gpu.enabled` switched the backend to
`llama.cpp:server-cuda`, requested one card, and the container saw
`CUDA0: NVIDIA GeForce GTX 1050 Ti (4034 MiB, 3986 MiB free)`. Inference went
from ~1.1 s per short completion on CPU threads to **0.17–0.29 s** once the model
was loaded, with 491 MiB resident. The first request after a fresh start took
~31 s because it pays the load cost, which is why the gate's first check is the
model list rather than a completion.

**What this changes here:** the CPU assertion in `ci/ai-check.sh` had to become a
per-target property. It now asserts the GPU contract on a target that sets
`ai_gpu=true` (a card is requested, the node advertises it, the CUDA image runs)
and keeps the CPU contract everywhere else.

**Still unproven:** multi-GPU splitting (`--split-mode`, `--tensor-split`) — the
lab has one card, so that remains fixture-tested only.

## Tests for the hardware paths

You asked for tests including pre-checks. The split:

**Pre-check (always, cheap, no privileges):**

```sh
lspci -nn -d 10de:            # is there an NVIDIA card at all
nvidia-smi -L                 # is a driver loaded For the running kernel
dnf needs-restarting -r       # is a reboot outstanding
mokutil --sb-state            # Secure Boot on?
```

Each maps to a named failure rather than a stack trace. `lspci` is the gate: no
`10de:` device means the node is CPU-only and the agent must not install a driver.

**Unit tests (fixtures, run in `ci/static.sh` via `go test`)** — the bulk:

| Case | Fixture | Expectation |
|---|---|---|
| no GPU | empty `lspci` | `plan` refuses driver install; CPU mode offered |
| one Turing card | `lspci` + `nvidia-smi` | open kmod, latest branch, no proprietary pin |
| one Pascal card | same, `10de:1c8c` | proprietary, version pinned from catalog |
| **two cards, both Turing** | two `10de:` lines | one plan, both cards listed, `count: 2` |
| **two cards, Turing + Pascal** | mixed | proprietary for the node, **warning naming the forcing card** |
| two cards, one already driven | partial `nvidia-smi` | plan reports the mismatch, does not reinstall blindly |
| EL9 host | `osMajor: 9` | refused with "EL10 only" |
| reboot outstanding | `needs-restarting -r` → 1 | `rebootRequired: true`, `apply` refuses to close |
| Secure Boot + module not loaded | `mokutil` enabled | MOK import planned, manual step reported |
| idempotent re-run | fully installed | "nothing to do", no reboot planned |

**Multi-GPU integration test (needs hardware, or a faked device plugin):** assert the
node advertises `nvidia.com/gpu: 2`, a pod requesting 2 schedules, and `--list-devices`
inside it shows both. Without 2 cards, a fake device plugin advertising 2 is enough to
prove the *scheduling* half; the llama.cpp split itself needs real cards.

**CPU-only test (no hardware needed, runs everywhere):** deploy the AI stack with
`:server`, assert `/v1/models` and a completion, and assert the app installed with
**no** `nvidia.com/gpu` in its pod spec. This is the one that keeps CPU mode honest.

## Open questions that remain

Most of what this document listed originally is now decided; these are what is left.

1. **Single-node reboot is an outage.** `dev-single` has one node and no GPU, so it is
   not affected today — but if a GPU were ever added there, the drain is meaningless
   and the reboot stops everything. Decide whether the console should refuse a driver
   install on a single-node cluster, or allow it with the loud warning at
   "Ordering: drain, then reboot" above.
2. **Catalog schema** — adopt the umkim `data/gpu/catalog.yaml` shape (PCI id →
   `kmod_flavor` + optional pinned `driver_version`, plus the CUDA repo URL) or
   reshape it for the console's node model. Reusing it means one file answers both
   products, and it is a small schema. Recommended: reuse, extended with a `count`
   and per-index array so multi-GPU works.
3. **Mixed-flavour nodes** — one Turing + one Pascal card forces the whole node to
   proprietary. Is that acceptable, or should the agent refuse a mixed node outright
   so the operator fixes the hardware? (Current design: allow, with a warning naming
   the card that forced it.)
4. **GPU node with apps already installed** — the console should require a second
   acknowledgement beyond `INSTALL`. Confirm the wording/UX rather than inventing it.

Resolved since the first draft, recorded so they are not reopened:

| Was | Now |
|---|---|
| EL9 vs EL10 branching | EL10 only, direct packages (all four hosts verified 10.2) |
| DKMS vs precompiled | DKMS against `kernel-devel-$(uname -r)` |
| Does it need a reboot | Yes — NVIDIA's requirement, `dnf needs-restarting -r` checks it |
| Who reboots | The agent reports, the console cordons/drains, the reboot follows |
| Secure Boot | Automated MOK import + one documented manual step |
| Runtime: default vs RuntimeClass | `nvidia` as the containerd default; `runtimeClassName` never set |
| Multi-GPU | Supported via llama.cpp split modes; agent reports all cards |
| CPU-only inference | First-class, `:server` image, no GPU resource |
