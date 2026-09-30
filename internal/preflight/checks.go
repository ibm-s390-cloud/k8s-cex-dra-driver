package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"k8s-cex-dra-driver/internal/features"
	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/sysfs"
	"k8s-cex-dra-driver/internal/zcrypt"
)

const (
	sysBusAPPath        = "/sys/bus/ap"
	sysClassMdevBusPath = "/sys/class/mdev_bus"
)

// apQueueDirGlob enumerates AP queue directories (card*/<apid>.<apqi>), not
// driver_override paths: globbing the attribute directly could not tell "no
// queues" (inconclusive) from "queues without the attribute" (the kernel
// provably lacks the feature). A var so tests can point it at a synthetic
// tree.
var apQueueDirGlob = "/sys/bus/ap/devices/card*/*.*"

// registry is every check the phase runs, as name, severity and body, in the
// order they are reported. A new check is a new entry here.
//
// Only the virtual-machine path rebinds queues: it moves them from zcrypt to
// vfio_ap through driver_override and builds vfio-ap mediated devices on top.
// Every prerequisite of that rebinding - the vfio_ap module, mdev support,
// the driver_override attribute and the all-1s masks it depends on - therefore
// carries the VirtualMachineWorkload gate and is skipped on a node that
// declares the path disabled. Such a node never writes driver_override, so it
// runs on kernels that predate the AP-bus feature (mainline 6.19). Startup
// recovery stays ungated: it globs driver_override and finds nothing to drain
// where the attribute does not exist.
var registry = []check{
	{name: "/sys/bus/ap present", severity: SeverityError, body: checkPathPresent(sysBusAPPath)},
	{name: "vfio_ap loaded (/sys/devices/vfio_ap/matrix present)", severity: SeverityError, body: checkVFIOAPMatrix, gate: features.VirtualMachineWorkload},
	{name: "mdev support (/sys/class/mdev_bus present)", severity: SeverityError, body: checkPathPresent(sysClassMdevBusPath), gate: features.VirtualMachineWorkload},
	{name: "driver_override on AP queues", severity: SeverityError, body: checkDriverOverride, gate: features.VirtualMachineWorkload},
	{name: "zcrypt masks all-1s (driver_override precondition)", severity: SeverityError, body: checkZcryptMasks, gate: features.VirtualMachineWorkload},
	{name: "zcrypt device nodes (/sys/class/zcrypt/create present)", severity: SeverityError, body: checkZcryptNodes, gate: features.ContainerWorkload},
}

func checkPathPresent(path string) CheckFunc {
	return func() (Status, string) {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return StatusError, "path does not exist"
			}
			return StatusError, err.Error()
		}
		return StatusOK, ""
	}
}

// checkZcryptNodes verifies the kernel offers additional zcrypt device nodes,
// which the container path builds on. The interface has no Kconfig option of
// its own: it is part of the zcrypt driver since Linux 4.20, and the class
// directory appears once the zcrypt module is loaded, with or without crypto
// cards assigned.
func checkZcryptNodes() (Status, string) {
	if !zcrypt.Supported() {
		return StatusError, fmt.Sprintf("%s missing - zcrypt module not loaded or kernel older than 4.20",
			filepath.Join(zcrypt.ClassDir, "create"))
	}
	return StatusOK, ""
}

func checkVFIOAPMatrix() (Status, string) {
	if _, err := os.Stat(mdev.VFIOAPMatrixPath); err != nil {
		if os.IsNotExist(err) {
			return StatusError, "matrix path missing - load vfio_ap kernel module"
		}
		return StatusError, err.Error()
	}
	return StatusOK, ""
}

