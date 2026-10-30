# KubeVirt CI deploy

This code is owned by the IBM OCP Virtualization team.
`cex-dra.sh` deploys the CEX DRA driver onto a shared cluster for CI.
Run it directly. There is no `make` target for it.
It removes a leftover driver and in-cluster registry, builds the plugin image, pushes it through a local port-forward, and copies it into the first node's CRI-O store.
It then applies the Kustomize overlay and waits until that node publishes a ResourceSlice.
It does not install or remove KubeVirt.
`undeploy` removes that driver, the in-cluster registry, and the test namespace.
It leaves KubeVirt in place.

## Prerequisites

The machine that runs the script needs `kubectl`, `git`, `podman`, `curl`, and `envsubst`.
The cluster needs one s390x node running CRI-O, with permission to create a privileged Pod and a NodePort.
The script uses the first node only: it reads that node's InternalIP and imports the image there.
That node needs a Crypto Express queue.
The script waits for a ResourceSlice device and fails when the slice has none.
Port `30511` must be free for the registry NodePort, and port `5000` must be free locally for the port-forward.

```bash
./contrib/kubevirt-ci/cex-dra.sh deploy
./contrib/kubevirt-ci/cex-dra.sh undeploy
```
