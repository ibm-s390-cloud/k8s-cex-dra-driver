package devicestate

import (
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"k8s-cex-dra-driver/internal/features"
)

// enableContainerWorkload flips the ContainerWorkload gate on for one test.
// features.Set is process-global, so the cleanup restores the alpha default
// rather than leaving the gate on for tests that run after.
func enableContainerWorkload(t *testing.T) {
	t.Helper()
	if err := features.Set(string(features.ContainerWorkload) + "=true"); err != nil {
		t.Fatalf("enable %s: %v", features.ContainerWorkload, err)
	}
	t.Cleanup(func() {
		if err := features.Set(string(features.ContainerWorkload) + "=false"); err != nil {
			t.Errorf("restore %s: %v", features.ContainerWorkload, err)
		}
	})
}

// disableVirtualMachineWorkload flips the VirtualMachineWorkload gate off for
// one test. Same process-global caveat as enableContainerWorkload, with the
// restore direction flipped: this gate defaults on.
func disableVirtualMachineWorkload(t *testing.T) {
	t.Helper()
	if err := features.Set(string(features.VirtualMachineWorkload) + "=false"); err != nil {
		t.Fatalf("disable %s: %v", features.VirtualMachineWorkload, err)
	}
	t.Cleanup(func() {
		if err := features.Set(string(features.VirtualMachineWorkload) + "=true"); err != nil {
			t.Errorf("restore %s: %v", features.VirtualMachineWorkload, err)
		}
	})
}

// mixedClaim builds an allocated claim with one VM and one container request,
// the combination Prepare refuses regardless of gate state.
func mixedClaim(t *testing.T) *resourceapi.ResourceClaim {
	t.Helper()
	claim := claimWithMode(t, DeviceClassVM, "")
	claim.Spec.Devices.Requests = append(claim.Spec.Devices.Requests, resourceapi.DeviceRequest{
		Name:    "ap-queue-container",
		Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: DeviceClassContainer},
	})
	claim.Status.Allocation.Devices.Results = append(claim.Status.Allocation.Devices.Results,
		resourceapi.DeviceRequestAllocationResult{
			Request: "ap-queue-container",
			Driver:  cdTestDriver,
			Pool:    "card01",
			Device:  "cex-01-0002",
		})
	return claim
}

// TestPrepareRejectsContainerClaimWhileGateDisabled pins the failure mode the
// gate exists for: with the gate at its default, a container claim must fail
// loudly at prepare instead of preparing into a pod with no crypto device. The
// error names both the DeviceClass and the gate, so the pod event is
// actionable without reading driver source.
func TestPrepareRejectsContainerClaimWhileGateDisabled(t *testing.T) {
	s := cdTestState(t)

	_, err := s.Prepare(t.Context(), claimWithMode(t, DeviceClassContainer, ""))
	if err == nil {
		t.Fatal("Prepare accepted a container claim with the gate disabled")
	}
	for _, want := range []string{string(features.ContainerWorkload), DeviceClassContainer, "disabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
}

// TestPrepareRejectsVMClaimWhileGateDisabled mirrors the container-gate
// rejection on the VM side: with VirtualMachineWorkload off the node was
// declared container-only, so a VM claim must fail loudly at prepare, before
// anything is created or switched. The error names both the DeviceClass and
// the gate, so the pod event is actionable without reading driver source.
func TestPrepareRejectsVMClaimWhileGateDisabled(t *testing.T) {
	disableVirtualMachineWorkload(t)
	s := cdTestState(t)

	_, err := s.Prepare(t.Context(), claimWithMode(t, DeviceClassVM, ""))
	if err == nil {
		t.Fatal("Prepare accepted a VM claim with the gate disabled")
	}
	for _, want := range []string{string(features.VirtualMachineWorkload), DeviceClassVM, "disabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
}

// TestUnprepareVMClaimIgnoresGate pins that cleanup is never gated: a claim
// prepared while the gate was on must tear down after the gate goes off, or
// a node could never exit VM duty cleanly.
func TestUnprepareVMClaimIgnoresGate(t *testing.T) {
	disableVirtualMachineWorkload(t)
	const claimUID = "claim-under-test"

	f := newUnprepareFixture(t, map[string]string{"01.0002": "vfio_ap"})
	f.addMdev(t, claimUID, "01.0002")
	pluginDataDir := t.TempDir()
	seedClaimMetadata(t, pluginDataDir, claimUID)

	s := New("node", cdTestDriver, t.TempDir(), pluginDataDir)
	if err := s.Unprepare(t.Context(), claimUID); err != nil {
		t.Fatalf("Unprepare with the gate disabled: %v", err)
	}
	if got := f.override(t, "01.0002"); got != "" {
		t.Errorf("driver_override = %q, want cleared (back on zcrypt)", got)
	}
}

// TestPrepareVMClaimIgnoresContainerGate pins that the VM path never consults
// the container gate: the same VM claim takes the same path to the same
// outcome in both ContainerWorkload states.
func TestPrepareVMClaimIgnoresContainerGate(t *testing.T) {
	prepare := func(t *testing.T) error {
		newUnprepareFixture(t, nil)
		s := cdTestState(t)
		_, err := s.Prepare(t.Context(), claimWithMode(t, DeviceClassVM, ""))
		return err
	}

	var gateOff, gateOn error
	t.Run("gate off", func(t *testing.T) {
		gateOff = prepare(t)
	})
	t.Run("gate on", func(t *testing.T) {
		enableContainerWorkload(t)
		gateOn = prepare(t)
	})

	// The fixture fakes sysfs only far enough for the VM path to fail at the
	// queue switch (the exact message embeds per-test temp paths, so no string
	// equality). Both runs must reach that same point, and never the gate.
	for name, err := range map[string]error{"gate off": gateOff, "gate on": gateOn} {
		if err == nil {
			t.Errorf("%s: Prepare succeeded, want the fixture's queue-switch failure", name)
			continue
		}
		if !strings.Contains(err.Error(), "switch queue") {
			t.Errorf("%s: err = %q, want the queue-switch failure", name, err)
		}
		if strings.Contains(err.Error(), string(features.ContainerWorkload)) {
			t.Errorf("%s: err = %q, want no gate mention on the VM path", name, err)
		}
	}
}

// TestPrepareRejectsMixedClaimRegardlessOfGate pins the check order: mixing is
// unsupported whatever the workload gates say, so the mixed-claim error wins
// in every gate state and never mentions a gate.
func TestPrepareRejectsMixedClaimRegardlessOfGate(t *testing.T) {
	tests := []struct {
		name  string
		gates func(t *testing.T)
	}{
		{"container gate off", func(t *testing.T) { t.Helper() }},
		{"container gate on", enableContainerWorkload},
		{"vm gate off", disableVirtualMachineWorkload},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.gates(t)
			s := cdTestState(t)

			_, err := s.Prepare(t.Context(), mixedClaim(t))
			if err == nil {
				t.Fatal("Prepare accepted a mixed VM+container claim")
			}
			if !strings.Contains(err.Error(), "mixes VM and container DeviceClasses") {
				t.Errorf("err = %q, want the mixed-claim message", err)
			}
			for _, gate := range []string{string(features.ContainerWorkload), string(features.VirtualMachineWorkload)} {
				if strings.Contains(err.Error(), gate) {
					t.Errorf("err = %q, want no %s mention in the mixed-claim message", err, gate)
				}
			}
		})
	}
}
