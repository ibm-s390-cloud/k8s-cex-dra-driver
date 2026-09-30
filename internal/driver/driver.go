// Package driver implements the DRA kubelet plugin for CEX AP queues:
// it scans the AP bus, publishes ResourceSlices, and prepares and
// unprepares claimed devices.
package driver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"k8s-cex-dra-driver/internal/apscanner"
	"k8s-cex-dra-driver/internal/binding"
	"k8s-cex-dra-driver/internal/cdi"
	"k8s-cex-dra-driver/internal/devicestate"
	"k8s-cex-dra-driver/internal/health"
	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/metadata"
	"k8s-cex-dra-driver/internal/sysfs"
)

// scanDetailLevel is the klog verbosity the per-cycle scan lines log at. It
// matches the level the scanner uses for its own per-card detail, so one -v
// setting brings back the whole picture of a repeat cycle.
const scanDetailLevel klog.Level = 4

// Config holds runtime configuration for the driver.
type Config struct {
	DriverName   string
	NodeName     string
	MachineID    string
	SysinfoPath  string
	ScanInterval time.Duration
	Client       coreclientset.Interface
	CancelCtx    func(error)
	// Heartbeat is stamped at the end of every scan cycle. Nil disables the
	// stamping, which is what a test that does not care about liveness gets.
	Heartbeat *health.Heartbeat

	KubeletRegistrarDirectoryPath string
	PluginDataDirectoryPath       string
	CDIRoot                       string
}

// Driver implements the kubeletplugin DRA interface. It keeps only what the
// scan loop and the claim handlers read. The rest of Config is consumed by
// New when it builds the device state and starts the kubelet plugin helper.
type Driver struct {
	helper       *kubeletplugin.Helper
	state        *devicestate.DeviceState
	cancelCtx    func(error)
	scanInterval time.Duration
	heartbeat    *health.Heartbeat
	machineID    string
	mkvpTracker  *apscanner.MKVPTracker
	scanTrigger  chan struct{}
	wg           sync.WaitGroup
}

// New creates and initializes a new Driver.
func New(ctx context.Context, cfg *Config) (*Driver, error) {
	d := &Driver{
		cancelCtx:    cfg.CancelCtx,
		scanInterval: cfg.ScanInterval,
		heartbeat:    cfg.Heartbeat,
	}

	// Resolve machineID
	machineID := cfg.MachineID
	if machineID == "" {
		var err error
		machineID, err = sysfs.MachineID(cfg.SysinfoPath)
		if err != nil {
			return nil, fmt.Errorf("resolve machine ID: %w", err)
		}
	}
	d.machineID = machineID
	logphase.Logf(logphase.Init, "Machine ID: %s", machineID)

	// Drain orphaned vfio-ap queues left over from a prior crash before
	// any state is built up.
	if err := binding.DrainOrphanedVFIOAP(); err != nil {
		logphase.Warnf(logphase.Preparation, "drain orphaned vfio-ap queues: %v", err)
	}

	// The file-side twin of the drain: CDI specs and KEP-5304 metadata are
	// written at Prepare and removed at Unprepare, so a claim that dies
	// without an Unprepare leaks them. Reconcile both directories against
	// the live mdev set now, while no RPC can race (the helper is not
	// started yet). Order matters: the metadata directory is Unprepare's
	// restart-surviving witness that a claim was a VM claim, and removing
	// it is safe only once the drain above has already reconciled the
	// queues that witness would have guarded.
	if err := cdi.GCStaleClaimSpecs(cfg.CDIRoot, mdev.Exists); err != nil {
		logphase.Warnf(logphase.Preparation, "GC stale CDI specs: %v", err)
	}
	if err := metadata.GCStaleClaimMetadata(cfg.PluginDataDirectoryPath, mdev.Exists); err != nil {
		logphase.Warnf(logphase.Preparation, "GC stale KEP-5304 metadata: %v", err)
	}

	// The container path's twin of both passes above: a zcrypt node and its
	// CDI spec are written as a pair, so a crash between the writes leaves one
	// without the other. Ungated like the drain: cleanup must run whatever
	// the gates say now.
	if err := devicestate.ReconcileZcryptNodes(cfg.CDIRoot); err != nil {
		logphase.Warnf(logphase.Preparation, "reconcile zcrypt nodes: %v", err)
	}

	// Initialize device state
	d.state = devicestate.New(cfg.NodeName, cfg.DriverName, cfg.CDIRoot, cfg.PluginDataDirectoryPath)

	// Initialize MKVP read-state tracker
	d.mkvpTracker = apscanner.NewMKVPTracker()

	// Initialize scan trigger channel
	d.scanTrigger = make(chan struct{}, 1)

	// Perform initial scan (all queues zcrypt-bound, real MKVPs)
	logphase.Logf(logphase.ScanLoop, "Starting initial device scan")
	devicesByCard, err := apscanner.Scan(machineID, d.mkvpTracker)
	if err != nil {
		return nil, fmt.Errorf("initial device scan: %w", err)
	}
	d.state.CompareAndUpdate(devicesByCard)
	totalDevices := 0
	for _, devs := range devicesByCard {
		totalDevices += len(devs)
	}
	logphase.Logf(logphase.ScanLoop, "Initial scan complete: %d devices found across %d cards", totalDevices, len(devicesByCard))

	// Start kubelet plugin
	helper, err := kubeletplugin.Start(
		ctx,
		d,
		kubeletplugin.KubeClient(cfg.Client),
		kubeletplugin.NodeName(cfg.NodeName),
		kubeletplugin.DriverName(cfg.DriverName),
		kubeletplugin.RegistrarDirectoryPath(cfg.KubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(cfg.PluginDataDirectoryPath),
	)
	if err != nil {
		return nil, err
	}
	d.helper = helper

	// Publish initial resources
	logphase.Logf(logphase.ScanLoop, "Publishing %d ResourceSlice(s) (update #%d)", len(devicesByCard), d.state.UpdateCount())
	if err := helper.PublishResources(ctx, d.state.DriverResources()); err != nil {
		return nil, err
	}
	logphase.Logf(logphase.ScanLoop, "ResourceSlice published successfully")

	// Start the scan loop. Shutdown waits for it through the WaitGroup.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.startScanLoop(ctx)
	}()

	return d, nil
}

