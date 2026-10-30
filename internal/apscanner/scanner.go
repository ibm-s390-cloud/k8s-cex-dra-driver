// Package apscanner scans the s390 AP bus to discover crypto queues (APQNs)
// and their attributes - notably online status and master-key verification
// state - for publishing as DRA ResourceSlices.
package apscanner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/sysfs"
)

// Card status values published as cex.ibm.com/card_status.
const (
	CardStatusConfigured = "configured"
	CardStatusDeconfig   = "deconfig"
	CardStatusChkstop    = "chkstop"
	CardStatusOffline    = "offline"
	CardStatusUnknown    = "unknown"
)

// Queue status values published as cex.ibm.com/queue_status.
const (
	QueueStatusOnline    = "online"
	QueueStatusOffline   = "offline"
	QueueStatusBoundVFIO = "bound-vfio"
	QueueStatusUnbound   = "unbound"
	QueueStatusUnknown   = "unknown"
)

// MKVP read-state values published as cex.ibm.com/mkvp_state.
const (
	MKVPStateKnown   = "known"
	MKVPStateCached  = "cached"
	MKVPStateUnknown = "unknown"
)

// MKVPRetryBudget is the number of consecutive sysfs read failures
// tolerated on a zcrypt-bound queue before the cached value is dropped
// and the queue transitions to "unknown". Counted in scan iterations.
const MKVPRetryBudget = 3

// zcryptDriver is the sysfs driver basename for queues bound to the
// in-kernel zcrypt stack. Only this driver populates the mkvps file.
const zcryptDriver = "cex4queue"

// vfioAPDriver is the sysfs driver basename for queues bound to vfio_ap
// for guest passthrough.
const vfioAPDriver = "vfio_ap"

// queueMKVPState is per-queue, in-memory state held by MKVPTracker.
// Not exposed externally - its derived form is the mkvp_state attribute.
type queueMKVPState struct {
	cache     map[string]string
	failCount int
}

// MKVPTracker holds the scanner's state across scan cycles: per-queue MKVP
// read provenance - the cache that bridges the zcrypt-to-vfio_ap binding flip
// and the retry budget that absorbs transient sysfs glitches - plus whether a
// scan has finished before, which is what decides a cycle's detail level.
type MKVPTracker struct {
	queues map[string]*queueMKVPState

	// firstScanDone is set once Scan has completed a cycle. A failed cycle
	// leaves it alone, so the next attempt still reports a full inventory.
	// It is read through detailLevel and never during the cycle that sets it.
	firstScanDone bool
}

// NewMKVPTracker returns an empty tracker.
func NewMKVPTracker() *MKVPTracker {
	return &MKVPTracker{queues: make(map[string]*queueMKVPState)}
}

// scanDetailLevel is the klog verbosity a repeat scan logs its per-card and
// per-queue detail at.
const scanDetailLevel klog.Level = 4

// detailLevel is the verbosity the current cycle logs per-item detail at. The
// first scan uses 0 - always on - so the startup block carries the full
// inventory of what was discovered. Later cycles re-derive those same lines
// every scan interval, so they log at scanDetailLevel and show up only when
// the operator asks for them.
func (t *MKVPTracker) detailLevel() klog.Level {
	if t.firstScanDone {
		return scanDetailLevel
	}
	return 0
}

// cardPattern matches card directories: cardXX (e.g., card00, cardff).
// The kernel formats card names as "card%02x" (ap_bus.c), so the adapter ID
// is always exactly two lowercase hex digits (8-bit APID, 0x00-0xff).
var cardPattern = regexp.MustCompile(`^card([0-9a-f]{2})$`)

// queuePattern matches AP queue directories: XX.YYYY (adapter.domain).
// The kernel formats queue names as "%02x.%04x" (ap_bus.c): adapter ID in two
// lowercase hex digits, domain in four. Both indices are emitted in lowercase
// hex only (%x), so uppercase never appears in real sysfs names.
var queuePattern = regexp.MustCompile(`^([0-9a-f]{2})\.([0-9a-f]{4})$`)

