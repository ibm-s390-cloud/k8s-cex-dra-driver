---
title: Driver installation
weight: 2
description: >
  Build the container image and deploy the driver with Kustomize
---

This page covers building the container image and installing the driver with Kustomize.
Make sure the nodes and the cluster meet the [prerequisites](prerequisites.md) first.

## Build and push the image

The repository's `Dockerfile` builds the kubelet plugin image:

```bash
podman build --platform=linux/s390x \
  --build-arg VERSION="$(git describe --tags --always --dirty --long)" \
  -t cex-dra-kubeletplugin:v1.0.0-alpha.0 .
podman push cex-dra-kubeletplugin:v1.0.0-alpha.0 registry.example.com/cex-dra-kubeletplugin:v1.0.0-alpha.0
```

The build stage always cross-compiles the binary for s390x, but the runtime base image is pulled for the build host's architecture by default.
`--platform=linux/s390x` keeps the two consistent.
Without it, a build on a non-s390x machine produces an image that will not run on the cluster nodes.
On an s390x host the flag is a no-op.

The `VERSION` build argument carries the build identity the binary reports, from `--version` and as its first log line.
`.dockerignore` keeps `.git` out of the build context, so the value has to come in from outside; left out entirely, the image reports `dev` and a running driver cannot be traced back to a source tree.
`--long` keeps the commit id in the string on a commit that is exactly a release tag (`v1.0.0-alpha.0-0-g1a2b3c4` instead of `v1.0.0-alpha.0`), so the identity still names the commit if the tag is later moved.
Building from an unpacked source archive rather than a clone leaves `git describe` with nothing to report: pass the release tag and the commit it was cut from by hand, in the same shape.

Any registry the cluster nodes can pull from works.
The manifests never hardcode a registry.
The overlay's `images:` transformer supplies it.

## Install with Kustomize

The manifests live under `deploy/kustomize/`:

```text
base/                       DaemonSet (hardened, unprivileged), SA, ClusterRole(Binding)
components/
  deviceclass-vm/           DeviceClass ap-queue.virtual-machine.ibm.com (opt-in)
  feature-container-workload/  container DeviceClass + ContainerWorkload gate + zcrypt mounts (opt-in, alpha)
  skip-preflight/           sets CEX_DRA_SKIP_PREFLIGHT=1                 (opt-in)
  vfio-ap-mounts/           VM-path host mounts: /sys/devices/vfio_ap, /var/run/cdi (opt-in)
  selinux-spc/              seLinuxOptions.type: spc_t for enforcing SELinux (opt-in)
overlays/
  template/                 reference overlay - copy and edit
  dev-unpriv/               local dev, hardened context, localhost:dev image
  dev-priv/                 dev-unpriv + privileged, for kubectl-exec debugging
  dev-alpha/                dev-unpriv + every alpha feature on (dev/test only)
```

There is no Helm chart and no templating engine: `kubectl apply -k` renders the manifests natively.

The `dev-*` overlays are for driver development and use an image imported into the node's containerd instead of a registry.
`dev-alpha` additionally enables every alpha gate and is the committed way to run with `AllAlpha=true`.

The `template` overlay is illustrative, not a production default.
Copy it, then edit your copy:

```bash
cp -r deploy/kustomize/overlays/template deploy/kustomize/overlays/my-cluster
```

1. **Set the image.**
   The base carries a non-pullable placeholder, so the overlay must point at your registry:

   ```yaml
   images:
     - name: cex-dra-kubeletplugin
       newName: registry.example.com/cex-dra-kubeletplugin
       newTag: v1.0.0-alpha.0
   ```

   Pin a release tag, not `latest`: the base sets `imagePullPolicy: IfNotPresent`, so a moving tag is never re-pulled once a node holds any image by that name - the node silently keeps running the stale build.
   For an immutable reference, pin the digest instead of a tag (`podman push` prints it):

   ```yaml
   images:
     - name: cex-dra-kubeletplugin
       newName: registry.example.com/cex-dra-kubeletplugin
       digest: sha256:4f8c...
   ```

