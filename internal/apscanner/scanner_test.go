package apscanner

import (
	"os"
	"path/filepath"
	"testing"

	"k8s-cex-dra-driver/internal/sysfs"
)

const sampleCCAMKVPs = `AES CUR: valid 0x0123456789abcdef
AES OLD: valid 0xfedcba9876543210
AES NEW: empty -
APKA CUR: valid 0xaabbccddeeff0011
APKA OLD: empty -
APKA NEW: empty -
ASYM CUR: valid 0x00112233445566778899aabbccddeeff
ASYM OLD: empty -
ASYM NEW: empty -
`

// queueFixture builds a fake queue directory tree under root. If mkvps
// is non-nil, an mkvps file with that content is created. The returned
// path is the queue dir to pass into Evaluate.
func queueFixture(t *testing.T, root string, mkvps *string) string {
	t.Helper()
	dir := filepath.Join(root, "01.0002")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if mkvps != nil {
		if err := os.WriteFile(filepath.Join(dir, "mkvps"), []byte(*mkvps), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func unreadableMKVPs(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "01.0002")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Make a directory at the mkvps path so ReadFile errors but Stat succeeds.
	if err := os.MkdirAll(filepath.Join(dir, "mkvps"), 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEvaluate_KnownOnFirstSuccess(t *testing.T) {
	tracker := NewMKVPTracker()
	body := sampleCCAMKVPs
	dir := queueFixture(t, t.TempDir(), &body)

	mkvps, state := tracker.Evaluate("01.0002", "cex4queue", dir, "cca")
	if state != MKVPStateKnown {
		t.Fatalf("state = %q, want %q", state, MKVPStateKnown)
	}
	if got := mkvps["cex.cca.domain.ibm.com/mkvp_aes_cur"]; got != "0123456789abcdef" {
		t.Errorf("aes_cur = %q, want %q", got, "0123456789abcdef")
	}
	if got := mkvps["cex.cca.domain.ibm.com/mkvp_aes_new"]; got != "" {
		t.Errorf("aes_new = %q, want empty (no 0x prefix)", got)
	}
}

func TestEvaluate_CachedAfterBindingFlip(t *testing.T) {
	tracker := NewMKVPTracker()
	body := sampleCCAMKVPs
	root := t.TempDir()
	dir := queueFixture(t, root, &body)

	if _, s := tracker.Evaluate("01.0002", "cex4queue", dir, "cca"); s != MKVPStateKnown {
		t.Fatalf("seed: state = %q, want known", s)
	}

	// Simulate vfio_ap binding flip: file removed, driver changes.
	if err := os.Remove(filepath.Join(dir, "mkvps")); err != nil {
		t.Fatal(err)
	}

	mkvps, state := tracker.Evaluate("01.0002", "vfio_ap", dir, "cca")
	if state != MKVPStateCached {
		t.Fatalf("state = %q, want %q", state, MKVPStateCached)
	}
	if got := mkvps["cex.cca.domain.ibm.com/mkvp_aes_cur"]; got != "0123456789abcdef" {
		t.Errorf("aes_cur = %q, want cached %q", got, "0123456789abcdef")
	}
}

func TestEvaluate_UnknownNoCacheFileAbsent(t *testing.T) {
	tracker := NewMKVPTracker()
	dir := queueFixture(t, t.TempDir(), nil)

	mkvps, state := tracker.Evaluate("01.0002", "vfio_ap", dir, "cca")
	if state != MKVPStateUnknown {
		t.Fatalf("state = %q, want %q", state, MKVPStateUnknown)
	}
	if got := mkvps["cex.cca.domain.ibm.com/mkvp_aes_cur"]; got != sysfs.MKVPUnknownValue {
		t.Errorf("aes_cur = %q, want %q", got, sysfs.MKVPUnknownValue)
	}
}

func TestEvaluate_RetryBudgetExhausted(t *testing.T) {
	tracker := NewMKVPTracker()
	root := t.TempDir()
	body := sampleCCAMKVPs

	// Seed cache with a successful read.
	dirOK := queueFixture(t, root, &body)
	if _, s := tracker.Evaluate("01.0002", "cex4queue", dirOK, "cca"); s != MKVPStateKnown {
		t.Fatalf("seed: state = %q, want known", s)
	}

	// Replace with an unreadable mkvps node (Stat ok, Read fails).
	if err := os.Remove(filepath.Join(dirOK, "mkvps")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dirOK, "mkvps"), 0o750); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= MKVPRetryBudget; i++ {
		_, s := tracker.Evaluate("01.0002", "cex4queue", dirOK, "cca")
		if s != MKVPStateCached {
			t.Fatalf("attempt %d: state = %q, want %q", i, s, MKVPStateCached)
		}
	}
	mkvps, s := tracker.Evaluate("01.0002", "cex4queue", dirOK, "cca")
	if s != MKVPStateUnknown {
		t.Fatalf("after budget: state = %q, want %q", s, MKVPStateUnknown)
	}
	if got := mkvps["cex.cca.domain.ibm.com/mkvp_aes_cur"]; got != sysfs.MKVPUnknownValue {
		t.Errorf("aes_cur = %q, want %q", got, sysfs.MKVPUnknownValue)
	}
}

// TestEvaluate_PlaceholderReadUsesCache pins that the kernel's placeholder
// payload - a successful read(2) whose card query failed - runs the failure
// ladder instead of overwriting the cache with empty values as known.
func TestEvaluate_PlaceholderReadUsesCache(t *testing.T) {
	tracker := NewMKVPTracker()
	body := sampleCCAMKVPs
	dir := queueFixture(t, t.TempDir(), &body)

	if _, s := tracker.Evaluate("01.0002", "cex4queue", dir, "cca"); s != MKVPStateKnown {
		t.Fatalf("seed: state = %q, want known", s)
	}

	placeholder := "AES NEW: - -\nAES CUR: - -\nAES OLD: - -\nAPKA NEW: - -\nAPKA CUR: - -\nAPKA OLD: - -\nASYM NEW: - -\nASYM CUR: - -\nASYM OLD: - -\n"
	if err := os.WriteFile(filepath.Join(dir, "mkvps"), []byte(placeholder), 0o644); err != nil {
		t.Fatal(err)
	}

	mkvps, state := tracker.Evaluate("01.0002", "cex4queue", dir, "cca")
	if state != MKVPStateCached {
		t.Fatalf("state = %q, want %q", state, MKVPStateCached)
	}
	if got := mkvps["cex.cca.domain.ibm.com/mkvp_aes_cur"]; got != "0123456789abcdef" {
		t.Errorf("aes_cur = %q, want the cached %q", got, "0123456789abcdef")
	}
}

func TestEvaluate_FailCountResetsOnSuccess(t *testing.T) {
	tracker := NewMKVPTracker()
	root := t.TempDir()
	body := sampleCCAMKVPs
	dir := queueFixture(t, root, &body)

	// Seed.
	tracker.Evaluate("01.0002", "cex4queue", dir, "cca")

	// Two failures.
	if err := os.Remove(filepath.Join(dir, "mkvps")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "mkvps"), 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tracker.Evaluate("01.0002", "cex4queue", dir, "cca")
	}

	// Restore good file. Counter must reset.
	if err := os.RemoveAll(filepath.Join(dir, "mkvps")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mkvps"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, s := tracker.Evaluate("01.0002", "cex4queue", dir, "cca"); s != MKVPStateKnown {
		t.Fatalf("after recovery: state = %q, want known", s)
	}

	// Now break the file again. Should still survive 3 failures (counter was reset).
	if err := os.Remove(filepath.Join(dir, "mkvps")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "mkvps"), 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MKVPRetryBudget; i++ {
		_, s := tracker.Evaluate("01.0002", "cex4queue", dir, "cca")
		if s != MKVPStateCached {
			t.Fatalf("post-reset attempt %d: state = %q, want cached", i, s)
		}
	}
}

func TestEvaluate_NewVFIOAPQueueNoCache(t *testing.T) {
	tracker := NewMKVPTracker()
	dir := queueFixture(t, t.TempDir(), nil)

	_, state := tracker.Evaluate("01.0002", "vfio_ap", dir, "ep11")
	if state != MKVPStateUnknown {
		t.Fatalf("state = %q, want %q", state, MKVPStateUnknown)
	}
}

func TestEvaluate_AccelKnownEmpty(t *testing.T) {
	tracker := NewMKVPTracker()
	dir := queueFixture(t, t.TempDir(), nil)

	mkvps, state := tracker.Evaluate("01.0002", "cex4queue", dir, "accel")
	if state != MKVPStateKnown {
		t.Fatalf("state = %q, want %q", state, MKVPStateKnown)
	}
	if len(mkvps) != 0 {
		t.Errorf("mkvps = %v, want empty", mkvps)
	}
}

func TestEvaluate_ReadFailNoCache(t *testing.T) {
	tracker := NewMKVPTracker()
	dir := unreadableMKVPs(t, t.TempDir())

	mkvps, state := tracker.Evaluate("01.0002", "cex4queue", dir, "ep11")
	if state != MKVPStateUnknown {
		t.Fatalf("state = %q, want %q", state, MKVPStateUnknown)
	}
	if got := mkvps["cex.ep11.domain.ibm.com/mkvp_wk_cur"]; got != sysfs.MKVPUnknownValue {
		t.Errorf("wk_cur = %q, want %q", got, sysfs.MKVPUnknownValue)
	}
}

func TestQueueToDevice_PublishesMKVPState(t *testing.T) {
	q := &APQueue{
		APID: "01", APQI: "0002", Type: "cca", Generation: 8,
		CardStatus: CardStatusConfigured, QueueStatus: QueueStatusOnline, Driver: "cex4queue",
		MKVPs:     map[string]string{"cex.cca.domain.ibm.com/mkvp_aes_cur": "deadbeef"},
		MKVPState: MKVPStateKnown,
	}
	dev := QueueToDevice(q, "ibm-3931-abc")
	attr, ok := dev.Attributes["cex.ibm.com/mkvp_state"]
	if !ok {
		t.Fatal("mkvp_state attribute missing")
	}
	if attr.StringValue == nil {
		t.Errorf("mkvp_state = nil, want %q", MKVPStateKnown)
	} else if *attr.StringValue != MKVPStateKnown {
		t.Errorf("mkvp_state = %q, want %q", *attr.StringValue, MKVPStateKnown)
	}
}

// cardFixture builds a fake cardXX directory under root with the given type
// file content. A nil typeContent creates the card without a type file. A
// special marker is not needed for read errors - tests create a directory at
// the type path instead.
func cardFixture(t *testing.T, root, name string, typeContent *string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if typeContent != nil {
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte(*typeContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// cardEntry lists root and returns the DirEntry for the card04 fixture.
func cardEntry(t *testing.T, root string) os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "card04" {
			return e
		}
	}
	t.Fatalf("card04 not found in %s", root)
	return nil
}

func withAPDevicesPath(t *testing.T, root string) {
	t.Helper()
	prev := sysfs.APDevicesPath
	sysfs.APDevicesPath = root
	t.Cleanup(func() { sysfs.APDevicesPath = prev })
}

func TestScanCard_TypeReadErrorFailsScan(t *testing.T) {
	root := t.TempDir()
	cardFixture(t, root, "card04", nil)
	// Directory at the type path: ReadFile fails with a non-ENOENT PathError.
	if err := os.MkdirAll(filepath.Join(root, "card04", "type"), 0o750); err != nil {
		t.Fatal(err)
	}
	withAPDevicesPath(t, root)

	_, ok, err := scanCard(cardEntry(t, root), "machine", NewMKVPTracker())
	if err == nil {
		t.Fatal("err = nil, want read failure so the whole scan cycle fails")
	}
	if ok {
		t.Error("ok = true, want false on read failure")
	}
}

func TestScanCard_TypeMissingSkips(t *testing.T) {
	root := t.TempDir()
	cardFixture(t, root, "card04", nil)
	withAPDevicesPath(t, root)

	_, ok, err := scanCard(cardEntry(t, root), "machine", NewMKVPTracker())
	if err != nil {
		t.Fatalf("err = %v, want nil: a vanished card is excluded, not fatal", err)
	}
	if ok {
		t.Error("ok = true, want false for a card without a type file")
	}
}

func TestScanCard_TypeGarbageSkips(t *testing.T) {
	root := t.TempDir()
	garbage := "BOGUS"
	cardFixture(t, root, "card04", &garbage)
	withAPDevicesPath(t, root)

	_, ok, err := scanCard(cardEntry(t, root), "machine", NewMKVPTracker())
	if err != nil {
		t.Fatalf("err = %v, want nil: a malformed type string is excluded, not fatal", err)
	}
	if ok {
		t.Error("ok = true, want false for a malformed type string")
	}
}

func TestScanCard_ValidCardNoQueues(t *testing.T) {
	root := t.TempDir()
	cexType := "CEX8C"
	cardFixture(t, root, "card04", &cexType)
	withAPDevicesPath(t, root)

	devices, ok, err := scanCard(cardEntry(t, root), "machine", NewMKVPTracker())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Error("ok = false, want true for a valid card")
	}
	if len(devices) != 0 {
		t.Errorf("devices = %d, want 0 for a card without queues", len(devices))
	}
}

// writeCardAttr writes one sysfs-style attribute file into a fixture dir.
func writeCardAttr(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadCardStatus(t *testing.T) {
	cases := []struct {
		name    string
		config  *string
		chkstop *string
		online  *string
		want    string
	}{
		{"configured no online file", ptr("1\n"), ptr("0\n"), nil, CardStatusConfigured},
		{"configured online", ptr("1\n"), ptr("0\n"), ptr("1\n"), CardStatusConfigured},
		{"soft-offline", ptr("1\n"), ptr("0\n"), ptr("0\n"), CardStatusOffline},
		{"deconfig", ptr("0\n"), ptr("0\n"), nil, CardStatusDeconfig},
		{"deconfig wins over online", ptr("0\n"), ptr("0\n"), ptr("1\n"), CardStatusDeconfig},
		{"chkstop", ptr("1\n"), ptr("1\n"), nil, CardStatusChkstop},
		{"chkstop wins over online", ptr("1\n"), ptr("1\n"), ptr("0\n"), CardStatusChkstop},
		{"deconfig wins over chkstop", ptr("0\n"), ptr("1\n"), nil, CardStatusDeconfig},
		{"config missing", nil, ptr("0\n"), nil, CardStatusUnknown},
		{"chkstop missing", ptr("1\n"), nil, nil, CardStatusUnknown},
		{"garbage content", ptr("maybe\n"), ptr("0\n"), nil, CardStatusUnknown},
		{"garbage online", ptr("1\n"), ptr("0\n"), ptr("x\n"), CardStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.config != nil {
				writeCardAttr(t, dir, "config", *tc.config)
			}
			if tc.chkstop != nil {
				writeCardAttr(t, dir, "chkstop", *tc.chkstop)
			}
			if tc.online != nil {
				writeCardAttr(t, dir, "online", *tc.online)
			}
			if got := readCardStatus(dir); got != tc.want {
				t.Errorf("readCardStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadQueueStatus(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		online *string
		want   string
	}{
		{"unbound", "", nil, QueueStatusUnbound},
		{"vfio_ap", "vfio_ap", nil, QueueStatusBoundVFIO},
		{"zcrypt online", "cex4queue", ptr("1\n"), QueueStatusOnline},
		{"zcrypt offline", "cex4queue", ptr("0\n"), QueueStatusOffline},
		{"zcrypt online file missing", "cex4queue", nil, QueueStatusUnknown},
		{"zcrypt garbage online", "cex4queue", ptr("x\n"), QueueStatusUnknown},
		{"unrecognized driver", "future_drv", nil, QueueStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.online != nil {
				writeCardAttr(t, dir, "online", *tc.online)
			}
			if got := readQueueStatus(dir, tc.driver); got != tc.want {
				t.Errorf("readQueueStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestScanCard_PublishesStatusAttributes(t *testing.T) {
	root := t.TempDir()
	cexType := "CEX8C"
	cardFixture(t, root, "card04", &cexType)
	cardDir := filepath.Join(root, "card04")
	writeCardAttr(t, cardDir, "config", "1\n")
	writeCardAttr(t, cardDir, "chkstop", "0\n")
	queueDir := filepath.Join(cardDir, "04.0007")
	if err := os.MkdirAll(queueDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// The driver symlink target does not need to exist. Only its basename is read.
	if err := os.Symlink("../../../bus/ap/drivers/cex4queue", filepath.Join(queueDir, "driver")); err != nil {
		t.Fatal(err)
	}
	writeCardAttr(t, queueDir, "online", "1\n")
	withAPDevicesPath(t, root)

	devices, ok, err := scanCard(cardEntry(t, root), "machine", NewMKVPTracker())
	if err != nil || !ok {
		t.Fatalf("scanCard = (ok=%v, err=%v), want ok with no error", ok, err)
	}
	if len(devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(devices))
	}
	attrs := devices[0].Attributes
	if got := attrs["cex.ibm.com/card_status"].StringValue; got == nil || *got != CardStatusConfigured {
		t.Errorf("card_status = %v, want %q", got, CardStatusConfigured)
	}
	if got := attrs["cex.ibm.com/queue_status"].StringValue; got == nil || *got != QueueStatusOnline {
		t.Errorf("queue_status = %v, want %q", got, QueueStatusOnline)
	}
}

func TestScan_CardReadErrorFailsWholeScan(t *testing.T) {
	root := t.TempDir()
	cexType := "CEX8C"
	cardFixture(t, root, "card03", &cexType)
	cardFixture(t, root, "card04", nil)
	if err := os.MkdirAll(filepath.Join(root, "card04", "type"), 0o750); err != nil {
		t.Fatal(err)
	}
	withAPDevicesPath(t, root)

	tracker := NewMKVPTracker()
	_, err := Scan("machine", tracker)
	if err == nil {
		t.Fatal("err = nil, want the whole scan to fail on one card's read error")
	}
	if got := tracker.detailLevel(); got != 0 {
		t.Errorf("detailLevel after a failed scan = %d, want 0: no inventory was reported, so the retry is still the first scan", got)
	}
}

// TestDetailLevelDropsAfterFirstScan pins the logging contract the scanner
// owns: the startup scan reports its inventory at any verbosity, and the
// cycles that repeat it every scan interval need -v=4 to be seen.
func TestDetailLevelDropsAfterFirstScan(t *testing.T) {
	root := t.TempDir()
	cexType := "CEX8C"
	cardFixture(t, root, "card03", &cexType)
	withAPDevicesPath(t, root)

	tracker := NewMKVPTracker()
	if got := tracker.detailLevel(); got != 0 {
		t.Errorf("detailLevel before the first scan = %d, want 0 (always logged)", got)
	}

	if _, err := Scan("machine", tracker); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if got := tracker.detailLevel(); got != scanDetailLevel {
		t.Errorf("detailLevel after the first scan = %d, want %d", got, scanDetailLevel)
	}
}
