package shadowsysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost builds a host AP sysfs with the given cards, each carrying the
// given domains, and points the package at it and at a fresh BaseDir.
func fakeHost(t *testing.T, cards map[int][]int) {
	t.Helper()
	root := t.TempDir()
	bus := filepath.Join(root, "bus", "ap")
	devs := filepath.Join(root, "devices", "ap")
	mustWrite(t, filepath.Join(bus, "ap_interrupts"), "1\n")
	mustWrite(t, filepath.Join(bus, "ap_max_domain_id"), "84\n")
	mustWrite(t, filepath.Join(bus, "poll_thread"), "0\n")
	mustWrite(t, filepath.Join(bus, "poll_timeout"), "250000\n")
	mustWrite(t, filepath.Join(bus, "ap_max_adapter_id"), "255\n")
	// Host-wide masks the shadow must not leak.
	mustWrite(t, filepath.Join(bus, "ap_adapter_mask"), "0x"+strings.Repeat("f", 64)+"\n")
	for c, doms := range cards {
		card := filepath.Join(devs, cardName(c))
		mustWrite(t, filepath.Join(card, "hwtype"), "13\n")
		mustWrite(t, filepath.Join(card, "raw_hwtype"), "13\n")
		mustWrite(t, filepath.Join(card, "type"), "CEX6C\n")
		mustWrite(t, filepath.Join(card, "ap_functions"), "0x92000000\n")
		mustWrite(t, filepath.Join(card, "depth"), "8\n")
		mustWrite(t, filepath.Join(card, "serialnr"), "93AADFK7\n")
		mustWrite(t, filepath.Join(card, "online"), "0\n") // must come out 1
		for _, d := range doms {
			q := filepath.Join(card, queueName(c, d))
			mustWrite(t, filepath.Join(q, "interrupt"), "Interrupts enabled\n")
			mustWrite(t, filepath.Join(q, "reset"), "No Reset Pending\n")
			mustWrite(t, filepath.Join(q, "mkvps"), "AES CUR: valid 0x0123456789abcdef\n")
		}
	}
	t.Cleanup(restore(&HostBusDir, bus))
	t.Cleanup(restore(&HostDevicesDir, devs))
	t.Cleanup(restore(&BaseDir, filepath.Join(t.TempDir(), "shadow")))
}

func cardName(c int) string     { return fmt.Sprintf("card%02x", c) }
func queueName(c, d int) string { return fmt.Sprintf("%02x.%04x", c, d) }

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func restore(p *string, v string) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func noSELinux(t *testing.T) {
	t.Helper()
	old := selinuxEnabled
	selinuxEnabled = func(string) bool { return false }
	t.Cleanup(func() { selinuxEnabled = old })
}

