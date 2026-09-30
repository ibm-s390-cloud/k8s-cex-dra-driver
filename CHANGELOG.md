# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

Features graduate on their own, independently of the release that ships them and a gate flip is where behavior changes for an existing user.
Every release entry therefore also records the feature gates it changed: the gate name, its stage, and whether it defaults on.

## Unreleased

### Added

- Container workload path behind the alpha `ContainerWorkload` gate: a claim against `ap-queue.container.ibm.com` gets a zcrypt device node restricted to its allocated queues, mapped to `/dev/z90crypt` in the container through CDI, and removed at unprepare.
- Shadow AP sysfs for container claims: a per-claim tree listing only the allocated cards and queues, mounted read-only over the container's `/sys/bus/ap` and `/sys/devices/ap`, so `lszcrypt` and the crypto libraries see only what the claim can reach. New `--shadow-sysfs-root` option (`SHADOW_SYSFS_ROOT`, default `/var/run/cex-dra/shadowsysfs`).
- Startup reconciliation of zcrypt nodes, shadow sysfs trees and their CDI specs left behind by a crash.
- Preflight check for additional zcrypt device nodes (`/sys/class/zcrypt/create`), gated on `ContainerWorkload`.
- Host mounts for `/sys/class/zcrypt`, `/sys/devices/virtual`, `/var/run/cdi` and the shadow sysfs root in the `feature-container-workload` Kustomize component.

### Changed

- Setting `VirtualMachineWorkload=false` now also skips the AP-bus `driver_override` and all-1s zcrypt mask preflight checks, so a container-only node starts on kernels without AP-bus `driver_override` (before mainline 6.19).
- `controlDomainMode` now applies to container claims: `usage-and-control` grants the allocated domains, `usage-only` none, and a mode that is not enabled grants none instead of being ignored.

### Feature gates

| Gate                     | Stage | Default | Effect when enabled                                                                                                                  |
| ------------------------ | ----- | ------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `ContainerWorkload`      | alpha | off     | Container claims get a zcrypt node restricted to their queues, mapped to `/dev/z90crypt`, and a shadow AP sysfs listing only them.     |
| `VirtualMachineWorkload` | beta  | on      | The VM passthrough path. Disabling it frees a node from the vfio_ap, mdev, driver_override and zcrypt mask checks.                   |

## 1.0.0-alpha.0

First public pre-release, a Technical Preview with no compatibility promise.

### Added

- Kubelet plugin publishing each AP queue (APQN) as a DRA device, with attributes for CEL selection.
- KubeVirt virtual machine passthrough over vfio-ap, through the `ap-queue.virtual-machine.ibm.com` DeviceClass.
- Claim configuration through the `CryptoConfig` envelope (`cex.ibm.com/v1alpha1`), carrying `controlDomainMode`.
- Feature gates on every driver deployable, through `--feature-gates` or `FEATURE_GATES`.
- Kustomize deployment, unprivileged by default.
- Documentation set under `docs/`.

### Feature gates

| Gate                     | Stage | Default | Effect when enabled                                                              |
| ------------------------ | ----- | ------- | -------------------------------------------------------------------------------- |
| `VirtualMachineWorkload` | beta  | on      | The VM passthrough path. Disabling it frees a node from the vfio_ap/mdev checks. |
