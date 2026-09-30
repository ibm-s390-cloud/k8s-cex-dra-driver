---
title: DeviceClasses and attributes
weight: 3
description: >
  The shipped DeviceClasses and the attributes published for every queue
---

## DeviceClasses

| Name                               | Binding                                                    |
| ---------------------------------- | ---------------------------------------------------------- |
| `ap-queue.virtual-machine.ibm.com` | vfio-ap mediated device for a KubeVirt VM                  |
| `ap-queue.container.ibm.com`       | zcrypt device node into a container (alpha, feature gated) |

Both select `device.driver == 'cex-driver.ibm.com'`.

## Device attributes

Core attributes, domain `cex.ibm.com`:

| Attribute      | Type   | Values                                                    |
| -------------- | ------ | --------------------------------------------------------- |
| `machineid`    | string | Machine identifier                                        |
| `apid`         | string | Adapter number, 2 lowercase hex digits                    |
| `apqi`         | string | Domain index, 4 lowercase hex digits                      |
| `type`         | string | `cca`, `ep11`, `accel`                                    |
| `generation`   | int    | Card generation (e.g. `8`)                                |
| `driver`       | string | `cex4queue`, `vfio_ap`, or empty (unbound)                |
| `card_status`  | string | `configured`, `deconfig`, `chkstop`, `offline`, `unknown` |
| `queue_status` | string | `online`, `offline`, `bound-vfio`, `unbound`, `unknown`   |
| `mkvp_state`   | string | `known`, `cached`, `unknown`. Omitted when not applicable |

`card_status` is the card-level state from the card's `config`, `chkstop`, and zcrypt `online` sysfs attributes.
`offline` means the whole card was admin-disabled (`chzcrypt -d cardXX`).
`queue_status` is the queue-level state: for a queue bound to the in-kernel zcrypt stack it reflects the zcrypt `online` attribute, otherwise the driver binding (`bound-vfio`, `unbound`).
The two axes are independent.
`unknown` means the corresponding sysfs read failed for the current scan cycle.
Toggling these states with `chzcrypt` or from the Hardware Management Console is a supported administrator operation.
[Sysfs ownership](../concepts.md#sysfs-ownership) names which attributes remain the driver's instead.
No selector is injected for either attribute yet.
Filter explicitly with CEL, for example `device.attributes["cex.ibm.com"].queue_status == "online"`.

Master key verification patterns (MKVP), Common Cryptographic Architecture (CCA) cards, domain `cex.cca.domain.ibm.com` (all strings):

| Attribute                                         | Register                           |
| ------------------------------------------------- | ---------------------------------- |
| `mkvp_aes_cur`, `mkvp_aes_old`, `mkvp_aes_new`    | AES master key: current, old, new  |
| `mkvp_apka_cur`, `mkvp_apka_old`, `mkvp_apka_new` | APKA master key: current, old, new |
| `mkvp_asym_cur`, `mkvp_asym_old`, `mkvp_asym_new` | ASYM master key: current, old, new |

Enterprise PKCS #11 (EP11) cards, domain `cex.ep11.domain.ibm.com` (all strings):

| Attribute                    | Register                   |
| ---------------------------- | -------------------------- |
| `mkvp_wk_cur`, `mkvp_wk_new` | Wrapping key: current, new |
