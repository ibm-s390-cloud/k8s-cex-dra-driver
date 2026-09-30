---
title: Claim configuration
weight: 2
description: >
  The CryptoConfig schema and the control-domain modes
---

## CryptoConfig

The opaque claim configuration accepted under `devices.config[].opaque` with `driver: cex-driver.ibm.com`.
Parsing is strict: unknown fields, a wrong `kind`, or a wrong `apiVersion` reject the claim configuration.

| Field               | Type   | Required | Meaning                                                                                |
| ------------------- | ------ | -------- | -------------------------------------------------------------------------------------- |
| `kind`              | string | yes      | Must be `CryptoConfig`                                                                 |
| `apiVersion`        | string | yes      | Must be `cex.ibm.com/v1alpha1`                                                         |
| `controlDomainMode` | string | no       | Control-domain access for the claim's mediated device. Defaults to `usage-and-control` |

`controlDomainMode` values:

| Value                 | Status               | Effect                                                        |
| --------------------- | -------------------- | ------------------------------------------------------------- |
| `usage-and-control`   | enabled (default)    | Control access to exactly the domains the claim was allocated |
| `usage-only`          | enabled              | No control access, usage access only                          |
| `all-control-domains` | defined, not enabled | Control access to every domain, refused at prepare time       |
| `0x` + 64 hex digits  | defined, not enabled | Custom 256-bit control-domain mask, refused at prepare time   |

The two unbounded modes grant access irrespective of the claim's allocation, and control domains are shareable by design, so they stay refused until admission-time policy can bound who may request them.
A claim using one is admitted and allocated but fails at prepare with `controlDomainMode "..." is a known mode that is not enabled`.
A misspelled value fails with `unknown controlDomainMode`, so a typo never looks like a not-yet-available mode.

One effective value per claim, resolved built-in default, then DeviceClass configuration, then claim configuration.
Container claims apply the two enabled modes the same way through the zcrypt node's control-domain mask, and grant no control domain for a mode that is not enabled instead of rejecting the claim.
