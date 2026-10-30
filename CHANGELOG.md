# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

Features graduate on their own, independently of the release that ships them and a gate flip is where behavior changes for an existing user.
Every release entry therefore also records the feature gates it changed: the gate name, its stage, and whether it defaults on.

## 1.0.0-alpha.1

A patch of 1.0.0-alpha.0 with the dependency fixes the scanners flagged and the fixes found since that cut.
No feature gate changed.

### Added

- `contrib/kubevirt-ci/`, the deploy and remove script KubeVirt's CI lane runs against this tree. It is owned by the IBM OCP Virtualization team and is not part of what the driver delivers.

### Fixed

- A `--scan-interval` (`SCAN_INTERVAL`) below `1s` is refused at startup with an error naming the flag. A zero value used to panic in the liveness watchdog, and a tiny one restarted a healthy driver in a loop.
- The card status tie-break matches `lszcrypt`: a card that is both deconfigured and in checkstop publishes `deconfig`, not `chkstop`.
- The image build runs its builder stage on the build platform, so `podman build --platform=linux/s390x` on another architecture no longer emulates the Go toolchain.

### Security

- golang.org/x/net moves to v0.62.0, which clears five HTTP/2 advisories in the module the driver pulls in through client-go and grpc: CVE-2026-78663, CVE-2026-97032, CVE-2026-78669, CVE-2026-78660, and CVE-2026-78659. The driver runs no HTTP/2 server from that module, and its client side talks to the API server only. The same move clears CVE-2026-97045 and CVE-2026-78661 in the module's HTML parser, which the driver does not build.
- go.etcd.io/etcd/client/pkg/v3 moves to v3.7.1, which clears CVE-2026-73500 in a module the driver requires but does not call.
- The build pins Go 1.26.10 for the crypto/x509 fix for CVE-2026-111943.
- github.com/urfave/cli/v3 moves to v3.11.0. Its vendored documentation requirements pin mkdocs-material 9.7.7, which clears CVE-2026-73295 that scanners report against the source tree. Nothing here builds that documentation, and no built artifact carries mkdocs-material.

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
