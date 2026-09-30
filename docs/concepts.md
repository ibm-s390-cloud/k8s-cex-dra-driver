---
title: Concepts
weight: 3
description: >
  The APQN device model, DeviceClasses, and the attribute taxonomy
---

This page defines what the driver publishes and what a claim can ask for.
How the pieces move at runtime - the scan loop, allocation, and preparation - is drawn in [Architecture](architecture/index.md).

## The APQN model

A Crypto Express adapter is the physical card.
Each adapter is partitioned into independent domains, each with its own master key registers, so the hardware resource a workload uses is a domain on an adapter.
Linux's AP bus represents each usable adapter-domain pair as an AP queue, the queue through which work is submitted to that domain on that adapter.
The AP queue number (APQN) addresses one queue.
It is the pair of:

- `apid` - the AP adapter ID: which physical card, two lowercase hex digits, for example `08`.
- `apqi` - the AP queue index: which domain within the adapter, four lowercase hex digits, for example `0029`.

One ResourceSlice device is one AP queue.
The device name is `<machineid>-<apid>-<apqi>`, so a device is traceable to its node and its exact queue.
Queues with the same `apqi` use the same domain number across cards.
Queues with the same `apid` sit on the same card.
Both axes matter for placement, which is what the `matchAttribute` constraint in [Requesting queues](usage/usage.md) is for.

## DeviceClasses

The driver defines two DeviceClasses, both selecting every device the driver publishes:

| DeviceClass                        | Intended binding                                                       |
| ---------------------------------- | ---------------------------------------------------------------------- |
| `ap-queue.virtual-machine.ibm.com` | vfio-ap mediated device, passed to a KubeVirt VM                       |
| `ap-queue.container.ibm.com`       | zcrypt device node passthrough into a container (alpha, feature gated) |

Which classes a cluster holds is an install choice, made per class through the opt-in Kustomize components in [Driver installation](installation/driver.md).
The container class installs only with the alpha `feature-container-workload` component.

Both use the same Common Expression Language (CEL) selector:

```yaml
selectors:
  - cel:
      expression: device.driver == 'cex-driver.ibm.com'
```

The class expresses _how the workload wants the queue bound_, not which kernel driver currently holds it.
The driver rebinds queues between the zcrypt stack and vfio-ap as claims demand.
The queue's current kernel binding is visible as the `cex.ibm.com/driver` attribute, and claims that care can select on it.
Intent and current state are separate: the DeviceClass is what the claim asked for, the attribute is what the queue is bound to at the moment you look.
The two states and the transitions between them are drawn under [Queue binding states](architecture/index.md#queue-binding-states).

## Attribute taxonomy

Every device carries the core attributes in the `cex.ibm.com` domain:

| Attribute                | Type   | Meaning                                                                                                                          |
| ------------------------ | ------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `cex.ibm.com/machineid`  | string | Machine identifier the queue belongs to                                                                                          |
| `cex.ibm.com/apid`       | string | Adapter number, 2 lowercase hex digits                                                                                           |
| `cex.ibm.com/apqi`       | string | Domain index, 4 lowercase hex digits                                                                                             |
| `cex.ibm.com/type`       | string | Card mode: `cca`, `ep11`, or `accel`                                                                                             |
| `cex.ibm.com/generation` | int    | Card generation, for example `8` for CEX8                                                                                        |
| `cex.ibm.com/driver`     | string | Kernel driver currently bound: `cex4queue` (zcrypt), `vfio_ap`, or empty when unbound                                            |
| `cex.ibm.com/mkvp_state` | string | Freshness of the master key verification pattern (MKVP) attributes: `known`, `cached`, or `unknown`. Omitted when not applicable |

Master key verification patterns are published in per-card-type domains, so workloads can pin a queue to a specific configured master key:

- Common Cryptographic Architecture (CCA) cards (`cex.cca.domain.ibm.com`): `mkvp_aes_cur`, `mkvp_aes_old`, `mkvp_aes_new`, `mkvp_apka_cur`, `mkvp_apka_old`, `mkvp_apka_new`, `mkvp_asym_cur`, `mkvp_asym_old`, `mkvp_asym_new`.
- Enterprise PKCS #11 (EP11) cards (`cex.ep11.domain.ibm.com`): `mkvp_wk_cur`, `mkvp_wk_new`.

The `cur`/`old`/`new` suffixes mirror the card's master key registers: the current key, the previous one kept for key changes, and a staged next key.
MKVP values are read from the card itself.
When a read is not possible (for example while a queue is bound to vfio-ap), the driver serves the last known values and says so via `mkvp_state`.

## Sysfs ownership

The driver shares the AP bus sysfs surface with the node administrator, and each attribute has exactly one writer.

On a node the driver manages, these things belong to the driver:

- `driver_override` on AP queues.
  It is the driver's rebinding mechanism and its crash-recovery marker: a queue whose override reads `vfio_ap` without a live vfio-ap mediated device behind it is reset to the zcrypt driver when the driver starts and after every claim unprepare.
  A manually written override is indistinguishable from a crash leftover and is reverted the same way, silently.
- `drivers_probe` writes for AP queues, which act on the override.
- Additional zcrypt device nodes named `cexdra-<claim UID prefix>`, created and destroyed with container claims, and the shadow AP sysfs trees under `--shadow-sysfs-root` that accompany them.
  Nodes with any other name, such as ones made with `zcryptctl`, are never touched.
- vfio-ap mediated devices.
  The reset spares mediated devices it did not create, but their queues remain published and schedulable, so a claim can be allocated onto one and the preparation rebind collides with the manual setup.
  Manual vfio-ap passthrough on driver-managed nodes is therefore unsupported.

Everything else stays with the administrator.
The driver reads it and never writes it:

- Card and queue `config` and `online`, toggled with `chzcrypt` (`-e`/`-d`, `--config-on`/`--config-off`) or from the Hardware Management Console or Support Element.
  These are the sanctioned maintenance knobs.
  Their effect is published as the `card_status` and `queue_status` attributes described in [DeviceClasses and attributes](reference/devices.md).
- The zcrypt masks `apmask` and `aqmask`.
  Restricting them removes queues from host visibility and so from the published inventory.
  The driver never writes them, but the virtual-machine path depends on them: on a node with `VirtualMachineWorkload` enabled, preflight fails when the masks are not all-1s, for the reason below.

The kernel enforces this split: the two mechanisms are mutually exclusive.
Every `apmask`/`aqmask` write returns `EINVAL` while any AP device carries a `driver_override`, and a `driver_override` write returns `EINVAL` while restricted masks are in use.
Mask changes therefore need a moment with no overrides active - no prepared claim on the node and no pending reset marker.
In the other direction the exclusion is bus-wide, not per-APQN: one clear bit in either mask makes every `driver_override` write on the node fail, including writes for queues the mask does not touch.
Restricted masks do not merely hide queues, they stop the driver from rebinding any queue at all, and they do it silently - the remaining queues are still scanned, published and allocated, so the failure surfaces only when a claim is prepared.
That is what preflight refuses to start on.

An offline or deconfigured queue stays published and remains allocatable: no selector is injected for the status attributes, so claims that must avoid such queues filter on them explicitly (the device reference shows the CEL expression).
A queue bound to an AP driver the scanner does not know is published with `queue_status: unknown`.
Running other AP drivers next to this one has no defined semantics.

## Verification

Inspect what a node actually publishes:

```bash
kubectl get resourceslices -o yaml | less
```

Each device shows the full attribute map.
Cross-check a queue against the node's own view with `lszcrypt -V` on the node.
