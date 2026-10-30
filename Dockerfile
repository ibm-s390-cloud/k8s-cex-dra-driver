# The builder runs on the build host's own platform. The Go toolchain
# cross-compiles for s390x either way, so emulating the builder under
# --platform=linux/s390x would only slow the build down. The --platform
# flag still selects the runtime base below.
FROM --platform=$BUILDPLATFORM quay.io/projectquay/golang:1.26 AS builder
# The build context carries no .git (see .dockerignore), so the build
# identity comes in from outside: podman build --build-arg VERSION="$(git
# describe --tags --always --dirty --long)". Left at the default, the binary
# reports "dev".
ARG VERSION=dev
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=s390x make VERSION=$VERSION

# Pinned by digest for reproducible image builds. The tag is documentation.
# To bump: resolve the current digest of the wanted tag, e.g.
#   skopeo inspect --raw docker://registry.access.redhat.com/ubi10-minimal:latest
FROM registry.access.redhat.com/ubi10-minimal:10.2-1786960640@sha256:61f820b7893b6226e499e928db99c59a0a9135aa17e4e056fdaf1015908cca14
COPY --from=builder /app/cex-dra-kubeletplugin /cex-dra-kubeletplugin
ENTRYPOINT ["/cex-dra-kubeletplugin"]
