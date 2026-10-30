#!/usr/bin/env bash
# Copyright contributors to the k8s-cex-dra-driver project
# SPDX-License-Identifier: Apache-2.0
#
# Deploy the CEX DRA driver for CI / shared clusters.
#
# Does NOT install, configure, or uninstall KubeVirt.
#
#  1. Remove leftover test workloads, driver, and in-cluster registry
#  2. Start an in-cluster registry
#  3. Build the driver image, push it, import it into the node store
#  4. Deploy the driver DaemonSet and wait for a ResourceSlice
#
# Run:
#   ./contrib/kubevirt-ci/cex-dra.sh deploy
#   ./contrib/kubevirt-ci/cex-dra.sh undeploy
#
# CRI-O cannot pull plain HTTP without host config, so a privileged Job copies
# the image into the node store.
#
# DELETE_WAIT=120 max seconds to wait for each uninstall (then error and stop)
# IMAGE_TAG=...   default: first 12 chars of HEAD git SHA

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034 # read by lib/*.sh
REPO_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

OVERLAY="${OVERLAY:-my-cluster}"
DELETE_WAIT="${DELETE_WAIT:-120}"
TEST_NS="${TEST_NS:-cex-dra-test}"
WORK_DIR="${WORK_DIR:-$HOME/cex-dra-e2e-work}"

PLUGIN_REPO="${PLUGIN_REPO:-cex-dra-kubeletplugin}"
IMAGE_TAG="${IMAGE_TAG:-}"
REGISTRY_NS="${REGISTRY_NS:-cex-dra-registry}"
REGISTRY_NAME="${REGISTRY_NAME:-registry}"
REGISTRY_NODEPORT="${REGISTRY_NODEPORT:-30511}"
REGISTRY_LOCAL_PORT="${REGISTRY_LOCAL_PORT:-5000}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-docker.io/library/registry:2}"
IMPORT_IMAGE="${IMPORT_IMAGE:-quay.io/skopeo/stable:latest}"
# shellcheck disable=SC2034 # read by lib/*.sh
IMAGE_NAME="" \
  IMAGE="" \
  REGISTRY_ADDR="" \
  REGISTRY_PF_PID=""

# shellcheck source=lib/helpers.sh
source "${SCRIPT_DIR}/lib/helpers.sh"
# shellcheck source=lib/assert.sh
source "${SCRIPT_DIR}/lib/assert.sh"
# shellcheck source=lib/workloads.sh
source "${SCRIPT_DIR}/lib/workloads.sh"
# shellcheck source=lib/uninstall.sh
source "${SCRIPT_DIR}/lib/uninstall.sh"
# shellcheck source=lib/driver.sh
source "${SCRIPT_DIR}/lib/driver.sh"

on_exit() {
  local rc=$?
  stop_registry_port_forward
  if [[ $rc -ne 0 ]]; then
    echo >&2
    echo "FAILED (exit ${rc}). Cluster state left in place for debugging." >&2
    echo "  re-run:  ${0} ${COMMAND:-deploy}" >&2
  fi
}
trap on_exit EXIT

usage() {
  cat <<USAGE
Usage: ${0##*/} [command]

Commands:
  deploy     (default) remove leftover driver/registry, then build and deploy
  undeploy   remove the driver, in-cluster registry, and test namespace
  -h         show this help

Env: DELETE_WAIT=${DELETE_WAIT}  IMAGE_TAG  TEST_NS=${TEST_NS}
USAGE
}

do_undeploy() {
  info "Remove CEX DRA driver and in-cluster registry"
  delete_workloads
  uninstall_driver
  uninstall_registry
}

do_deploy() {
  info "Remove existing driver and registry (if any)"
  delete_workloads
  uninstall_driver
  uninstall_registry
  assert_driver_gone
  assert_registry_gone

  install_registry
  build_image
  push_image
  import_image_to_node
  deploy_driver
}

main() {
  COMMAND="${1:-deploy}"
  mkdir -p "${WORK_DIR}"

  case "${1:-deploy}" in
    deploy)
      check_required_tools
      do_deploy
      ;;
    undeploy)
      need_cmd kubectl
      do_undeploy
      ;;
    -h | --help | help)
      usage
      ;;
    *)
      usage
      fail "unknown command: $1"
      ;;
  esac
}

main "$@"
