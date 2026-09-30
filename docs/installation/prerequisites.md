---
title: Prerequisites
weight: 1
description: >
  Support matrix and node and cluster requirements for the CEX DRA driver
---

The driver runs as a DaemonSet on s390x nodes and talks to the Adjunct Processor (AP) bus through sysfs.
This page lists what each [node](https://kubernetes.io/docs/concepts/architecture/nodes/) and the cluster must provide before the install.
Each requirement comes with a command that verifies it.

## Support matrix

This matrix applies to the release documented by this version of the site.

| Dimension    | Requirement                                                                                                        |
| ------------ | ------------------------------------------------------------------------------------------------------------------ |
| Architecture | s390x (IBM Z, LinuxONE) with Crypto Express cards                                                                  |
| Kubernetes   | 1.34 or later (DRA is GA and on by default from 1.34)                                                              |
| KubeVirt     | 1.9.0 or later, with the `HostDevices` and `HostDevicesWithDRA` feature gates enabled                              |
| Node kernel  | AP-bus `driver_override` feature available (mainline 6.19 or later, backports accepted), virtual-machine path only |

Before 1.0.0 the public API surface (DeviceClass attribute schema, `CryptoConfig` schema) is allowed to change.
The sections below explain each requirement and how to verify it.

## Cluster requirements

Run the commands in this section from any machine with `kubectl` access to the cluster.

- [Kubernetes](https://kubernetes.io/) 1.34 or later.
  Dynamic resource allocation (DRA) is GA and enabled by default from 1.34.
  No feature gates or API group opt-ins are needed on the cluster.
  The server version in the output must be 1.34 or later.

  ```bash
  kubectl version
  ```

- A container runtime that resolves Container Device Interface (CDI) specs from `/var/run/cdi`.
  containerd 2.0 or later does so by default.
  containerd 1.7 needs `enable_cdi = true` under `[plugins."io.containerd.grpc.v1.cri"]` in `/etc/containerd/config.toml`.
  CRI-O does so by default, so any CRI-O release paired with a supported Kubernetes version needs no configuration.
  Without CDI resolution, a pod consuming a claim fails to start with `unresolvable CDI devices ibm.com/vfio-ap-passthrough=...`.

  A VM booting from a containerDisk carries two requirements beyond CDI resolution.
  KubeVirt 1.9.0 ships containerDisk content as an image volume, a Kubernetes feature that mounts a container image directly as a read-only volume.

  The cluster needs the Kubernetes `ImageVolume` feature gate enabled on the API server and on the kubelet.
  The gate is off by default on 1.34, on by default from 1.35, and generally available from 1.36, so 1.34 is the one supported version that needs the switch thrown.
  A 1.34 API server without it drops the image volume from the virt-launcher pod instead of rejecting the pod, the init container then fails with `stat /container-disk-binary/usr/bin/container-disk: no such file or directory`, and the VM never reaches `Ready`.
  Nothing in that message names the gate.

  The node needs containerd 2.1 or later, the first release that supports these mounts.
  On an older containerd, the virt-launcher pod fails with `failed to generate spec: failed to mkdir ""`.

  Where either requirement cannot be met, disabling the `ImageVolume` feature gate in the KubeVirt custom resource restores the sidecar mechanism of earlier releases.
  VMs booting from a DataVolume are not affected, and CRI-O again needs no configuration.

  The `CONTAINER-RUNTIME` column names each node's runtime and version.

  ```bash
  kubectl get nodes -o wide
  ```

- [KubeVirt](https://kubevirt.io/) 1.9.0 or later, configured as described in [KubeVirt integration](kubevirt.md). ([`VirtualMachineWorkload=true` (default)](../reference/feature-gates.md))

  ```bash
  kubectl get kubevirt -A -o jsonpath='{.items[0].status.observedKubeVirtVersion}'
  ```

## Node requirements

Run the commands in this section on the [node](https://kubernetes.io/docs/concepts/architecture/nodes/) itself.

Each node that should serve CEX queues needs:

- A kernel whose AP bus exposes `driver_override` on queue devices. ([`VirtualMachineWorkload=true` (default)](../reference/feature-gates.md))
  Mainline kernels have the feature from 6.19.
  The driver probes the attribute itself, so a vendor or custom kernel that backports it is accepted whatever its version string says.
  With no cards assigned there are no queues to probe and the 6.19 mainline floor serves as the reference.

  ```bash
  ls /sys/bus/ap/devices/card*/*/driver_override
  ```

- Both zcrypt masks left at all-1s, the kernel's default. ([`VirtualMachineWorkload=true` (default)](../reference/feature-gates.md))
  One clear bit in `apmask` or `aqmask` stops the driver binding any queue to vfio-ap, and preflight fails the node ([Sysfs ownership](../concepts.md#sysfs-ownership)).
  Both values must read `0x` followed by nothing but `f`.

  ```bash
  cat /sys/bus/ap/apmask /sys/bus/ap/aqmask
  ```

  A node previously prepared for manual vfio-ap passthrough has its masks cleared and needs them reset, with no claim prepared on the node:

  ```bash
  chzdev --type ap apmask=+0x00-0xff aqmask=+0x00-0xff
  ```

- The AP bus present.

  ```bash
  ls /sys/bus/ap
  ```

- The `vfio_ap` kernel module loaded. ([`VirtualMachineWorkload=true` (default)](../reference/feature-gates.md))

  ```bash
  ls /sys/devices/vfio_ap/matrix
  ```

- Mediated device support ([`VirtualMachineWorkload=true` (default)](../reference/feature-gates.md))

  ```bash
  ls /sys/class/mdev_bus
  ```

- Additional zcrypt device nodes, with the zcrypt module loaded. ([`ContainerWorkload=true`](../reference/feature-gates.md))
  The interface is part of the zcrypt driver since Linux 4.20 and has no Kconfig option of its own; the class appears with or without crypto cards assigned.
  The kernel allows at most 256 additional nodes per host, one per prepared container claim.

  ```bash
  ls /sys/class/zcrypt/create
  ```
