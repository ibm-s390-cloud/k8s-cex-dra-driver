#!/usr/bin/env bash
# Copyright contributors to the k8s-cex-dra-driver project
# SPDX-License-Identifier: Apache-2.0
#
# Helper functions shared across e2e scripts.
# Variables such as REPO_DIR and DELETE_WAIT are set by contrib/kubevirt-ci/cex-dra.sh.
# shellcheck disable=SC2154

fail() {
  echo "ERROR: $*" >&2
  exit 1
}
info() {
  echo
  echo "======== $* ========"
}
step() {
  echo
  echo "-------- $* --------"
}
need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"
}
overlay_dir() {
  echo "${REPO_DIR}/deploy/kustomize/overlays/${OVERLAY}"
}
crd_exists() {
  kubectl get crd "$1" >/dev/null 2>&1
}
# Return 0 if kubectl get still finds the object(s).
still_there() { kubectl get "$@" >/dev/null 2>&1; }
# Delete and wait up to DELETE_WAIT seconds. Caller must then require_* / assert.
kdel() {
  kubectl delete "$@" --ignore-not-found=true --wait=true --timeout="${DELETE_WAIT}s" || true
}
items_exist() {
  kubectl get "$@" --no-headers 2>/dev/null | grep -q '[^[:space:]]'
}