// triggerScan requests an immediate scan cycle. Non-blocking. If a scan is
// already pending the request is coalesced.
func (d *Driver) triggerScan() {
	select {
	case d.scanTrigger <- struct{}{}:
	default:
		// Scan already pending
	}
}

// startScanLoop periodically scans for AP queues and publishes changes.
//
// The cadence is stated once, here, and a quiet cycle then says nothing: a
// steady-state line per cycle is noise an operator has to read past, and the
// evidence that the loop is turning is the liveness heartbeat, which a probe
// can act on. The per-cycle lines remain at detail verbosity.
func (d *Driver) startScanLoop(ctx context.Context) {
	logphase.Logf(logphase.ScanLoop, "Scanning every %s; only changes are logged", d.scanInterval)
	ticker := time.NewTicker(d.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logphase.Logf(logphase.ScanLoop, "Scan loop stopping: context cancelled")
			return
		case <-ticker.C:
			d.runScanCycle(ctx)
		case <-d.scanTrigger:
			d.runScanCycle(ctx)
			ticker.Reset(d.scanInterval)
		}
	}
}

// runScanCycle runs one cycle and records that it finished. A cycle that
// returned an error still finished, and liveness is about the loop turning,
// not about the hardware answering: the error has its own log line, and a
// fatal one reaches the exit code on its own path.
func (d *Driver) runScanCycle(ctx context.Context) {
	if err := d.scanAndPublish(ctx); err != nil {
		klog.Errorf("Scan and publish error: %v", err)
	}
	d.heartbeat.Stamp()
	logphase.Vf(scanDetailLevel, logphase.ScanLoop, "Next scan in %s", d.scanInterval)
}

// scanAndPublish performs a single scan cycle and publishes if there are changes.
func (d *Driver) scanAndPublish(ctx context.Context) error {
	devicesByCard, err := apscanner.Scan(d.machineID, d.mkvpTracker)
	if err != nil {
		return fmt.Errorf("scan AP queues: %w", err)
	}

	if d.state.CompareAndUpdate(devicesByCard) {
		logphase.Logf(logphase.ScanLoop, "Devices changed, publishing %d ResourceSlice(s) (update #%d)", len(devicesByCard), d.state.UpdateCount())
		if err := d.helper.PublishResources(ctx, d.state.DriverResources()); err != nil {
			return fmt.Errorf("publish resources: %w", err)
		}
		logphase.Logf(logphase.ScanLoop, "ResourceSlice published successfully")
	} else {
		logphase.Vf(scanDetailLevel, logphase.ScanLoop, "No device changes detected")
	}

	return nil
}

// Shutdown stops the kubelet plugin helper and waits for the scan loop to
// exit, so no publish is in flight when it returns. The scan loop only exits
// once the context passed to New is cancelled. Cancel before calling. Neither
// step can fail, so there is nothing to report back.
func (d *Driver) Shutdown() {
	d.helper.Stop()
	d.wg.Wait()
}

