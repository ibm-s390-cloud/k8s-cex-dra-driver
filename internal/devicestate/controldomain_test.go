package devicestate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s-cex-dra-driver/internal/cryptoconfig"
)

const cdTestDriver = "cex-driver.ibm.com"

// claimWithMode builds an allocated single-queue claim against deviceClass,
// carrying mode as claim-level CryptoConfig. An empty mode omits the config
// entirely, which is what a claim that never mentions control domains looks
// like.
func claimWithMode(t *testing.T, deviceClass, mode string) *resourceapi.ResourceClaim {
	t.Helper()
	if mode == "" {
		return claimWithRawConfig(t, deviceClass, "")
	}
	return claimWithRawConfig(t, deviceClass,
		`{"kind":"CryptoConfig","apiVersion":"cex.ibm.com/v1alpha1","controlDomainMode":"`+mode+`"}`)
}

// claimWithRawConfig builds the same claim as claimWithMode but takes the
// opaque parameters verbatim, so a test can hand the driver a payload no
// well-formed CryptoConfig could produce. An empty raw omits the config block.
func claimWithRawConfig(t *testing.T, deviceClass, raw string) *resourceapi.ResourceClaim {
	t.Helper()
	claim := &resourceapi.ResourceClaim{
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: []resourceapi.DeviceRequest{{
					Name:    "ap-queue",
					Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: deviceClass},
				}},
			},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Request: "ap-queue",
						Driver:  cdTestDriver,
						Pool:    "card01",
						Device:  "cex-01-0002",
					}},
				},
			},
		},
	}
	claim.UID = "claim-under-test"

	if raw != "" {
		claim.Status.Allocation.Devices.Config = []resourceapi.DeviceAllocationConfiguration{{
			Source: resourceapi.AllocationConfigSourceClaim,
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver:     cdTestDriver,
					Parameters: runtime.RawExtension{Raw: []byte(raw)},
				},
			},
		}}
	}
	return claim
}

// cdTestState returns a DeviceState that knows the one device claimWithMode
// allocates, so Prepare gets past the allocatable lookup.
func cdTestState(t *testing.T) *DeviceState {
	t.Helper()
	s := New("node", cdTestDriver, t.TempDir(), t.TempDir())
	s.allocatable["cex-01-0002"] = resourceapi.Device{
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"cex.ibm.com/apid": {StringValue: ptr("01")},
			"cex.ibm.com/apqi": {StringValue: ptr("0002")},
		},
	}
	return s
}

func ptr(s string) *string { return &s }

// TestPrepareRejectsControlDomainModeBeforeTouchingSysfs covers the two failure
// outcomes the closed value set exists to tell apart, and pins that neither
// leaves a half-built mdev behind: the fixture's matrix root has no `create`
// attribute, so any attempt to build one would fail with a different error.
func TestPrepareRejectsControlDomainModeBeforeTouchingSysfs(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{
			name:    "unbounded mode is known but not enabled",
			mode:    cryptoconfig.ControlDomainModeAllControlDomains,
			wantErr: "is not enabled",
		},
		{
			name:    "typo is unknown",
			mode:    "all-control-domain",
			wantErr: "unknown controlDomainMode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnprepareFixture(t, nil)
			s := cdTestState(t)

			_, err := s.Prepare(t.Context(), claimWithMode(t, DeviceClassVM, tc.mode))
			if err == nil {
				t.Fatalf("Prepare succeeded for controlDomainMode %q", tc.mode)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want it to contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.mode) {
				t.Errorf("err = %q, want it to name the mode %q", err, tc.mode)
			}

			entries, readErr := os.ReadDir(f.matrix)
			if readErr != nil {
				t.Fatalf("read matrix dir: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("matrix dir has %d entries, want none (no mdev built)", len(entries))
			}
		})
	}
}

// TestAssignMdevControlDomains covers the two enabled modes against a fake
// matrix tree.
//
// One caveat shapes the multi-domain case: on real sysfs each
// assign_control_domain write sets a bit in the mdev's ADM and the union is read
// back from control_domains, but a plain file has no such accumulation - each
// write truncates, so only the last one survives. The union is therefore
// asserted on hardware by the e2e case, and what is pinned here is that every
// mode drives the right number of writes to the right attribute.
func TestAssignMdevControlDomains(t *testing.T) {
	const claimUID = "claim-under-test"

	tests := []struct {
		name    string
		mode    string
		domains []string
		// wantOneOf lists the values a single write may leave behind. Empty
		// means no write is expected at all.
		wantOneOf []string
	}{
		{
			name:      "usage-only assigns nothing",
			mode:      cryptoconfig.ControlDomainModeUsageOnly,
			domains:   []string{"0002"},
			wantOneOf: nil,
		},
		{
			name:      "usage-and-control assigns the allocated domain",
			mode:      cryptoconfig.ControlDomainModeUsageAndControl,
			domains:   []string{"0002"},
			wantOneOf: []string{"0x0002"},
		},
		{
			name:      "usage-and-control assigns every allocated domain",
			mode:      cryptoconfig.ControlDomainModeUsageAndControl,
			domains:   []string{"0002", "002a"},
			wantOneOf: []string{"0x0002", "0x002a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnprepareFixture(t, nil)
			dir := filepath.Join(f.matrix, claimUID)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			attr := filepath.Join(dir, "assign_control_domain")
			write(t, attr, "")

			domains := make(map[string]bool, len(tc.domains))
			for _, d := range tc.domains {
				domains[d] = true
			}

			if err := assignMdevControlDomains(claimUID, tc.mode, domains); err != nil {
				t.Fatalf("assignMdevControlDomains: %v", err)
			}

			got, err := os.ReadFile(attr) //nolint:gosec // test-owned temp path
			if err != nil {
				t.Fatalf("read assign_control_domain: %v", err)
			}
			if len(tc.wantOneOf) == 0 {
				if string(got) != "" {
					t.Errorf("assign_control_domain = %q, want empty (nothing assigned)", got)
				}
				return
			}
			if !slices.Contains(tc.wantOneOf, string(got)) {
				t.Errorf("assign_control_domain = %q, want one of %v", got, tc.wantOneOf)
			}
		})
	}
}

// TestAssignMdevControlDomainsPropagatesWriteFailure pins that a failed
// assignment is surfaced rather than swallowed: without it a claim would report
// PREPARE success while the guest silently lacks control access.
func TestAssignMdevControlDomainsPropagatesWriteFailure(t *testing.T) {
	newUnprepareFixture(t, nil)

	// No mdev directory was created, so the write has nowhere to land.
	err := assignMdevControlDomains("no-such-claim",
		cryptoconfig.ControlDomainModeUsageAndControl, map[string]bool{"0002": true})
	if err == nil {
		t.Fatal("assignMdevControlDomains succeeded without an mdev")
	}
	if !strings.Contains(err.Error(), "0x0002") {
		t.Errorf("err = %q, want it to name the domain", err)
	}
}
