#!/usr/bin/env bash
# Copyright contributors to the k8s-cex-dra-driver project
# SPDX-License-Identifier: Apache-2.0
#
# driver.sh - CEX DRA driver image build/push/deploy helpers.
# shellcheck disable=SC2154
# Sourced by cex-dra.sh; expects REPO_DIR, WORK_DIR, IMAGE, IMAGE_NAME,
# IMAGE_TAG, OVERLAY, and kubectl/podman to be available in the calling
# environment.

build_image() {
  [[ -n "${IMAGE}" ]] || fail "registry address not set (install_registry first)"
  set_registry_image_vars
  cd "${REPO_DIR}" || exit
  local version
  version="$(git describe --tags --always --dirty --long)"
  mkdir -p "${WORK_DIR}"
  info "CI build ${IMAGE} from ${version}"
  podman build --platform=linux/s390x \
    --build-arg VERSION="${version}" \
    --no-cache \
    -t "${IMAGE}" \
    .
}

# Retry helper: push a single image ref up to 3 times.
push_with_retry() {
  local push_target="$1"
  for attempt in 1 2 3; do
    if podman push --tls-verify=false "${push_target}"; then
      return 0
    fi
    [[ "$attempt" == "3" ]] && fail "podman push failed after 3 attempts"
    echo "  push attempt ${attempt} failed; retrying in 5s"
    sleep 5
  done
}

push_image() {
  [[ -n "${IMAGE}" ]] || fail "registry address not set (install_registry first)"
  local local_port="${REGISTRY_LOCAL_PORT:-5000}"
  local push_target="127.0.0.1:${local_port}/${PLUGIN_REPO}:${IMAGE_TAG}"
  podman tag "${IMAGE}" "${push_target}"
  echo "Pushing ${push_target} via port-forward (HTTP registry, tls-verify=false)"
  push_with_retry "${push_target}"
  image_in_registry || fail "push finished but ${IMAGE} is not in the registry catalog"
}

# Prepare the kustomize overlay directory with the target image name and tag.
prepare_overlay() {
  local od template
  od="$(overlay_dir)"
  template="${REPO_DIR}/deploy/kustomize/overlays/template"
  [[ -d "$template" ]] || fail "missing ${template} after git update"
  if [[ "$od" != "$template" ]]; then
    rm -rf "$od"
    cp -a "$template" "$od"
  fi
  sed -i "0,/newName:/{s|\(newName:[[:space:]]*\).*|\1${IMAGE_NAME}|}" "$od/kustomization.yaml"
  sed -i "0,/newTag:/{s|\(newTag:[[:space:]]*\).*|\1${IMAGE_TAG}|}" "$od/kustomization.yaml"
  echo "$od/kustomization.yaml -> ${IMAGE_NAME} ${IMAGE_TAG}"
}

# Apply the kustomize overlay and force imagePullPolicy=IfNotPresent on the daemonset.
apply_driver_manifests() {
  local od
  od="$(overlay_dir)"
  kubectl apply -k "$od"
  kubectl -n cex-dra-driver patch daemonset cex-dra-driver --type json -p \
    '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
    >/dev/null 2>&1 ||
    kubectl -n cex-dra-driver patch daemonset cex-dra-driver --type json -p \
      '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
      >/dev/null 2>&1 || true
}

# Poll until at least one ResourceSlice device is published by the driver.
wait_for_resourceslice() {
  echo "Waiting for ResourceSlice..."
  for _ in $(seq 1 30); do
    if kubectl get resourceslices -o jsonpath='{.items[*].spec.devices[*].name}' 2>/dev/null | grep -q .; then
      kubectl get resourceslices -o wide
      return
    fi
    sleep 5
  done
  kubectl -n cex-dra-driver logs -l app.kubernetes.io/name=cex-dra-driver --tail=80 || true
  fail "no ResourceSlice devices published"
}

# Wait for the driver daemonset to roll out and for at least one ResourceSlice device.
wait_for_driver() {
  kubectl -n cex-dra-driver rollout status daemonset/cex-dra-driver --timeout=180s
  kubectl -n cex-dra-driver get pods -o wide
  kubectl -n cex-dra-driver logs -l app.kubernetes.io/name=cex-dra-driver --tail=30 || true
  wait_for_resourceslice
}

deploy_driver() {
  info "Deploy CEX DRA driver"
  assert_driver_gone
  [[ -n "${IMAGE_NAME}" ]] || fail "registry address not set (install_registry first)"
  cd "${REPO_DIR}" || exit
  prepare_overlay
  apply_driver_manifests
  wait_for_driver
}
