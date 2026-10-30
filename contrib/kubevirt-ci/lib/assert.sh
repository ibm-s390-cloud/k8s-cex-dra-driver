#!/usr/bin/env bash
# Copyright contributors to the k8s-cex-dra-driver project
# SPDX-License-Identifier: Apache-2.0
#
# Assertion and cluster-state helpers for e2e scripts.
# Expects DELETE_WAIT to be set by the caller.
# shellcheck disable=SC2154

require_empty() {
  local what="$1"
  shift
  if items_exist "$@"; then
    kubectl get "$@" 2>/dev/null || true
    fail "${what} still exists after ${DELETE_WAIT}s; refusing to continue"
  fi
}

require_ns_gone() {
  local ns="$1"
  if still_there ns "$ns"; then
    dump_ns "$ns"
    fail "namespace ${ns} still exists after ${DELETE_WAIT}s; refusing to continue"
  fi
}

dump_ns() {
  local ns="$1"
  echo "==== leftover objects in namespace ${ns} ===="
  kubectl get ns "$ns" -o jsonpath='{.metadata.name} {.status.phase}{"\n"}' 2>/dev/null || true
  kubectl get all,sa,cm,secret,pvc,role,rolebinding -n "$ns" 2>/dev/null || true
}

cex_slice_names() {
  crd_exists resourceslices.resource.k8s.io || return 0
  kubectl get resourceslice \
    -o jsonpath='{range .items[?(@.spec.driver=="cex-driver.ibm.com")]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null || true
}

cex_cluster_leftovers() {
  # Cluster-scoped RBAC and device class objects left by the CEX DRA driver.
  grep -Ei 'cex-dra|ap-queue\.virtual-machine\.ibm\.com' \
    <(kubectl get clusterrole -o name 2>/dev/null) \
    <(kubectl get clusterrolebinding -o name 2>/dev/null) \
    <(kubectl get deviceclass -o name 2>/dev/null) ||
    true
  # ResourceSlice objects owned by the CEX driver (cluster-scoped, not namespaced).
  local s
  while IFS= read -r s; do
    [[ -n "$s" ]] && echo "resourceslice.resource.k8s.io/${s}"
  done < <(cex_slice_names)
}

assert_driver_ns_gone() {
  if still_there ns cex-dra-driver; then
    dump_ns cex-dra-driver
    fail "namespace cex-dra-driver still exists; refusing to continue"
  fi
}

assert_driver_cluster_objects_gone() {
  local leftover
  leftover="$(cex_cluster_leftovers)"
  if [[ -n "$leftover" ]]; then
    echo "$leftover"
    fail "CEX DRA cluster objects still exist; refusing to continue"
  fi
}

assert_driver_pods_gone() {
  if kubectl get pods -A -l app.kubernetes.io/name=cex-dra-driver --no-headers 2>/dev/null | grep -q .; then
    kubectl get pods -A -l app.kubernetes.io/name=cex-dra-driver
    fail "CEX DRA pods still exist; refusing to continue"
  fi
}

assert_driver_gone() {
  assert_driver_ns_gone
  assert_driver_cluster_objects_gone
  assert_driver_pods_gone
  echo "CEX DRA driver: fully gone"
}

assert_registry_gone() {
  local ns="${REGISTRY_NS:-cex-dra-registry}"
  if still_there ns "${ns}"; then
    dump_ns "${ns}"
    fail "namespace ${ns} still exists; refusing to continue"
  fi
  echo "in-cluster registry: fully gone"
}

check_required_tools() {
  need_cmd kubectl
  need_cmd git
  need_cmd podman
  need_cmd curl
  need_cmd envsubst
}
