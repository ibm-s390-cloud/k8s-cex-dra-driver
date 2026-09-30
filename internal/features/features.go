// Package features hosts the driver's feature-gate registry, shared by every
// driver binary. Gates are maturity switches for a feature's lifecycle (alpha
// opt-in, beta opt-out, GA unconditionally on), not configuration tunables:
// tunables stay ordinary flags or per-claim CryptoConfig.
//
// Each binary parses its --feature-gates value into the registry once at
// startup with Set. Everything afterwards only reads, through Gate:
//
//	if features.Gate.Enabled(features.SomeFeature) { ... }
//
// The registry ships empty of project gates. A feature that needs one declares
// it below, following the declaration convention.
package features

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

// Gate declaration convention: PascalCase, named for the feature (no "Enable"
// prefix), one constant per gate with a doc comment naming the owner and the
// release that shipped each stage. A released gate name is API - it never
// renames, and it is removed only at the next major after the feature went GA.
//
//	const (
//		// SomeFeature enables <one-line description>.
//		//
//		// Owner: @some-handle
//		// Alpha: v1.1.0
//		// Beta:  v1.2.0.
//		SomeFeature featuregate.Feature = "SomeFeature"
//	)

const (
	// ContainerWorkload enables claims against the container DeviceClass
	// ap-queue.container.ibm.com, which binds AP queues into containers via
	// zcrypt rather than vfio-ap. The path behind it is an unimplemented
	// stub: enabling the gate exposes it for development, it does not make
	// a container claim deliver a device.
	//
	// Owner: @bodo-brand1
	// Alpha: v1.0.0.
	ContainerWorkload featuregate.Feature = "ContainerWorkload"

	// VirtualMachineWorkload enables the virtual-machine passthrough path:
	// claims against the DeviceClass ap-queue.virtual-machine.ibm.com,
	// vfio-ap binding and mdev creation, and the VM-stack preflight checks
	// (vfio_ap, mdev, AP-bus driver_override, all-1s zcrypt masks). Disabling
	// it is a supported configuration for nodes that only serve container
	// workloads, including nodes whose kernel lacks AP-bus driver_override.
	//
	// Owner: @bodo-brand1
	// Beta: v1.0.0-alpha.0
	// GA:   v1.0.0, unlocked - the opt-out remains supported indefinitely.
	VirtualMachineWorkload featuregate.Feature = "VirtualMachineWorkload"
)

// defaultFeatureGates registers every gate with its stage spec. Stages map to
// specs as follows:
//
//	Alpha:         {Default: false, PreRelease: featuregate.Alpha}
//	Beta:          {Default: true, PreRelease: featuregate.Beta}
//	GA:            {Default: true, PreRelease: featuregate.GA, LockToDefault: true}
//	GA (unlocked): {Default: true, PreRelease: featuregate.GA}
//	Deprecated:    {Default: false, PreRelease: featuregate.Deprecated}
//
// GA-unlocked is for gates whose disabled state is a supported operational
// configuration, not a graduation leftover: the =false opt-out stays honored
// indefinitely. It is declared per gate and justified in the gate's doc
// comment. An ordinary graduation locks.
var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	ContainerWorkload:      {Default: false, PreRelease: featuregate.Alpha},
	VirtualMachineWorkload: {Default: true, PreRelease: featuregate.Beta},
}

// registry couples a mutable feature gate with the warning sink used for
// settings that are known but ineffective. The sink is a field so tests can
// record warnings without redirecting klog globally.
type registry struct {
	gate  featuregate.MutableFeatureGate
	warnf func(format string, args ...any)
}

// newRegistry builds a registry holding the given gates on top of the
// library's built-in AllAlpha / AllBeta group gates. It panics on a malformed
// spec map, which is a programming error in the declarations above.
func newRegistry(specs map[featuregate.Feature]featuregate.FeatureSpec) *registry {
	gate := featuregate.NewFeatureGate()
	if err := gate.Add(specs); err != nil {
		panic(fmt.Sprintf("register feature gates: %v", err))
	}
	return &registry{gate: gate, warnf: klog.Warningf}
}