// TestBuildShowsOnlyTheGrid builds a tree for one queue of a host with two
// cards and several domains, and checks that the container sees exactly that
// queue - through the masks, the device listing and the driver listing - and
// that every relative link resolves inside the tree.
func TestBuildShowsOnlyTheGrid(t *testing.T) {
	noSELinux(t)
	fakeHost(t, map[int][]int{0x06: {0, 1, 5}, 0x07: {0, 5}})

	tree, err := Build("uid-1", Grid{Adapters: []int{6}, UsageDomains: []int{5}, ControlDomains: []int{5}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tree.BusDir != filepath.Join(Path("uid-1"), "bus", "ap") || tree.DevicesDir != filepath.Join(Path("uid-1"), "devices", "ap") {
		t.Errorf("tree = %+v", tree)
	}

	wantAdapter := "0x02" + strings.Repeat("0", 62) + "\n" // bit 6
	wantDomain := "0x04" + strings.Repeat("0", 62) + "\n"  // bit 5
	for name, want := range map[string]string{
		"ap_adapter_mask":        wantAdapter,
		"ap_usage_domain_mask":   wantDomain,
		"ap_control_domain_mask": wantDomain,
		"apmask":                 "0x" + strings.Repeat("f", 64) + "\n",
		"ap_domain":              "5\n",
		"ap_max_domain_id":       "84\n",
		"ap_max_adapter_id":      "255\n",
	} {
		if got := read(t, filepath.Join(tree.BusDir, name)); got != want {
			t.Errorf("bus/ap/%s = %q, want %q", name, got, want)
		}
	}

	entries, err := os.ReadDir(filepath.Join(tree.BusDir, "devices"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "06.0005,card06" {
		t.Errorf("bus/ap/devices = %v, want [06.0005 card06]", names)
	}

	// Links resolve inside the tree, the way they will in the container.
	for link, want := range map[string]string{
		filepath.Join(tree.BusDir, "devices", "card06"):                  filepath.Join(tree.DevicesDir, "card06"),
		filepath.Join(tree.BusDir, "devices", "06.0005"):                 filepath.Join(tree.DevicesDir, "card06", "06.0005"),
		filepath.Join(tree.BusDir, "drivers", "cex4card", "card06"):      filepath.Join(tree.DevicesDir, "card06"),
		filepath.Join(tree.BusDir, "drivers", "cex4queue", "06.0005"):    filepath.Join(tree.DevicesDir, "card06", "06.0005"),
		filepath.Join(tree.DevicesDir, "card06", "driver"):               filepath.Join(tree.BusDir, "drivers", "cex4card"),
		filepath.Join(tree.DevicesDir, "card06", "06.0005", "subsystem"): tree.BusDir,
		filepath.Join(tree.DevicesDir, "card06", "06.0005", "driver"):    filepath.Join(tree.BusDir, "drivers", "cex4queue"),
	} {
		got, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Errorf("resolve %s: %v", link, err)
			continue
		}
		wantReal, _ := filepath.EvalSymlinks(want)
		if got != wantReal {
			t.Errorf("%s -> %s, want %s", link, got, wantReal)
		}
	}

	card := filepath.Join(tree.DevicesDir, "card06")
	if got := read(t, filepath.Join(card, "type")); got != "CEX6C\n" {
		t.Errorf("card type = %q", got)
	}
	if got := read(t, filepath.Join(card, "online")); got != "1\n" {
		t.Errorf("card online = %q, want the fixed 1", got)
	}
	if got := read(t, filepath.Join(card, "06.0005", "mkvps")); !strings.Contains(got, "AES CUR") {
		t.Errorf("mkvps not copied: %q", got)
	}
	if _, err := os.Stat(filepath.Join(card, "06.0000")); !os.IsNotExist(err) {
		t.Error("unclaimed queue 06.0000 leaked into the tree")
	}
	if _, err := os.Stat(filepath.Join(tree.DevicesDir, "card07")); !os.IsNotExist(err) {
		t.Error("unclaimed card07 leaked into the tree")
	}
	info, err := os.Stat(filepath.Join(card, "type"))
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("file mode = %v, want 0444", info.Mode().Perm())
	}
}

// TestBuildGrid covers a two-by-two grid: all four queues present, ap_domain
// the lowest domain.
func TestBuildGrid(t *testing.T) {
	noSELinux(t)
	fakeHost(t, map[int][]int{0x01: {2, 3}, 0x02: {2, 3}})
	tree, err := Build("uid-grid", Grid{Adapters: []int{2, 1}, UsageDomains: []int{3, 2}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, q := range []string{"card01/01.0002", "card01/01.0003", "card02/02.0002", "card02/02.0003"} {
		if _, err := os.Stat(filepath.Join(tree.DevicesDir, q)); err != nil {
			t.Errorf("queue %s missing: %v", q, err)
		}
	}
	if got := read(t, filepath.Join(tree.BusDir, "ap_domain")); got != "2\n" {
		t.Errorf("ap_domain = %q, want 2", got)
	}
	if got := read(t, filepath.Join(tree.BusDir, "ap_control_domain_mask")); got != "0x"+strings.Repeat("0", 64)+"\n" {
		t.Errorf("control mask = %q, want empty", got)
	}
}

// TestBuildIsIdempotent pins that a complete tree is reused, never rebuilt
// under a running container, and that a half-built one is replaced.
func TestBuildIsIdempotent(t *testing.T) {
	noSELinux(t)
	fakeHost(t, map[int][]int{0x06: {0}})
	g := Grid{Adapters: []int{6}, UsageDomains: []int{0}}
	tree, err := Build("uid-2", g)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(tree.Dir, "sentinel")
	mustWrite(t, sentinel, "x")
	if _, err := Build("uid-2", g); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("complete tree was rebuilt")
	}

	// Incomplete: no marker. Build replaces it.
	if err := os.Remove(filepath.Join(tree.Dir, completeMarker)); err != nil {
		t.Fatal(err)
	}
	if _, err := Build("uid-2", g); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Error("incomplete tree was reused")
	}
}

// TestBuildMissingQueueFails leaves nothing behind when the host lacks a
// claimed queue.
func TestBuildMissingQueueFails(t *testing.T) {
	noSELinux(t)
	fakeHost(t, map[int][]int{0x06: {0}})
	if _, err := Build("uid-3", Grid{Adapters: []int{6}, UsageDomains: []int{1}}); err == nil {
		t.Fatal("Build succeeded for a queue the host does not have")
	}
	entries, _ := os.ReadDir(BaseDir)
	if len(entries) != 0 {
		t.Errorf("leftovers after failed build: %v", entries)
	}
}

func TestRemoveAndList(t *testing.T) {
	noSELinux(t)
	fakeHost(t, map[int][]int{0x06: {0}})
	g := Grid{Adapters: []int{6}, UsageDomains: []int{0}}
	for _, uid := range []string{"a", "b"} {
		if _, err := Build(uid, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(BaseDir, tmpPrefix+"c"), 0o755); err != nil {
		t.Fatal(err)
	}
	uids, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(uids, ",") != "a,b" {
		t.Errorf("List = %v, want [a b]", uids)
	}
	if _, err := os.Stat(filepath.Join(BaseDir, tmpPrefix+"c")); !os.IsNotExist(err) {
		t.Error("List kept a crash leftover build directory")
	}
	if err := Remove("a"); err != nil {
		t.Fatal(err)
	}
	if err := Remove("a"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	if uids, _ := List(); strings.Join(uids, ",") != "b" {
		t.Errorf("List after Remove = %v", uids)
	}
}

func TestListMissingBase(t *testing.T) {
	t.Cleanup(restore(&BaseDir, filepath.Join(t.TempDir(), "absent")))
	uids, err := List()
	if err != nil || len(uids) != 0 {
		t.Errorf("List = %v, %v; want nothing", uids, err)
	}
}

func TestInvalidUID(t *testing.T) {
	for _, uid := range []string{"", ".", "..", "../x", "a/b", ".tmp-x"} {
		if _, err := Build(uid, Grid{Adapters: []int{1}, UsageDomains: []int{1}}); err == nil {
			t.Errorf("Build(%q) accepted", uid)
		}
		if err := Remove(uid); err == nil {
			t.Errorf("Remove(%q) accepted", uid)
		}
	}
}

func TestFormatMask(t *testing.T) {
	if got := formatMask([]int{0, 255}, false); got != "0x80"+strings.Repeat("0", 60)+"01\n" {
		t.Errorf("formatMask = %q", got)
	}
	if got := formatMask(nil, false); got != "0x"+strings.Repeat("0", 64)+"\n" {
		t.Errorf("empty mask = %q", got)
	}
}
