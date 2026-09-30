---
title: Requesting queues
weight: 1
description: >
  ResourceClaims, CEL selectors, constraints, and claim configuration
---

## Claims and templates

Workloads request queues with the standard dynamic resource allocation (DRA) objects:

- A **ResourceClaimTemplate** stamps out a fresh claim per pod or VMI.
  This is the common case: each VM instance gets its own queue allocation.
- A **ResourceClaim** is a standalone object with its own lifecycle, shared by every pod that references it.

All examples below use templates.
The `spec.spec.devices` block is the same in both.

## Selecting devices

Requests draw from a DeviceClass (see [Concepts](../concepts.md)) and narrow the candidate set with Common Expression Language (CEL) selectors over the published attributes.
Select by card mode:

```yaml
selectors:
  - cel:
      expression: device.attributes["cex.ibm.com"].type == "cca"
```

Note: The type can be cca, ep11 or accel.

By card generation:

```yaml
selectors:
  - cel:
      expression: device.attributes["cex.ibm.com"].generation >= 8
```

By configured master key, pinning the claim to queues whose current AES master key matches a known verification pattern:

```yaml
selectors:
  - cel:
      expression: >-
        device.attributes["cex.ibm.com"].type == "cca" &&
        device.attributes["cex.cca.domain.ibm.com"].mkvp_aes_cur == "7f902215156ccaec"
```

## Co-locating queues in one domain

When a claim asks for several queues, a `matchAttribute` constraint forces them to agree on an attribute.
Requiring the same `apqi` places all queues in the same crypto domain, whichever domain has capacity - no hardcoded domain number:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: default
  name: dual-cca-ap-queue-vm
spec:
  spec:
    devices:
      requests:
        - name: cca-ap-queues
          exactly:
            deviceClassName: ap-queue.virtual-machine.ibm.com
            allocationMode: ExactCount
            count: 2
            selectors:
              - cel:
                  expression: device.attributes["cex.ibm.com"].type == "cca"
      constraints:
        - requests: ["cca-ap-queues"]
          matchAttribute: cex.ibm.com/apqi
```

## Multi-type claims

One claim can carry several requests with different selectors.
A constraint spanning all of them still forces a single shared domain:

```yaml
requests:
  - name: cca-ap-queues
    exactly:
      deviceClassName: ap-queue.virtual-machine.ibm.com
      allocationMode: ExactCount
      count: 2
      selectors:
        - cel:
            expression: device.attributes["cex.ibm.com"].type == "cca"
  - name: ep11-ap-queues
    exactly:
      deviceClassName: ap-queue.virtual-machine.ibm.com
      allocationMode: ExactCount
      count: 2
      selectors:
        - cel:
            expression: device.attributes["cex.ibm.com"].type == "ep11"
constraints:
  - requests: ["cca-ap-queues", "ep11-ap-queues"]
    matchAttribute: cex.ibm.com/apqi
```

For virtual machines, note how this meets KubeVirt: the driver prepares **one vfio-ap mediated device per claim**, whose matrix covers every allocated queue across all requests.
The VMI therefore lists **one `hostDevices` entry per claim**, with `requestName` naming any one of the claim's requests: that single mediated device already carries every allocated queue, so the guest sees all of them whichever request the entry names.
One entry per request fails, because libvirt accepts only one vfio-ap host device per guest (see [KubeVirt integration](../installation/kubevirt.md)).

## Claim configuration

The driver accepts an opaque configuration block on a claim, the `CryptoConfig`:

```yaml
spec:
  spec:
    devices:
      requests:
        - name: cca-queues
          exactly:
            deviceClassName: ap-queue.virtual-machine.ibm.com
            allocationMode: ExactCount
            count: 2
      config:
        - opaque:
            driver: cex-driver.ibm.com
            parameters:
              kind: CryptoConfig
              apiVersion: cex.ibm.com/v1alpha1
              controlDomainMode: "usage-and-control"