// APQueue represents a single AP queue discovered from sysfs.
type APQueue struct {
	APID        string            // Adapter ID (hex, e.g., "01")
	APQI        string            // Queue Index (hex, e.g., "0002")
	Type        string            // Card type: "cca", "ep11" or "accel"
	Generation  int               // CEX generation (e.g., 8 for CEX8)
	CardStatus  string            // Card state: "configured" | "deconfig" | "chkstop" | "offline" | "unknown"
	QueueStatus string            // Queue state: "online" | "offline" | "bound-vfio" | "unbound" | "unknown"
	Driver      string            // Raw sysfs driver basename: "vfio_ap", "cex4queue", or "" (unbound)
	MKVPs       map[string]string // Master Key Verification Patterns
	MKVPState   string            // Read provenance: "known" | "cached" | "unknown" | "" (n/a)
}

// Scan scans /sys/devices/ap/ for AP queues and returns Kubernetes devices.
// It performs a two-level scan: first card directories (cardXX), then queue
// subdirectories (XX.YYYY) within each card.
// The tracker carries MKVP cache and retry-budget state across scan cycles,
// and decides how much detail this cycle logs. See detailLevel.
func Scan(machineID string, tracker *MKVPTracker) (devicesByCard map[string][]resourceapi.Device, err error) {
	// Pin this goroutine to its OS thread for the duration of the scan.
	// The mkvps sysfs read submits a CPRB to the AP card and blocks in
	// wait_for_completion_interruptible(). If Go's async preemption signal
	// (SIGURG, sent cross-thread by the runtime's sysmon) arrives during
	// that wait it returns -ERESTARTSYS. cca_get_info / ep11_get_domain_info
	// bails out with its info struct still zero-initialised. The kernel's
	// mkvps show callback (cca_mkvps_show / ep11_mkvps_show) ignores that
	// error and walks each register, where every state-byte validity check
	// fails and the "- -" placeholder branch is emitted for every field.
	// read() returns success with this all-"- -" payload, so ParseMKVPs
	// accepts it and the published MKVP attribute set flaps between real
	// hex and empty values across scan cycles. Pinning excludes this OS
	// thread from sysmon's retake list, so no preemption signal is
	// delivered during the syscall.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Every detail line in this cycle, down to the ones the tracker emits
	// itself, reads detailLevel. The flip happens only once the cycle is
	// over so they all agree on it, and only for a cycle that finished.
	defer func() {
		if err == nil {
			tracker.firstScanDone = true
		}
	}()

	logphase.Vf(tracker.detailLevel(), logphase.ScanLoop, "Scanning: %s", sysfs.APDevicesPath)

	cardEntries, err := os.ReadDir(sysfs.APDevicesPath)
	if err != nil {
		if os.IsNotExist(err) {
			logphase.Logf(logphase.ScanLoop, "AP devices path does not exist (non-s390x?), returning empty device list")
			return nil, nil
		}
		return nil, fmt.Errorf("read AP devices directory: %w", err)
	}

	devicesByCard = make(map[string][]resourceapi.Device)
	cardsScanned, totalPublished := 0, 0

	for _, cardEntry := range cardEntries {
		devices, ok, err := scanCard(cardEntry, machineID, tracker)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if len(devices) > 0 {
			devicesByCard[cardEntry.Name()] = devices
		}
		totalPublished += len(devices)
		cardsScanned++
	}

	logphase.Vf(tracker.detailLevel(), logphase.ScanLoop, "Scan complete: %d cards scanned, %d AP queues published",
		cardsScanned, totalPublished)
	return devicesByCard, nil
}

// scanCard scans one cardXX directory and returns its queue devices. ok is
// false when the entry is not a card directory, an attribute is absent
// (ENOENT: the card vanished mid-scan on hot-unplug, or it exists but is not
// bound to zcrypt - the type attribute is created by the zcrypt driver on
// bind, so an unbound card has none), or its type string does not parse -
// those exclusions are deliberate and logged. A non-ENOENT I/O error is
// returned instead:
// dropping the card on a transient sysfs hiccup would deallocate all its
// published devices, so the caller must fail the whole cycle and keep the
// previous one. A valid card with no matching queues returns ok with no
// devices so the caller can still count it as scanned.
func scanCard(cardEntry os.DirEntry, machineID string, tracker *MKVPTracker) (devices []resourceapi.Device, ok bool, err error) {
	if !cardEntry.IsDir() || cardPattern.FindStringSubmatch(cardEntry.Name()) == nil {
		return nil, false, nil
	}

	cardPath := filepath.Join(sysfs.APDevicesPath, cardEntry.Name())
	cardType, generation, err := parseCardType(cardPath)
	if err != nil {
		if readFailed(err) {
			return nil, false, fmt.Errorf("parse card type of %s: %w", cardEntry.Name(), err)
		}
		logphase.Logf(logphase.ScanLoop, "Warning: skipping %s: %v", cardEntry.Name(), err)
		return nil, false, nil
	}

	queueEntries, err := os.ReadDir(cardPath)
	if err != nil {
		if readFailed(err) {
			return nil, false, fmt.Errorf("read card %s: %w", cardEntry.Name(), err)
		}
		logphase.Logf(logphase.ScanLoop, "Warning: cannot read %s: %v", cardEntry.Name(), err)
		return nil, false, nil
	}

	cardStatus := readCardStatus(cardPath)

	for _, queueEntry := range queueEntries {
		device, ok := scanQueue(cardPath, queueEntry, cardType, generation, cardStatus, machineID, tracker)
		if !ok {
			continue
		}
		devices = append(devices, device)
	}

	logphase.Vf(tracker.detailLevel(), logphase.ScanLoop, "Scanning %s (type=%s, gen=%d, card_status=%s): %d queues",
		cardEntry.Name(), cardType, generation, cardStatus, len(devices))
	return devices, true, nil
}