1. **Choose components.**
   DeviceClasses are cluster-scoped and opt-in: list a component to include it, omit it to leave it out (for example when DeviceClasses are managed elsewhere):

   ```yaml
   components:
     - ../../components/deviceclass-vm
     - ../../components/vfio-ap-mounts
     - ../../components/selinux-spc
   ```

   - `feature-container-workload` opts into the alpha container workload path: it ships the container DeviceClass together with the `ContainerWorkload` feature gate the driver requires before preparing a claim against it.
     It also carries the host mounts the path writes through: `/sys/class/zcrypt` to create and destroy the per-claim zcrypt nodes, `/sys/devices/virtual` for their masks, `/var/run/cdi` for their CDI specs, and `/var/run/cex-dra/shadowsysfs` for the per-claim shadow AP sysfs trees.
     The shadow sysfs root is mounted at its host path because the runtime resolves the CDI mounts pointing into it on the host; a different root set with `--shadow-sysfs-root` needs the same change to the mount.
     On SELinux nodes the driver labels each tree `container_file_t`, which requires the `selinux-spc` component (or another context allowed to relabel), so pods can read the mounts.
     The masks live under `/sys/devices/virtual/zcrypt`, which the kernel creates only with the first additional node, so its always-present parent is mounted instead.
     `/sys/class/zcrypt` is `type: Directory`, so the pod starts only where the zcrypt module is loaded.
   - `vfio-ap-mounts` carries the two host mounts the VM path cannot work without: the writable `/sys/devices/vfio_ap` subtree the unprivileged DaemonSet needs to manage vfio-ap mediated devices, and the host's `/var/run/cdi` directory the driver writes prepared-claim CDI specs into.
     The runtime reads CDI specs from the host's `/var/run/cdi`.
     Without the mount, a spec lands only inside the container and the consuming pod fails with `unresolvable CDI devices ibm.com/vfio-ap-passthrough=...`.
     The sysfs mount also gates pod start on the `vfio_ap` module being loaded.
     The CDI mount does not (the directory is created if absent).
   - `selinux-spc` runs the plugin SELinux-unconfined (`seLinuxOptions.type: spc_t`) so its AP sysfs writes are not denied on enforcing SELinux nodes (the default on Fedora, RHEL, and CentOS).
     `spc_t` is stock `container-selinux`.
     Capabilities stay dropped and seccomp stays on.
     Harmless on non-SELinux nodes.
     A non-default `seLinuxOptions.type` sits above the baseline Pod Security Standard.
     The shipped namespace already accounts for it (see [Pod Security](#pod-security)).
   - `skip-preflight` turns preflight errors into warnings (`CEX_DRA_SKIP_PREFLIGHT=1`).
     For dev and demo clusters only.
     Never enable it in production.

1. **Adjust driver options** if needed.
   The DaemonSet sets `NODE_NAME` from the node it lands on, `SCAN_INTERVAL` to 30 seconds, and `HEALTHCHECK_PORT` to the port its `livenessProbe` dials (see [Liveness](../reference/driver-options.md#liveness) before changing either).
   Everything else runs at its default.
   Options are set as environment variables, never as container args, because `env` entries merge by name under a strategic-merge patch while an `args` patch replaces the whole list.
   The full flag and environment-variable surface is in [Driver options](../reference/driver-options.md).
   The template overlay shows a patch changing the scan interval:

   ```yaml
   patches:
     - target:
         kind: DaemonSet
         name: cex-dra-driver
       patch: |-
         apiVersion: apps/v1
         kind: DaemonSet
         metadata:
           name: cex-dra-driver
         spec:
           template:
             spec:
               containers:
                 - name: plugin
                   env:
                     - name: SCAN_INTERVAL
                       value: 60s
   ```

1. **Scope the DaemonSet to virtual-machine nodes** if not every s390x node should run the driver.
   The virtual-machine [node requirements](prerequisites.md#node-requirements), the `vfio_ap` module and mediated device support, are checked wherever the DaemonSet lands with the VM path at its default-enabled state.
   On a cluster where only some nodes are meant for crypto passthrough, restrict the driver to those nodes instead of letting it fail preflight on the rest.
   (A deployment that should run everywhere but serve VMs nowhere sets `VirtualMachineWorkload=false` instead of a nodeSelector.)
   Add a label to the nodeSelector (the base's `kubernetes.io/arch: s390x` entry stays in effect, the patch merges into it):

   ```yaml
   patches:
     - target:
         kind: DaemonSet
         name: cex-dra-driver
       patch: |-
         apiVersion: apps/v1
         kind: DaemonSet
         metadata:
           name: cex-dra-driver
         spec:
           template:
             spec:
               nodeSelector:
                 cex.ibm.com/workload-profile: vm
   ```

   Then label the intended nodes:

   ```bash
   kubectl label node <node> cex.ibm.com/workload-profile=vm
   ```

   Unlabeled nodes never run the driver: no preflight failures there, and no ResourceSlices either, so their queues are invisible to claims.
   The node requirements then apply only to the labeled nodes.
   Removing the label from a node while claims are still prepared on it strands them the same way deleting the driver would (see [Uninstall](#uninstall)).
   Drain the node's claim-consuming workloads first.

1. **Tolerate your own node taints** if the crypto nodes carry any.
   The base sets no tolerations, so a node tainted to reserve it for crypto workloads never runs the driver.
   The only trace is the DaemonSet's `DESIRED` count, which excludes the node.
   The DaemonSet controller tolerates the [node-condition taints](https://kubernetes.io/docs/concepts/workloads/controllers/daemonset/#taints-and-tolerations) on its own, so only the taints you or your cluster installer set need this.
   Crypto cards on a control-plane node are the case that is easy to miss.
   A kubeadm cluster taints such a node `node-role.kubernetes.io/control-plane:NoSchedule`, and that taint keeps the driver off the node like any other.
   Either untaint the node or tolerate that key here.
   Claim-consuming workloads need the same decision, because virt-launcher pods do not tolerate the taint either, and a claim that only control-plane queues can satisfy stays pending until one of the two is done.

   ```yaml
   patches:
     - target:
         kind: DaemonSet
         name: cex-dra-driver
       patch: |-
         apiVersion: apps/v1
         kind: DaemonSet
         metadata:
           name: cex-dra-driver
         spec:
           template:
             spec:
               tolerations:
                 - key: cex.ibm.com/dedicated
                   operator: Exists
                   effect: NoSchedule
   ```

1. **Render and apply:**

   ```bash
   kubectl kustomize deploy/kustomize/overlays/my-cluster   # inspect
   kubectl apply -k deploy/kustomize/overlays/my-cluster
   ```

The overlay creates the `cex-dra-driver` namespace and installs the ServiceAccount, ClusterRole, ClusterRoleBinding, and DaemonSet into it.
The DaemonSet targets s390x nodes only (`nodeSelector: kubernetes.io/arch: s390x`), runs unprivileged with all capabilities dropped and a read-only root filesystem, and is marked `system-node-critical`.

## Pod Security

The DaemonSet exceeds the `baseline` [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/): it mounts hostPath volumes (AP sysfs, the kubelet plugin directories), runs as UID 0 (above `restricted`), and with `selinux-spc` sets a non-default `seLinuxOptions.type`.
The namespace must therefore enforce the `privileged` Pod Security level.
The shipped namespace manifest carries the labels:

```yaml
labels:
  pod-security.kubernetes.io/enforce: privileged
  pod-security.kubernetes.io/audit: privileged
  pod-security.kubernetes.io/warn: privileged
```

Namespace labels take precedence over any cluster-wide default the PodSecurity admission configuration sets, so the install works unchanged on clusters that default to `baseline` or `restricted`.
`privileged` here is the admission tier of the namespace, not the container: the pod itself stays unprivileged, with all capabilities dropped, seccomp on, and a read-only root filesystem.

On OpenShift, SecurityContextConstraints are the enforcing layer, not the namespace labels.
The `cex-dra-driver` ServiceAccount needs an SCC that allows hostPath volumes, UID 0, and (with `selinux-spc`) a custom SELinux type.
The stock `privileged` SCC covers all three:

```bash
oc adm policy add-scc-to-user privileged -z cex-dra-driver -n cex-dra-driver
```

Without the `selinux-spc` component, the narrower stock `hostmount-anyuid` SCC also fits.

## Verify

Plugin pods are running on every s390x node:

```bash
kubectl -n cex-dra-driver get pods -l app.kubernetes.io/name=cex-dra-driver
```

The driver log shows the preflight results, the registration, and the first scan:

```bash
kubectl -n cex-dra-driver logs -l app.kubernetes.io/name=cex-dra-driver
```

Each node publishes its AP queues as ResourceSlice devices:

```bash
kubectl get resourceslices -o wide
```

An empty list usually means the node has no visible Crypto Express queues (check `lszcrypt` on the node) or preflight failed (check the pod log).

## Uninstall

Removal order matters: unprepare needs the driver running.
Deleting the driver while claims are still prepared strands the vfio-ap mediated devices and the queues' driver bindings on the nodes.
Workloads and claims go first.

1. **Delete the workloads and claims.**
   Delete every virtual machine and pod that consumes a claim allocated by the driver, then any remaining ResourceClaims and ResourceClaimTemplates:

   ```bash
   kubectl get resourceclaims -A
   ```

   As each claim unprepares, its queues return to the default zcrypt driver - visible as `cex.ibm.com/driver: "cex4queue"` on the ResourceSlice device, or with `lszcrypt` on the node.

1. **Delete the manifests** with the same overlay you applied:

   ```bash
   kubectl delete -k deploy/kustomize/overlays/my-cluster
   ```

   This covers everything the overlay rendered, including the cluster-scoped objects: the Namespace, ClusterRole, ClusterRoleBinding, and whichever DeviceClasses the overlay's components shipped.
   Note that `kubectl apply -k` never prunes, so any object a previous manifest version created and a newer one dropped is not in the render.
   Check the release notes for removed objects and delete them explicitly.

1. **Verify nothing is left.**

   ```bash
   kubectl get namespace cex-dra-driver   # NotFound
   kubectl get deviceclasses              # no ap-queue.* classes
   kubectl get resourceslices             # no slices from the driver
   ```

   The driver's ResourceSlices can outlive the delete by about a minute.
   The kubelet removes them once the plugin deregisters, so re-run the check instead of deleting them by hand.

   On each node, no mediated device remains and the queues are back on the default driver:

   ```bash
   ls /sys/devices/vfio_ap/matrix/        # no mdev UUIDs
   lszcrypt                               # queues online, driver cex4queue
   ```

### If the driver was deleted first

The mediated devices and vfio-ap queue bindings survive on the nodes with nothing left to unprepare them.
The driver's startup recovery deliberately leaves queues alone while a mediated device holds them, because a running guest may be using the queues.
Reinstall the driver (`kubectl apply -k` with the same overlay), delete the remaining workloads and claims so unprepare runs, then uninstall in order.

As a last resort, clean up a node manually once no guest uses the queues: remove each mediated device, then unbind each of its queues, clear the override, and reprobe:

```bash
echo 1 > /sys/devices/vfio_ap/matrix/<uuid>/remove
echo <ap>.<domain> > /sys/bus/ap/devices/<ap>.<domain>/driver/unbind
printf '\n' > /sys/bus/ap/devices/<ap>.<domain>/driver_override
echo <ap>.<domain> > /sys/bus/ap/drivers_probe
```
