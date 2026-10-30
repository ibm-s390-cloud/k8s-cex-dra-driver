package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s-cex-dra-driver/internal/features"
)

// setWorkloadGates applies the given workload-gate combination for one test.
// features.Set is process-global, so the cleanup restores both gates to their
// shipped defaults rather than leaving the combination for tests that follow.
func setWorkloadGates(t *testing.T, vm, container bool) {
	t.Helper()
	pairs := fmt.Sprintf("%s=%t,%s=%t", features.VirtualMachineWorkload, vm, features.ContainerWorkload, container)
	if err := features.Set(pairs); err != nil {
		t.Fatalf("set %s: %v", pairs, err)
	}
	t.Cleanup(func() {
		restore := fmt.Sprintf("%s=true,%s=false", features.VirtualMachineWorkload, features.ContainerWorkload)
		if err := features.Set(restore); err != nil {
			t.Errorf("restore %s: %v", restore, err)
		}
	})
}

// TestValidateMachineID pins the startup contract of --machine-id: empty
// defers to sysinfo auto-detection, a compliant label with room for the
// adapter-domain suffix passes, and a value the resource API would refuse
// fails here naming the flag rather than at ResourceSlice write time
// naming the object.
func TestValidateMachineID(t *testing.T) {
	tests := []struct {
		name      string
		machineID string
		wantErr   string // substring of the error, empty means no error
	}{
		{"empty defers to auto-detection", "", ""},
		{"auto-detection shape", "ibm-3931-02-00000000000a8f67", ""},
		{"longest fitting value", strings.Repeat("a", maxMachineIDLen), ""},
		{"one over the cap", strings.Repeat("a", maxMachineIDLen+1), fmt.Sprintf("at most %d", maxMachineIDLen)},
		{"uppercase", "IBM-3931", "DNS-1123"},
		{"underscore", "ibm_3931", "DNS-1123"},
		{"leading hyphen", "-ibm-3931", "DNS-1123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMachineID(tt.machineID)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMachineID(%q) = %v, want nil", tt.machineID, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateMachineID(%q) = nil, want an error containing %q", tt.machineID, tt.wantErr)
			}
			for _, want := range []string{tt.wantErr, "--machine-id"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestValidateScanInterval pins the startup contract of --scan-interval:
// the floor and anything above it pass, anything below fails here naming
// the flag, whether it would have spun the liveness watchdog into a
// restart loop or panicked its ticker.
func TestValidateScanInterval(t *testing.T) {
	tests := []struct {
		name         string
		scanInterval time.Duration
		wantErr      string // substring of the error, empty means no error
	}{
		{"shipped default", 30 * time.Second, ""},
		{"floor", minScanInterval, ""},
		{"just below the floor", minScanInterval - time.Nanosecond, "below the minimum"},
		{"one nanosecond", time.Nanosecond, "below the minimum"},
		{"zero", 0, "below the minimum"},
		{"negative", -5 * time.Second, "below the minimum"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScanInterval(tt.scanInterval)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateScanInterval(%v) = %v, want nil", tt.scanInterval, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateScanInterval(%v) = nil, want an error containing %q", tt.scanInterval, tt.wantErr)
			}
			for _, want := range []string{tt.wantErr, "--scan-interval"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestValidateWorkloadGates pins the supported node shapes: VM-only,
// container-only, and both are fine. Every workload path disabled is a
// startup error naming both gates so the fix is readable from the message.
func TestValidateWorkloadGates(t *testing.T) {
	tests := []struct {
		name          string
		vm, container bool
		wantErr       bool
	}{
		{"both enabled", true, true, false},
		{"vm only (the default)", true, false, false},
		{"container only", false, true, false},
		{"both disabled", false, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setWorkloadGates(t, tt.vm, tt.container)

			err := validateWorkloadGates()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateWorkloadGates() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateWorkloadGates() = nil, want the no-workload error")
			}
			for _, want := range []string{
				string(features.VirtualMachineWorkload),
				string(features.ContainerWorkload),
				"no workload path enabled",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