// readCardStatus derives cex.ibm.com/card_status from the card's config and
// chkstop sysfs attributes plus, on a configured card, the zcrypt-layer
// online attribute (the kernel's ac->config && !ac->chkstop && zc->online
// composite, admin-writable via chzcrypt). config == 0 is tested first, so a
// card reading config == 0 and chkstop == 1 at once publishes "deconfig",
// which is what lszcrypt reports for it too. The online file exists only
// while a zcrypt card driver is bound. Its absence means configured, not a
// failure. A read failure yields "unknown" for this cycle only: the device
// stays published and the next scan re-reads.
func readCardStatus(cardPath string) string {
	config, err := sysfs.ReadFile(filepath.Join(cardPath, "config"))
	if err != nil {
		return CardStatusUnknown
	}
	chkstop, err := sysfs.ReadFile(filepath.Join(cardPath, "chkstop"))
	if err != nil {
		return CardStatusUnknown
	}
	switch {
	case strings.TrimSpace(config) == "0":
		return CardStatusDeconfig
	case strings.TrimSpace(chkstop) == "1":
		return CardStatusChkstop
	case strings.TrimSpace(config) == "1":
		online, err := sysfs.ReadFile(filepath.Join(cardPath, "online"))
		if err != nil {
			if readFailed(err) {
				return CardStatusUnknown
			}
			return CardStatusConfigured
		}
		switch strings.TrimSpace(online) {
		case "1":
			return CardStatusConfigured
		case "0":
			return CardStatusOffline
		default:
			return CardStatusUnknown
		}
	default:
		return CardStatusUnknown
	}
}

// readQueueStatus derives cex.ibm.com/queue_status from the driver binding
// and, for zcrypt-bound queues, the zcrypt-layer online attribute (the
// kernel's aq->config && !aq->chkstop && zq->online composite). A vfio_ap
// binding exposes no online flag - the binding itself is the state. A driver
// name outside the two known ones yields "unknown" rather than guessing its
// semantics.
func readQueueStatus(queuePath, driver string) string {
	switch driver {
	case "":
		return QueueStatusUnbound
	case vfioAPDriver:
		return QueueStatusBoundVFIO
	case zcryptDriver:
		online, err := sysfs.ReadFile(filepath.Join(queuePath, "online"))
		if err != nil {
			return QueueStatusUnknown
		}
		switch strings.TrimSpace(online) {
		case "1":
			return QueueStatusOnline
		case "0":
			return QueueStatusOffline
		default:
			return QueueStatusUnknown
		}
	default:
		return QueueStatusUnknown
	}
}

// readFailed reports whether err is an I/O failure other than ENOENT. ENOENT
// means the sysfs node does not exist - the card was hot-unplugged mid-scan
// or never had the attribute (not bound to zcrypt) - so excluding it is
// correct. Anything else (EIO, EACCES, ...) is a hiccup the next cycle may
// not see, so it must not silently unpublish devices. ENODEV (removal racing
// between open and read) deliberately falls into the fatal path too: the
// cycle fails, and the next one no longer lists the card. Pure parse errors
// carry no PathError and report false.
func readFailed(err error) bool {
	var pathErr *fs.PathError
	return errors.As(err, &pathErr) && !errors.Is(err, fs.ErrNotExist)
}