```

`controlDomainMode` decides whether the guest gets control access - the key-management side of a crypto domain, as opposed to the encrypt/decrypt usage side - to the domains of its allocated queues:

- `usage-and-control` (default): control access to exactly the domains the claim was allocated.
  What a CCA key-management VM needs.
- `usage-only`: usage access only, no control access.

Both values are bounded by the claim's own allocation.
Neither can reach a domain the claim was not given.
Two further modes (`all-control-domains` and a custom 256-bit mask) are defined but not enabled.
See the [claim configuration reference](../reference/claim-configuration.md) for details.

The configuration is strict: unknown fields, a wrong `kind`, or a wrong `apiVersion` are rejected.
Scope is the whole claim - one effective value, merged from the built-in default, then DeviceClass configuration, then claim configuration.
For container claims the enabled modes set the zcrypt node's control-domain mask, and a mode that is not enabled grants no control domain rather than failing the claim, so the same claim text works for either binding type.

The kernel has the last word: assigned control domains are intersected with the host's `ap_control_domain_mask`, so a domain the LPAR does not control never becomes effective in the guest.

## Containers

With the alpha `ContainerWorkload` gate and the `feature-container-workload` component installed, a pod requests queues from the container DeviceClass:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: default
  name: single-ep11-ap-queue-container
spec:
  spec:
    devices:
      requests:
        - name: ep11-ap-queue
          exactly:
            deviceClassName: ap-queue.container.ibm.com
            allocationMode: ExactCount
            count: 1
            selectors:
              - cel:
                  expression: device.attributes["cex.ibm.com"].type == "ep11"
---
apiVersion: v1
kind: Pod
metadata:
  namespace: default
  name: cex-container
spec:
  resourceClaims:
    - name: cex
      resourceClaimTemplateName: single-ep11-ap-queue-container
  containers:
    - name: app
      image: registry.example.com/my-crypto-app
      resources:
        claims:
          - name: cex
```

For each claim the driver creates a zcrypt device node whose adapter, usage-domain and control-domain masks grant exactly the allocated queues, and hands it to the container as `/dev/z90crypt`, the path libica, openCryptoki and the s390-tools open by default.
The node is world read-writable inside the container, so workloads need not run as root: the masks, not the file mode, bound what it reaches.
The container also receives `CEX_APQNS`, the comma-separated list of its queues (`06.0000`).
The queues stay bound to the zcrypt driver throughout, so this path needs neither `vfio_ap` nor AP-bus `driver_override`.
A queue that is not bound to zcrypt when the claim is prepared, for example one still on `vfio_ap`, fails the prepare with an error naming it.

Several queues in one claim follow the same rule as for virtual machines: the node's masks are a cross-product of adapters and domains, so constrain the queues to one domain with `matchAttribute` as in [Co-locating queues in one domain](#co-locating-queues-in-one-domain).

The container's AP sysfs is narrowed to the claim as well.
The driver builds a small copy of `/sys/bus/ap` and `/sys/devices/ap` holding only the allocated cards and queues and mounts it read-only over both paths, so `lszcrypt` inside the container lists exactly the claim's queues, and software that enumerates the bus before opening `/dev/z90crypt` picks one it can reach. For a claim holding queue 06.0000 of a CEX6P in EP11 mode:

```text
$ lszcrypt
CARD.DOM TYPE  MODE        STATUS  REQUESTS
-----------------------------------------------
06       CEX6P EP11-Coproc online         0
06.0000  CEX6P EP11-Coproc online         0
```

The copy is taken when the claim is prepared.
Static attributes such as the card type, firmware level and master key verification patterns (`mkvps`) are copied from the host, the masks are generated from the allocation, and state and counter attributes read as online and idle.
It does not follow later changes on the host: a card that goes offline still reads online in the container, and the request counters stay at 0.

## Verification

Watch a claim through its lifecycle:

```bash
kubectl get resourceclaims -n <namespace> -o wide
```

An allocated claim names its devices.
Each device name encodes machine, adapter, and domain (`<machineid>-<apid>-<apqi>`).
Inside a VM guest, `lszcrypt` shows the passed-through queues at those adapter-domain positions, and control-domain access is visible in `/sys/bus/ap/ap_control_domain_mask` read from the guest.
