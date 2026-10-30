#!/usr/bin/env bash
# Copyright contributors to the k8s-cex-dra-driver project
# SPDX-License-Identifier: Apache-2.0
#
# Uninstall helpers: tear down the in-cluster registry and CEX DRA driver.
# shellcheck disable=SC2154
# Does not uninstall KubeVirt.

uninstall_registry() {
  step "Removing in-cluster registry (if present)"
  stop_registry_port_forward
  kdel ns "${REGISTRY_NS}"
  require_ns_gone "${REGISTRY_NS}"
  assert_registry_gone
}

# ---------------------------------------------------------------------------
# Remove leftover driver
# ---------------------------------------------------------------------------

remove_vfio_ap_mdevs() {
  [[ -d /sys/devices/vfio_ap/matrix ]] || return 0
  local d base
  for d in /sys/devices/vfio_ap/matrix/*; do
    [[ -d "$d" ]] || continue
    base="$(basename "$d")"
    [[ "$base" =~ ^[0-9a-fA-F-]{36}$ ]] || continue
    if [[ -w "$d/remove" ]]; then
      echo 1 | sudo tee "$d/remove" >/dev/null || true
    fi
  done
}

delete_cex_resourceslices() {
  crd_exists resourceslices.resource.k8s.io || return 0
  kdel resourceslice -l app.kubernetes.io/name=cex-dra-driver
  while IFS=' ' read -r name driver rest; do
    [[ -n "$name" ]] || continue
    if echo "${name} ${driver} ${rest}" | grep -qiE 'cex|ap-queue|vfio_ap'; then
      echo "deleting ResourceSlice ${name} driver= ${driver}"
      kubectl delete resourceslice "$name" --ignore-not-found=true --wait=true --timeout="${DELETE_WAIT}s" || true
    fi
  done < <(kubectl get resourceslice -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.driver}{" "}{.spec}{"\n"}{end}' 2>/dev/null || true)
}

delete_cex_cluster_resources() {
  kdel clusterrole cex-dra-driver
  kdel clusterrolebinding cex-dra-driver
  kdel deviceclass ap-queue.virtual-machine.ibm.com
  kdel deviceclass -l app.kubernetes.io/name=cex-dra-driver
  delete_cex_resourceslices
  local leftover
  leftover="$(cex_cluster_leftovers)"
  if [[ -n "$leftover" ]]; then
    echo "Deleting remaining CEX DRA cluster objects:"
    echo "$leftover"
    # shellcheck disable=SC2086
    kdel $leftover
  fi
}

uninstall_driver() {
  step "Removing CEX DRA driver (if present)"
  local od
  od="$(overlay_dir)"
  if [[ -d "$od" ]]; then
    kubectl delete -k "$od" --ignore-not-found=true --wait=true --timeout="${DELETE_WAIT}s" || true
  fi
  kdel daemonset cex-dra-driver -n cex-dra-driver
  kdel ns cex-dra-driver
  require_ns_gone cex-dra-driver
  delete_cex_cluster_resources
  remove_vfio_ap_mdevs
  assert_driver_gone
}