// scanQueue parses one queue directory entry and converts it to a Device.
// Returns (_, false) when the entry is not a matching queue directory.
func scanQueue(cardPath string, queueEntry os.DirEntry, cardType string, generation int, cardStatus string, machineID string, tracker *MKVPTracker) (resourceapi.Device, bool) {
	if !queueEntry.IsDir() {
		return resourceapi.Device{}, false
	}
	queueMatch := queuePattern.FindStringSubmatch(queueEntry.Name())
	if queueMatch == nil {
		return resourceapi.Device{}, false
	}

	queuePath := filepath.Join(cardPath, queueEntry.Name())
	queue := parseAPQueueDir(queuePath, queueMatch[1], queueMatch[2], cardType, generation, cardStatus, tracker)
	logphase.Vf(tracker.detailLevel(), logphase.ScanLoop, "Found AP queue: %s.%s (type=%s, gen=%d, driver=%s, card_status=%s, queue_status=%s)",
		queue.APID, queue.APQI, queue.Type, queue.Generation, queue.Driver, queue.CardStatus, queue.QueueStatus)
	return QueueToDevice(queue, machineID), true
}

// readQueueDriver reads the driver symlink for an AP queue directory and
// returns the raw sysfs driver basename ("vfio_ap", "cex4queue", or any
// future name), or "" if the symlink is absent, meaning the queue is bound
// to no driver. Any other read failure also yields "" but is logged, so it
// does not silently publish as an unbound queue.
func readQueueDriver(queuePath string) string {
	target, err := os.Readlink(filepath.Join(queuePath, "driver"))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		logphase.Warnf(logphase.ScanLoop, "read driver symlink of %s: %v", queuePath, err)
		return ""
	}
	return filepath.Base(target)
}

