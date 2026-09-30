package devicestate

import (
	"strings"
	"testing"

	"k8s-cex-dra-driver/internal/cryptoconfig"
	"k8s-cex-dra-driver/internal/zcrypt"
)

// TestPrepareRejectsMalformedConfigOnEitherBinding pins that strict parsing is a
// property of the claim, not of the node that happens to run it.
//
// The binding type is decided by the DeviceClass, so the same claim text can be
// prepared down either path. If only one path parsed, a typo'd field would fail
// on a VM claim and be waved through on a container claim - and an operator who
// hit the forgiving path would have no signal that the value they wrote is not
// the value the driver reads.
func TestPrepareRejectsMalformedConfigOnEitherBinding(t *testing.T) {
	// The gate check precedes config resolution, so the container half needs
	// the ContainerWorkload gate on to reach the parser at all. The VM half
	// never consults the gate.
	enableContainerWorkload(t)

	payloads := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "unknown field",
			raw:     `{"kind":"CryptoConfig","apiVersion":"cex.ibm.com/v1alpha1","controlDomainMod":"usage-only"}`,
			wantErr: "decode CryptoConfig",
		},
		{
			name:    "wrong kind",
			raw:     `{"kind":"CryptoConfiguration","apiVersion":"cex.ibm.com/v1alpha1"}`,
			wantErr: "unexpected kind",
		},
		{
			name:    "future apiVersion",
			raw:     `{"kind":"CryptoConfig","apiVersion":"cex.ibm.com/v1beta1"}`,
			wantErr: "unsupported apiVersion",
		},
		{
			name:    "malformed json",
			raw:     `{"kind":"CryptoConfig",`,
			wantErr: "decode CryptoConfig",
		},
	}

	bindings := []struct {
		name        string
		deviceClass string
	}{
		{"vm", DeviceClassVM},
		{"container", DeviceClassContainer},
	}

	for _, b := range bindings {
		for _, p := range payloads {
			t.Run(b.name+"/"+p.name, func(t *testing.T) {
				s := cdTestState(t)

				_, err := s.Prepare(t.Context(), claimWithRawConfig(t, b.deviceClass, p.raw))
				if err == nil {
					t.Fatalf("Prepare accepted %s on the %s path", p.name, b.name)
				}
				if !strings.Contains(err.Error(), p.wantErr) {
					t.Errorf("err = %q, want it to contain %q", err, p.wantErr)
				}
			})
		}
	}
}

// TestPrepareAcceptsWellFormedConfigOnEitherBinding is the counterweight to the
// test above: parsing is strict, not hostile. A container claim carrying a
// control-domain mode the container path does not serve - including one the
// driver refuses on the VM path - still prepares: the node then grants no
// control domain rather than failing the claim.
func TestPrepareAcceptsWellFormedConfigOnEitherBinding(t *testing.T) {
	// See TestPrepareRejectsMalformedConfigOnEitherBinding: the container half
	// is reachable only with the ContainerWorkload gate on.
	enableContainerWorkload(t)

	tests := []struct {
		name        string
		deviceClass string
		mode        string
	}{
		{"vm with an enabled mode", DeviceClassVM, cryptoconfig.ControlDomainModeUsageOnly},
		{"container with an enabled mode", DeviceClassContainer, cryptoconfig.ControlDomainModeUsageOnly},
		{"container with a not-enabled mode", DeviceClassContainer, cryptoconfig.ControlDomainModeAllControlDomains},
		{"container with no config at all", DeviceClassContainer, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The VM case needs a matrix root: usage-only still builds an mdev,
			// it just assigns no control domain.
			newUnprepareFixture(t, nil)
			if tc.deviceClass == DeviceClassContainer {
				newZcryptFixture(t).addNode(t, zcrypt.NodeName("claim-under-test"))
			}
			s := cdTestState(t)

			_, err := s.Prepare(t.Context(), claimWithMode(t, tc.deviceClass, tc.mode))
			if tc.deviceClass == DeviceClassContainer {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			// The VM path goes on to touch sysfs, which the fixture only fakes
			// far enough to fail at mdev creation. What matters here is that it
			// got past config resolution.
			if err != nil && strings.Contains(err.Error(), "CryptoConfig") {
				t.Fatalf("Prepare failed in config resolution: %v", err)
			}
		})
	}
}
