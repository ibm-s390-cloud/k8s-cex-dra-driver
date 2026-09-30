# Kubernetes CEX DRA Driver (Technical Preview)

This driver enables Kubernetes workloads to use IBM Crypto Express (CEX) cards on IBM Z and LinuxONE systems through Kubernetes Dynamic Resource Allocation (DRA).
It exposes CEX cards as allocatable Kubernetes resources.

The current release supports [KubeVirt](https://kubevirt.io/) virtual machine workloads.
Containerized workloads are available as an alpha feature behind the `ContainerWorkload` feature gate.

The [k8s-cex-dev-plugin](https://github.com/ibm-s390-cloud/k8s-cex-dev-plugin) project also gives Kubernetes workloads access to CEX cards.
It is separate from this driver and does not use DRA.

## Documentation

Start with the [documentation](./docs/_index.md) or jump straight to the [quickstart](./docs/quickstart.md).

## Status

The current release is `v1.0.0-alpha.0`.
It is the first public Technical Preview.
Public settings and device properties may change before version `v1.0.0`.

[`CHANGELOG.md`](./CHANGELOG.md) lists changes between releases.

This release contains source code only.
To build a container image follow [Build and push the image](./docs/installation/driver.md#build-and-push-the-image).

## Standards

- Releases follow [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).
- [`CHANGELOG.md`](./CHANGELOG.md) follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/).
- Commit messages follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/), with the full format in [`.gitmessage`](./.gitmessage).
- Documentation keeps [one sentence on each source line](https://asciidoctor.org/docs/asciidoc-recommended-practices/#one-sentence-per-line).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) to build the project, run checks, and submit a change.

## License

This project is licensed under the Apache License, Version 2.0.
See [LICENSE](./LICENSE) for the full license text.

## Authors

- Bodo Brand <bodo.brand1@ibm.com>
- Harald Freudenberger <freude@linux.ibm.com>