var defaultRegistry = newRegistry(defaultFeatureGates)

// Gate is the read view of the registry, queried as
// features.Gate.Enabled(features.SomeFeature).
var Gate featuregate.FeatureGate = defaultRegistry.gate

// Set applies a --feature-gates value at startup. A pair naming a locked GA
// gate logs a warning and is dropped - the feature is unconditionally on, and
// an upgrade must not fail over an opt-out that was valid in the previous
// release. A pair naming an unlocked GA gate passes through: its opt-out is a
// supported configuration, not a leftover. The remaining pairs go to the
// library, so an unknown name or a non-bool value still returns an error and
// aborts startup.
func Set(value string) error { return defaultRegistry.set(value) }

// Summary renders every known gate with its resolved value, sorted by name, as
// the startup log line's payload. Unlike KnownFeatures it includes GA and
// deprecated gates, so the log shows the value that actually applies.
func Summary() string { return defaultRegistry.summary() }

// KnownFeatures describes the settable gates - name, stage, and default - for
// the --feature-gates help text. The library hides every GA gate from its
// list. The unlocked ones stay settable, so they are appended here to keep
// the help text the complete inventory of settable gates.
func KnownFeatures() []string { return defaultRegistry.knownFeatures() }

func (r *registry) set(value string) error {
	kept := make([]string, 0, strings.Count(value, ",")+1)
	graduated := r.graduatedGates()

	for _, pair := range strings.Split(value, ",") {
		if pair == "" {
			continue
		}
		name := strings.TrimSpace(pair)
		if i := strings.Index(pair, "="); i >= 0 {
			name = strings.TrimSpace(pair[:i])
		}
		if _, ok := graduated[featuregate.Feature(name)]; ok {
			r.warnf("feature gate %s is GA and always on, so the setting %s is ignored, remove it",
				name, strings.TrimSpace(pair))
			continue
		}
		kept = append(kept, pair)
	}

	if err := r.gate.Set(strings.Join(kept, ",")); err != nil {
		return fmt.Errorf("parse --feature-gates: %w", err)
	}
	return nil
}

func (r *registry) summary() string {
	all := r.gate.GetAll()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, string(name))
	}
	sort.Strings(names)

	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, fmt.Sprintf("%s=%t", name, r.gate.Enabled(featuregate.Feature(name))))
	}
	return strings.Join(pairs, " ")
}

// graduatedGates collects the gates whose setting has no effect: the ones
// declared locked to their default. The library hard-errors on a write to a
// locked gate. Filtering them out first is what turns that error into a
// warning. Keying on the lock bit alone, not the GA stage, is what makes the
// GA-unlocked variant work: the library accepts writes to an unlocked GA gate
// natively, so its pairs pass through.
func (r *registry) graduatedGates() map[featuregate.Feature]struct{} {
	graduated := map[featuregate.Feature]struct{}{}
	for name, spec := range r.gate.GetAll() {
		if spec.LockToDefault {
			graduated[name] = struct{}{}
		}
	}
	return graduated
}

// knownFeatures appends the settable GA gates to the library's help entries.
// The library's KnownFeatures hides every GA gate (HelpString returns "" for
// GA), which is right for locked gates and wrong for unlocked ones. The GA
// stage renders as an empty string, so the entry spells it out.
func (r *registry) knownFeatures() []string {
	known := r.gate.KnownFeatures()
	for name, spec := range r.gate.GetAll() {
		if spec.PreRelease == featuregate.GA && !spec.LockToDefault {
			known = append(known, fmt.Sprintf("%s=true|false (GA - default=%t)", name, spec.Default))
		}
	}
	sort.Strings(known)
	return known
}