// fileExists returns true if the file at path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyMKVPs returns a shallow copy of a string map.
func copyMKVPs(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Evaluate determines the MKVP map and read-state for a single queue
// based on the current driver binding, the presence/readability of the
// mkvps file, and the tracker's cached state. It mutates tracker state
// (cache and failCount) as a side effect of each scan.
//
// Card types without master keys ("accel") short-circuit to ("known", empty)
// so accel queues remain allocatable under future webhook enforcement.
func (t *MKVPTracker) Evaluate(apqn, driver, queuePath, cardType string) (map[string]string, string) {
	if cardType != "cca" && cardType != "ep11" {
		return map[string]string{}, MKVPStateKnown
	}

	state, ok := t.queues[apqn]
	if !ok {
		state = &queueMKVPState{}
		t.queues[apqn] = state
	}

	mkvpPath := filepath.Join(queuePath, "mkvps")
	if driver == zcryptDriver && fileExists(mkvpPath) {
		return t.readZcryptMKVPs(state, apqn, queuePath, cardType)
	}

	// File absent (vfio_ap, unbound, or zcrypt without mkvps file).
	if state.cache != nil {
		logphase.Vf(t.detailLevel(), logphase.ScanLoop, "mkvps file absent for %s, using cached values, mkvp_state=%s",
			apqn, MKVPStateCached)
		return copyMKVPs(state.cache), MKVPStateCached
	}
	logphase.Vf(t.detailLevel(), logphase.ScanLoop, "mkvps file absent for %s, no cache, mkvp_state=%s",
		apqn, MKVPStateUnknown)
	return sysfs.UnknownMKVPs(cardType), MKVPStateUnknown
}

// readZcryptMKVPs reads /sys mkvps for a queue currently bound to zcrypt
// where the mkvps file is present. It updates state.cache and state.failCount
// as side effects and returns the resolved (mkvps, mkvpState) pair.
func (t *MKVPTracker) readZcryptMKVPs(state *queueMKVPState, apqn, queuePath, cardType string) (map[string]string, string) {
	mkvps, err := sysfs.ParseMKVPs(queuePath, cardType)
	if err != nil {
		// Read failed on a zcrypt-bound queue with mkvps file present.
		if state.cache == nil {
			logphase.Warnf(logphase.ScanLoop, "read(mkvps) failed for %s: %v, no cache, mkvp_state=%s",
				apqn, err, MKVPStateUnknown)
			return sysfs.UnknownMKVPs(cardType), MKVPStateUnknown
		}
		state.failCount++
		if state.failCount <= MKVPRetryBudget {
			logphase.Warnf(logphase.ScanLoop, "read(mkvps) failed for %s: %v, using cache (fail_count=%d/%d), mkvp_state=%s",
				apqn, err, state.failCount, MKVPRetryBudget, MKVPStateCached)
			return copyMKVPs(state.cache), MKVPStateCached
		}
		logphase.Warnf(logphase.ScanLoop, "read(mkvps) failed for %s: %v, retry budget exhausted, mkvp_state=%s, cache dropped",
			apqn, err, MKVPStateUnknown)
		state.cache = nil
		return sysfs.UnknownMKVPs(cardType), MKVPStateUnknown
	}

	state.cache = copyMKVPs(mkvps)
	state.failCount = 0
	logphase.Vf(t.detailLevel(), logphase.ScanLoop, "MKVP read OK for %s, mkvp_state=%s", apqn, MKVPStateKnown)
	return mkvps, MKVPStateKnown
}

// parseAPQueueDir parses a single AP queue directory.
func parseAPQueueDir(path, apid, apqi, cardType string, generation int, cardStatus string, tracker *MKVPTracker) *APQueue {
	queue := &APQueue{
		APID:       apid,
		APQI:       apqi,
		Type:       cardType,
		Generation: generation,
		CardStatus: cardStatus,
	}

	queue.Driver = readQueueDriver(path)
	queue.QueueStatus = readQueueStatus(path, queue.Driver)

	apqn := fmt.Sprintf("%s.%s", apid, apqi)
	queue.MKVPs, queue.MKVPState = tracker.Evaluate(apqn, queue.Driver, path, cardType)

	return queue
}

// parseCardType reads the card type file and returns the type string and generation.
func parseCardType(cardPath string) (string, int, error) {
	typeContent, err := sysfs.ReadFile(filepath.Join(cardPath, "type"))
	if err != nil {
		return "", 0, fmt.Errorf("read card type: %w", err)
	}

	typeContent = strings.TrimSpace(typeContent)
	if len(typeContent) < 5 || !strings.HasPrefix(typeContent, "CEX") {
		return "", 0, fmt.Errorf("unexpected card type format: %q", typeContent)
	}

	genStr := typeContent[3 : len(typeContent)-1]
	gen, err := strconv.Atoi(genStr)
	if err != nil {
		return "", 0, fmt.Errorf("parse generation from %q: %w", typeContent, err)
	}

	typeChar := typeContent[len(typeContent)-1:]
	var cardType string
	switch typeChar {
	case "C":
		cardType = "cca"
	case "P":
		cardType = "ep11"
	case "A":
		cardType = "accel"
	default:
		return "", 0, fmt.Errorf("unknown card type character: %q", typeChar)
	}

	return cardType, gen, nil
}

// QueueToDevice converts an APQueue to a Kubernetes Device.
func QueueToDevice(queue *APQueue, machineID string) resourceapi.Device {
	name := fmt.Sprintf("%s-%s-%s", machineID, queue.APID, queue.APQI)

	attrs := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)

	SetStringAttr(attrs, "cex.ibm.com/machineid", machineID)
	SetStringAttr(attrs, "cex.ibm.com/apid", queue.APID)
	SetStringAttr(attrs, "cex.ibm.com/apqi", queue.APQI)
	SetStringAttr(attrs, "cex.ibm.com/type", queue.Type)
	SetIntAttr(attrs, "cex.ibm.com/generation", int64(queue.Generation))
	SetStringAttr(attrs, "cex.ibm.com/driver", queue.Driver)
	SetStringAttr(attrs, "cex.ibm.com/card_status", queue.CardStatus)
	SetStringAttr(attrs, "cex.ibm.com/queue_status", queue.QueueStatus)

	if queue.MKVPState != "" {
		SetStringAttr(attrs, "cex.ibm.com/mkvp_state", queue.MKVPState)
	}

	for key, value := range queue.MKVPs {
		SetStringAttr(attrs, key, value)
	}

	return resourceapi.Device{
		Name:       name,
		Attributes: attrs,
	}
}

// SetStringAttr sets a string attribute on a device attribute map.
func SetStringAttr(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, name, value string) {
	v := value
	attrs[resourceapi.QualifiedName(name)] = resourceapi.DeviceAttribute{
		StringValue: &v,
	}
}

// SetIntAttr sets an integer attribute on a device attribute map.
func SetIntAttr(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, name string, value int64) {
	attrs[resourceapi.QualifiedName(name)] = resourceapi.DeviceAttribute{
		IntValue: &value,
	}
}
