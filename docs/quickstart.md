---
title: Quickstart
weight: 1
description: >
  From driver install to a KubeVirt virtual machine with a CEX queue
---

This page walks the end-to-end happy path: install the driver, request a Common Cryptographic Architecture (CCA) queue with a ResourceClaimTemplate, attach it to a KubeVirt virtual machine, and verify the queue inside the guest.

## What you need

- An s390x Kubernetes cluster, version 1.34 or later.
  See [Prerequisites](installation/prerequisites.md) for the full list, including the node kernel requirement.
- KubeVirt 1.9.0 or later with the dynamic resource allocation (DRA) host-device feature gates enabled.
  See [KubeVirt integration](installation/kubevirt.md) for the KubeVirt-side setup.
- The [virtctl](https://kubevirt.io/user-guide/user_workloads/virtctl_client_tool/) client.
  The verification step uses `virtctl ssh` to log into the guest.
- A container image registry you can push to and the cluster nodes can pull from.
  No prebuilt driver image is published.
  The install starts by building and pushing your own.

## Install the driver

Build the image from the repository's `Dockerfile` and push it to your registry ([Driver installation](installation/driver.md) covers the build flags):

```bash
podman build --platform=linux/s390x \
  --build-arg VERSION="$(git describe --tags --always --dirty --long)" \
  -t cex-dra-kubeletplugin:v1.0.0-alpha.1 .
podman push cex-dra-kubeletplugin:v1.0.0-alpha.1 registry.example.com/cex-dra-kubeletplugin:v1.0.0-alpha.1
```

The `VERSION` build argument is what the driver reports in its first log line and from `--version`.
Build without it and the image reports `dev`, with no way to tell which source tree it came from.

The driver ships plain Kustomize manifests under `deploy/kustomize/`.
Copy the reference overlay and point it at your image:

```bash
cp -r deploy/kustomize/overlays/template deploy/kustomize/overlays/my-cluster
```

Edit `deploy/kustomize/overlays/my-cluster/kustomization.yaml` and set the image:

```yaml
images:
  - name: cex-dra-kubeletplugin
    newName: registry.example.com/cex-dra-kubeletplugin
    newTag: v1.0.0-alpha.1
```

Then apply:

```bash
kubectl apply -k deploy/kustomize/overlays/my-cluster
```

The overlay creates the `cex-dra-driver` namespace, the DaemonSet, its RBAC, and the `ap-queue.virtual-machine.ibm.com` DeviceClass this walkthrough uses.
Further DeviceClasses are opt-in overlay components.
See [Driver installation](installation/driver.md).
Wait for the plugin pods:

```bash
kubectl -n cex-dra-driver get pods -l app.kubernetes.io/name=cex-dra-driver
```

Within one scan interval each node publishes its Adjunct Processor (AP) queues as ResourceSlices, one per Crypto Express adapter:

```bash
kubectl get resourceslices -o wide
```

## Request a CCA queue

Create a ResourceClaimTemplate that asks for one CCA queue from the virtual-machine DeviceClass:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: default
  name: single-cca-ap-queue-vm
spec:
  spec:
    devices:
      requests:
        - name: cca-ap-queue
          exactly:
            deviceClassName: ap-queue.virtual-machine.ibm.com
            allocationMode: ExactCount
            count: 1
            selectors:
              - cel:
                  expression: device.attributes["cex.ibm.com"].type == "cca"
```

## Start a virtual machine with the queue

Reference the template from a VirtualMachineInstance: the claim is declared under `spec.resourceClaims`, and the device is mapped into the guest under `spec.domain.devices.hostDevices` by claim and request name:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  namespace: default
  name: cex-quickstart
spec:
  domain:
    devices:
      disks:
        - name: containerdisk
          disk:
            bus: virtio
        - name: cloudinit
          disk:
            bus: virtio
      hostDevices:
        - name: cex-cca
          claimName: claim0
          requestName: cca-ap-queue
      rng: {}
    resources:
      requests:
        memory: 1Gi
      limits:
        memory: 1Gi
  volumes:
    - name: containerdisk
      containerDisk:
        image: quay.io/containerdisks/fedora:latest
    - name: cloudinit
      cloudInitNoCloud:
        userData: |-
          #cloud-config
          user: fedora
          password: fedora
          chpasswd: { expire: False }
          ssh_pwauth: True
          packages:
            - s390utils-base
          # ssh_authorized_keys:
          #   - ssh-ed25519 AAAA... you@example.com
  resourceClaims:
    - name: claim0
      resourceClaimTemplateName: single-cca-ap-queue-vm
```

The containerdisk image ships no credentials.
The cloud-init volume sets the login `fedora`/`fedora` so the SSH step under Verify works with the manifest as-is.
A guest with this password is for testing only, never production: switch to key auth by uncommenting `ssh_authorized_keys`, adding your public key, and dropping the password lines.
The stock containerdisk also carries no s390 crypto tools, so the `packages` entry has cloud-init install `s390utils-base` on first boot.
That package provides the `lszcrypt` run under Verify.
The install needs the guest to reach the Fedora mirrors, and until it finishes `lszcrypt` reports `command not found`.

When the VMI schedules, the driver prepares a vfio-ap mediated device for the allocated queue and KubeVirt passes it through to the guest.

## Verify

The claim is allocated:

```bash
kubectl get resourceclaims -n default
```

The VMI is running:

```bash
kubectl get vmi -n default cex-quickstart
```

The guest sees the CCA queue:

```bash
virtctl ssh fedora@vmi/cex-quickstart -c 'lszcrypt'
```

`lszcrypt` lists one CEX device of type CCA, at the adapter-domain pair the claim was allocated (compare with the `apid` and `apqi` attributes on the ResourceSlice device).

## Next steps

- [Requesting queues](usage/usage.md): selectors, co-location constraints, multi-type claims, and claim configuration.
- [Concepts](concepts.md): the device model behind the attributes used above.
- [Architecture](architecture/index.md): what happened between `kubectl apply` and the queue showing up in the guest.