// checkDriverOverride detects the AP-bus driver_override feature by probing
// the attribute on one queue device. The AP bus creates the attribute for
// every queue it registers, so one probe decides for the whole bus. The
// kernel release rides along as diagnostic detail only - it is printed
// verbatim and never compared: vendor and custom kernels backport the
// feature under version strings that do not track mainline.
//
// Presence only. The attribute exists and is mode 0644 on a node whose masks
// reject every write to it, so usability is checkZcryptMasks's half.
func checkDriverOverride() (Status, string) {
	matches, err := filepath.Glob(apQueueDirGlob)
	if err != nil {
		return StatusError, err.Error()
	}
	if len(matches) == 0 {
		return StatusWarn, fmt.Sprintf("no AP queues to probe, kernel %s", kernelRelease())
	}
	queueDir := matches[0]
	override := filepath.Join(queueDir, "driver_override")
	if _, err := os.Stat(override); err != nil {
		if os.IsNotExist(err) {
			return StatusError, fmt.Sprintf("no driver_override under %s, kernel %s lacks AP-bus driver_override", queueDir, kernelRelease())
		}
		return StatusError, err.Error()
	}
	return StatusOK, fmt.Sprintf("%s, kernel %s", override, kernelRelease())
}

// kernelRelease returns the running kernel's release string for diagnostic
// detail lines.
func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return fmt.Sprintf("uname failed: %v", err)
	}
	return unixString(u.Release[:])
}

// readMasks is swapped by tests. The real reader touches fixed sysfs paths.
var readMasks = sysfs.ReadCurrentMasks

// maskResetCmd is the remediation printed with a failing mask, naming the
// s390-tools command that puts both masks back to all-1s.
const maskResetCmd = "chzdev --type ap apmask=+0x00-0xff aqmask=+0x00-0xff"

// checkZcryptMasks gates startup on both AP masks reading all-1s.
//
// This is not a judgment about how an administrator should configure a node.
// The kernel makes the masks and driver_override mutually exclusive, and the
// gate is bus-wide rather than per-APQN. In drivers/s390/crypto/ap_queue.c,
// driver_override_store() opens with rc = -EINVAL and returns it untouched
// when the flag is set:
//
//	/* Do not allow driver override if apmask/aqmask is in use */
//	if (ap_apmask_aqmask_in_use)
//		goto out;
//
// and drivers/s390/crypto/ap_bus.c derives that flag in both apmask_commit()
// and aqmask_commit() as
//
//	ap_apmask_aqmask_in_use =
//		bitmap_full(ap_perms.apm, AP_DEVICES) &&
//		bitmap_full(ap_perms.aqm, AP_DOMAINS) ? false : true;
//
// So one clear bit in either mask makes every driver_override write on the
// node fail with EINVAL - including writes for queues the mask does not
// touch - which is the driver's only rebinding mechanism. Nothing before
// NodePrepareResources notices: the scan still sees the queues, publishes
// them, and the scheduler allocates them, so the node accepts claims it
// cannot serve and every preparation fails with `write driver_override ...:
// invalid argument`. That total, silent-until-workload failure is why this is
// an error and not the plain report line it used to be, whose premise -
// driver_override working regardless of mask shape - the two functions above
// contradict.
//
// The driver still never writes the masks. It refuses to run instead.
func checkZcryptMasks() (Status, string) {
	ap, aq, err := readMasks()
	if err != nil {
		return StatusError, fmt.Sprintf("cannot read masks: %v", err)
	}
	found := fmt.Sprintf("apmask=%s aqmask=%s", truncateMask(ap), truncateMask(aq))
	if !maskAllOnes(ap) || !maskAllOnes(aq) {
		return StatusError, fmt.Sprintf("%s - a mask that is not all-1s makes every driver_override write fail with EINVAL; reset with: %s", found, maskResetCmd)
	}
	return StatusOK, found
}

// maskAllOnes reports whether a normalized sysfs mask has every bit set. The
// kernel prints one hex digit per four bits, so all-1s is an all-f string.
// The width is not asserted: it follows AP_DEVICES and AP_DOMAINS, which are
// the bus's to choose. Anything that is not hex f fails, so a mask in an
// unexpected format errors here rather than passing silently - a false
// positive costs a startup abort with the masks printed, a false negative
// costs a node that fails every claim.
func maskAllOnes(mask string) bool {
	digits := strings.TrimPrefix(strings.TrimPrefix(mask, "0x"), "0X")
	if digits == "" {
		return false
	}
	for _, c := range digits {
		if c != 'f' && c != 'F' {
			return false
		}
	}
	return true
}

func unixString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func truncateMask(m string) string {
	const max = 18
	if len(m) <= max {
		return m
	}
	return m[:max] + "..."
}
