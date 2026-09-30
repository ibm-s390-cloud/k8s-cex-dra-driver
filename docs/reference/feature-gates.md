---
title: Feature gates
weight: 5
description: >
  Gate semantics, the gates this release defines, and verifying the resolved map
---

## Feature gates

Every driver deployable takes a `--feature-gates` flag (or the `FEATURE_GATES` environment variable) with comma-separated `Name=bool` pairs.
One value per deployable: a feature spanning several deployables needs the same pairs set on each.

Stage semantics follow the Kubernetes model:

| Stage         | Default    | Setting it                                         |
| ------------- | ---------- | -------------------------------------------------- |
| Alpha         | off        | `Name=true` opts in                                |
| Beta          | on         | `Name=false` opts out                              |
| GA            | on, locked | A leftover setting warns in the log and is ignored |
| GA (unlocked) | on         | `Name=false` stays a supported opt-out             |
| Deprecated    | off        | `Name=true` keeps it alive for one last release    |

GA-unlocked is the exception, declared per gate: it is only for gates whose disabled state is a supported operational configuration - a workload path switch - so graduation does not end the opt-out.
An ordinary feature still locks at GA.

An unknown gate name or a non-boolean value is a startup error - the pod crash-loops instead of running with a misread gate map.

Gates this release defines:

| Gate                     | Stage | Default | Effect when enabled                                                                                                                                                                                                     |
| ------------------------ | ----- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ContainerWorkload`      | alpha | off     | Accepts claims against the container DeviceClass `ap-queue.container.ibm.com`: a zcrypt device node restricted to the allocated queues, mapped to `/dev/z90crypt`, a shadow AP sysfs, and the zcrypt node preflight check. |
| `VirtualMachineWorkload` | beta  | on      | The virtual-machine passthrough path: claims against `ap-queue.virtual-machine.ibm.com`, vfio-ap binding, mdev creation, and its preflight checks (`vfio_ap`, mdev, `driver_override`, all-1s masks).                     |

With `ContainerWorkload` off, a claim against the container DeviceClass fails at prepare with an error naming the gate.
Enabling it delivers the alpha container path described under [Requesting queues](../usage/usage.md#containers).
Setting `VirtualMachineWorkload=false` declares a container-only node: the virtual-machine preflight checks (`vfio_ap`, mdev, AP-bus `driver_override` and the all-1s zcrypt masks) are skipped, so the node runs the driver without the `vfio_ap` kernel module and on kernels without AP-bus `driver_override`, and a claim against the VM DeviceClass fails at prepare with an error naming the gate.
Cleanup is never gated - claims prepared before the flip still unprepare, and leftover vfio-ap state drains back to zcrypt on the next start.
Disabling every workload gate is a startup error: the driver would serve nothing.
The gate is planned to graduate GA-unlocked at 1.0.0, keeping the opt-out supported indefinitely.
Besides the project gates, the group gates `AllAlpha` and `AllBeta` (both default `false`) flip every alpha resp. beta gate at once.
They are a test and development convenience, not a supported production surface.
The `dev-alpha` overlay (`deploy/kustomize/overlays/dev-alpha`) is the committed way to run with `AllAlpha=true` during development.

## Verification

A build lists the gates it knows, with stage and default:

```bash
cex-dra-kubeletplugin --help
```

The driver logs its resolved gate map at startup:

```console
I... [STARTUP] feature gates: AllAlpha=false AllBeta=false ContainerWorkload=false VirtualMachineWorkload=true
```
