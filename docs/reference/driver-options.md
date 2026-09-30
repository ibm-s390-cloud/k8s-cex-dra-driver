---
title: Driver options
weight: 4
description: >
  Every flag and environment variable the kubelet plugin accepts
---

## Flags and environment variables

Every flag of `cex-dra-kubeletplugin` has an environment-variable source.
A flag given on the command line wins over its environment variable.

| Flag                                 | Environment variable               | Default                             | Meaning                                                                                                                                                                                                                                                           |
| ------------------------------------ | ---------------------------------- | ----------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--node-name`                        | `NODE_NAME`                        | hostname                            | The node whose AP queues the driver publishes and whose ResourceSlices it owns.                                                                                                                                                                                   |
| `--machine-id`                       | `MACHINE_ID`                       | parsed from sysinfo                 | Machine identifier that keeps device names unique across machines. Must be a DNS-1123 label of at most 55 characters: device names append an 8-character adapter-domain suffix and the API caps names at 63. The driver refuses a non-compliant value at startup. |
| `--sysinfo-path`                     | `SYSINFO_PATH`                     | `/proc/sysinfo`                     | File parsed for machine-id auto-detection. The container's own procfs exposes the node-global file, so no host mount is needed.                                                                                                                                   |
| `--kubeconfig`                       | `KUBECONFIG`                       | in-cluster config                   | Absolute path to a kubeconfig, for running out-of-cluster. Empty means the in-cluster service account.                                                                                                                                                            |
| `--kubelet-registrar-directory-path` | `KUBELET_REGISTRAR_DIRECTORY_PATH` | `/var/lib/kubelet/plugins_registry` | Directory where the kubelet watches for plugin registration sockets.                                                                                                                                                                                              |
| `--kubelet-plugins-directory-path`   | `KUBELET_PLUGINS_DIRECTORY_PATH`   | `/var/lib/kubelet/plugins`          | Directory where the driver keeps its plugin socket and data, in a `cex-driver.ibm.com/` subdirectory.                                                                                                                                                             |
| `--scan-interval`                    | `SCAN_INTERVAL`                    | `30s`                               | Interval between AP queue scans.                                                                                                                                                                                                                                  |
| `--healthcheck-port`                 | `HEALTHCHECK_PORT`                 | `-1` (disabled)                     | Port for the gRPC liveness service the kubelet probes. Positive is a literal port, zero allocates a random one, negative disables it.                                                                                                                             |
| `--cdi-root`                         | `CDI_ROOT`                         | `/var/run/cdi`                      | Directory the driver writes CDI specification files into.                                                                                                                                                                                                         |
| `--shadow-sysfs-root`                | `SHADOW_SYSFS_ROOT`                | `/var/run/cex-dra/shadowsysfs`      | Host directory holding the per-claim shadow AP sysfs trees of container claims. Must be absolute and mounted at the same path into the plugin container.                                                                                                          |
| `--feature-gates`                    | `FEATURE_GATES`                    | empty                               | `Name=bool` pairs enabling or disabling features. Empty leaves every gate at its stage default. See [Feature gates](feature-gates.md).                                                                                                                            |
| `-v`                                 | `LOG_VERBOSITY`                    | `0`                                 | Klog verbosity level. Per-queue detail from repeat scans and from `driver_override` switching appears at `4` and up.                                                                                                                                              |
| `--vmodule`                          | `LOG_VMODULE`                      | empty                               | Comma-separated `pattern=N` file-filtered klog verbosity overrides.                                                                                                                                                                                               |

`--version` (long form only - `-v` is verbosity) prints the version and exits.

## CEX_DRA_SKIP_PREFLIGHT

Environment variable only, no flag equivalent.
Set to `1`, `true`, or `yes` (case-insensitive), it downgrades preflight errors to warnings so the driver starts on a node that fails a check.
Any other non-empty value keeps strict mode and is called out in the log (`ignoring unrecognized CEX_DRA_SKIP_PREFLIGHT=...`).
The `skip-preflight` Kustomize component sets it.
Dev and demo clusters only, never production.
A node that fails the `vfio_ap` or mdev check because it is not meant to host virtual machines does not need this escape hatch: it declares itself container-only with the [`VirtualMachineWorkload=false` feature gate](feature-gates.md), which skips exactly those checks and keeps the rest strict.

## How the shipped DaemonSet sets these

The base DaemonSet passes no flags at all.
It sets three environment variables - `NODE_NAME` (from a `fieldRef` to `spec.nodeName`), `SCAN_INTERVAL=30s`, and `HEALTHCHECK_PORT=51515` - and leaves everything else at its default.
The environment form is deliberate: container `env` entries merge by name under a strategic-merge patch, so an overlay or feature component setting one option leaves the others in place, while an `args` patch replaces the whole list and would drop them.
That is how the feature components and dev overlays set `FEATURE_GATES` without disturbing the rest.
The `GOMEMLIMIT` variable in the base is a Go runtime setting, not a driver option.

## Liveness

The scan loop can stop making progress without the process dying.
Reading a queue's master key verification pattern submits a request to the card and waits for the answer with no timeout, and the sockets the kubelet's registration protocol talks to keep answering throughout, so nothing in the DRA contract observes a wedged scan loop.
The AP bus arms a request timer of its own, but that timer is a retry rather than a deadline: on expiry it resets the queue and sends the request again, so a card that stays configured and never answers is waited on indefinitely.
The read does unblock when the bus notices the card is deconfigured or checkstopped, which covers an adapter being taken away but not a mute one.
The driver therefore stamps a heartbeat at the end of every completed cycle - including a cycle that returned an error, since the error has its own log line and only a cycle that never returns means the loop is gone - and serves a gRPC health service that reports `SERVING` while that stamp is fresher than a staleness threshold.
The threshold is ten times `SCAN_INTERVAL`, so raising the scan interval cannot leave the two out of step.
The check reads the timestamp and nothing else: a check that touched the AP hardware would block exactly when it needed to answer.

`HEALTHCHECK_PORT` and the base DaemonSet's `livenessProbe` are one unit.
A negative port disables the service, and Kustomize cannot render a probe conditionally, so an overlay that disables the service without also patching the probe out leaves the kubelet dialing a dead port, which is a guaranteed restart loop.
Patch both or neither.

When the heartbeat goes stale the driver logs one line, and one more if it recovers before the kubelet restarts the container, so the diagnosis survives into the restarted pod's previous logs.
The watchdog samples at a quarter of the threshold, so the line trails the stale edge by up to that much and, against the fastest possible probe phase, can lose the race to the restart.
The wait is interruptible, so the restart the probe asks for does complete rather than leaving the pod stuck in `Terminating`.

## Non-default kubelet root

The two kubelet directory defaults assume the standard root `/var/lib/kubelet`.
On a node where the kubelet runs with a different `--root-dir` - as some k3s, RKE2, and MicroShift setups do - the flags alone are not enough: the base DaemonSet's `plugins-registry` and `plugins` hostPath volumes point at the same paths and must move with them.
The `plugins_registry` mount is `type: Directory`, so with a wrong path the pod never starts.
It stays in ContainerCreating with a FailedMount event instead of failing inside the driver.
Patch the flags and the volumes together in your overlay.