// PrepareResourceClaims implements the kubeletplugin DRA interface.
func (d *Driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	logphase.Logf(logphase.Allocate, "PrepareResourceClaims called: %d claims", len(claims))
	result := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		result[claim.UID] = d.prepareResourceClaim(ctx, claim)
	}

	d.triggerScan()

	return result, nil
}

func (d *Driver) prepareResourceClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	logphase.Logf(logphase.Allocate, "Preparing claim: UID=%s, Name=%s, Namespace=%s", claim.UID, claim.Name, claim.Namespace)

	if claim.Status.Allocation != nil {
		for _, result := range claim.Status.Allocation.Devices.Results {
			logphase.Logf(logphase.Allocate, "  Allocated device: Request=%s, Driver=%s, Pool=%s, Device=%s",
				result.Request, result.Driver, result.Pool, result.Device)
		}
	}

	preparedDevices, err := d.state.Prepare(ctx, claim)
	if err != nil {
		logphase.Logf(logphase.Preparation, "Error preparing devices for claim %s: %v", claim.UID, err)
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
	}

	var prepared []kubeletplugin.Device
	for _, pd := range preparedDevices {
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     pd.RequestNames,
			PoolName:     pd.PoolName,
			DeviceName:   pd.DeviceName,
			CDIDeviceIDs: pd.CdiDeviceIds,
		})
	}

	logphase.Logf(logphase.Preparation, "Claim %s prepared successfully: %d devices", claim.UID, len(prepared))
	return kubeletplugin.PrepareResult{Devices: prepared}
}

// UnprepareResourceClaims implements the kubeletplugin DRA interface.
func (d *Driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	logphase.Logf(logphase.Allocate, "UnprepareResourceClaims called: %d claims", len(claims))
	result := make(map[types.UID]error)

	for _, claim := range claims {
		result[claim.UID] = d.unprepareResourceClaim(ctx, claim)
	}

	d.triggerScan()

	return result, nil
}

func (d *Driver) unprepareResourceClaim(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	logphase.Logf(logphase.Allocate, "Unpreparing claim: UID=%s, Name=%s, Namespace=%s", claim.UID, claim.Name, claim.Namespace)

	if err := d.state.Unprepare(ctx, string(claim.UID)); err != nil {
		logphase.Logf(logphase.Preparation, "Error unpreparing claim %s: %v", claim.UID, err)
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claim.UID, err)
	}

	logphase.Logf(logphase.Preparation, "Claim %s unprepared successfully", claim.UID)
	return nil
}

// HandleError implements the kubeletplugin DRA interface.
func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) && d.cancelCtx != nil {
		d.cancelCtx(fmt.Errorf("fatal background error: %w", err))
	}
}

// healthResendInterval is how often the health stream repeats its snapshot.
// The kubelet ages a device out to unknown when no report arrives within the
// health check timeout, 30 seconds by default, and the helper checks for
// staleness every 10 seconds. The resend cadence is therefore a constant well
// under that timeout rather than anything derived from --scan-interval, which
// an operator can raise past the timeout.
const healthResendInterval = 10 * time.Second

// WatchHealthStatus implements the kubeletplugin DRA interface. The driver
// does not determine per-device health yet, so it reports every published
// device as Unknown - the documented value for health that cannot be
// determined - and holds the stream open, repeating the snapshot on its own
// ticker so the kubelet never sees the reports go stale. Deriving real health
// from the scanned queue state is a separate change.
//
// The alternative is to decline with ErrHealthNotSupported, which the helper
// turns into a gRPC Unimplemented status. A 1.37 kubelet then stops asking,
// but a 1.36 kubelet redials every five seconds, and the helper logs every
// closed stream at error level: a healthy driver produces two error lines
// every five seconds, forever. Reporting Unknown says the same thing about
// the driver's knowledge and says it quietly.
func (d *Driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	ticker := time.NewTicker(healthResendInterval)
	defer ticker.Stop()

	for {
		select {
		case reports <- d.unknownHealthReport():
		case <-ctx.Done():
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// unknownHealthReport snapshots every currently published device at Unknown.
// The identities come from the same DriverResources the ResourceSlices are
// built from, because the kubelet matches health to an allocation by exact
// pool and device name.
func (d *Driver) unknownHealthReport() kubeletplugin.DeviceHealthReport {
	resources := d.state.DriverResources()
	now := time.Now()

	var report kubeletplugin.DeviceHealthReport
	for poolName, pool := range resources.Pools {
		for _, slice := range pool.Slices {
			for _, device := range slice.Devices {
				report.Devices = append(report.Devices, kubeletplugin.DeviceHealth{
					PoolName:    poolName,
					DeviceName:  device.Name,
					Health:      kubeletplugin.HealthStatusUnknown,
					LastUpdated: now,
				})
			}
		}
	}
	return report
}
