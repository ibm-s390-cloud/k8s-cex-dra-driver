package zcrypt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testName = "cexdra-0123456789abcdef01234567"
	// emptyMask is what the kernel shows for a fresh node: four 64-bit longs.
	emptyMask = "0x0000000000000000000000000000000000000000000000000000000000000000"
)

// newFakeSysfs points the package at an empty fake tree for one test: the
// class attributes exist, no node does. The kernel's side effects - a node
// directory appearing on create, disappearing on destroy - are not
// simulated, the tests check what the package writes and read what the test
// placed.
func newFakeSysfs(t *testing.T) (classDir, nodeDir, devDir string) {
	t.Helper()
	root := t.TempDir()
	classDir = filepath.Join(root, "class", "zcrypt")
	nodeDir = filepath.Join(root, "devices", "virtual", "zcrypt")
	devDir = filepath.Join(root, "dev")
	for _, d := range []string{classDir, nodeDir, devDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, attr := range []string{"create", "destroy"} {
		if err := os.WriteFile(filepath.Join(classDir, attr), nil, 0o600); err != nil {
			t.Fatalf("create %s: %v", attr, err)
		}
	}

	savedClass, savedNode, savedDev := ClassDir, NodeDir, DevDir
	ClassDir, NodeDir, DevDir = classDir, nodeDir, devDir
	t.Cleanup(func() { ClassDir, NodeDir, DevDir = savedClass, savedNode, savedDev })
	return classDir, nodeDir, devDir
}

// makeNode creates a node directory the way the kernel does after a
// successful create: all four masks present and empty.
func makeNode(t *testing.T, nodeDir, name string) string {
	t.Helper()
	dir := filepath.Join(nodeDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, attr := range []string{"apmask", "aqmask", "admask", "ioctlmask"} {
		if err := os.WriteFile(filepath.Join(dir, attr), []byte(emptyMask+"\n"), 0o600); err != nil {
			t.Fatalf("create %s: %v", attr, err)
		}
	}
	return dir
}

func readAttr(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestNodeName(t *testing.T) {
	got := NodeName("E287E0A9-5562-4F73-A845-DE277A3A0601")
	if want := "cexdra-e287e0a955624f73a845de27"; got != want {
		t.Errorf("NodeName = %q, want %q", got, want)
	}
	if len(got) != MaxNameLen {
		t.Errorf("len = %d, want %d", len(got), MaxNameLen)
	}
	if err := ValidateName(got); err != nil {
		t.Errorf("NodeName output fails ValidateName: %v", err)
	}
}

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{testName, true},
		{"cexdra-a", true},
		{"cexdra-", false},                           // prefix only
		{"cexlab", false},                            // not ours
		{"zcrypt_3", false},                          // kernel default name
		{"cexdra-0123456789abcdef012345678", false},  // 32 characters
		{"cexdra-ABC", false},                        // upper case
		{"cexdra-../../class/zcrypt/destroy", false}, // path escape
		{"cexdra-a b", false},                        // space
	} {
		err := ValidateName(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestCreateWritesName(t *testing.T) {
	classDir, _, _ := newFakeSysfs(t)
	if err := Create(testName); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := readAttr(t, filepath.Join(classDir, "create")); got != testName {
		t.Errorf("create = %q, want %q", got, testName)
	}
}

func TestCreateRejectsInvalidName(t *testing.T) {
	classDir, _, _ := newFakeSysfs(t)
	if err := Create("zcrypt_1"); err == nil {
		t.Fatal("Create accepted a name without the driver prefix")
	}
	if got := readAttr(t, filepath.Join(classDir, "create")); got != "" {
		t.Errorf("create = %q, want nothing written", got)
	}
}

func TestCreateWithoutKernelSupport(t *testing.T) {
	newFakeSysfs(t)
	ClassDir = filepath.Join(t.TempDir(), "missing")
	if Supported() {
		t.Error("Supported() = true without a create attribute")
	}
	err := Create(testName)
	if err == nil || !strings.Contains(err.Error(), testName) {
		t.Errorf("Create = %v, want an error naming the node", err)
	}
}

func TestRestrictWritesAbsoluteMasks(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	dir := makeNode(t, nodeDir, testName)

	perms := Perms{
		Adapters:       []int{6},
		UsageDomains:   []int{0, 1},
		ControlDomains: []int{0},
		Ioctls:         AllIoctls(),
	}
	if err := Restrict(testName, perms); err != nil {
		t.Fatalf("Restrict: %v", err)
	}

	for attr, want := range map[string]string{
		"apmask":    "0x02", // adapter 6: bit 6 = second digit, value 2
		"aqmask":    "0xc",  // domains 0 and 1
		"admask":    "0x8",  // control domain 0
		"ioctlmask": "0x" + strings.Repeat("f", 64),
	} {
		if got := readAttr(t, filepath.Join(dir, attr)); got != want {
			t.Errorf("%s = %q, want %q", attr, got, want)
		}
	}
}

func TestRestrictEmptyListClearsMask(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	dir := makeNode(t, nodeDir, testName)

	if err := Restrict(testName, Perms{Adapters: []int{6}, UsageDomains: []int{0}}); err != nil {
		t.Fatalf("Restrict: %v", err)
	}
	// usage-only: no control domains, and no ioctls yet.
	for _, attr := range []string{"admask", "ioctlmask"} {
		if got := readAttr(t, filepath.Join(dir, attr)); got != "0x0" {
			t.Errorf("%s = %q, want %q", attr, got, "0x0")
		}
	}
}

func TestRestrictRejectsOutOfRange(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	dir := makeNode(t, nodeDir, testName)

	err := Restrict(testName, Perms{Adapters: []int{256}})
	if err == nil || !strings.Contains(err.Error(), "apmask") {
		t.Fatalf("Restrict = %v, want an apmask range error", err)
	}
	if got := readAttr(t, filepath.Join(dir, "apmask")); got != emptyMask+"\n" {
		t.Errorf("apmask = %q, want it untouched", got)
	}
}

func TestRestrictMissingNode(t *testing.T) {
	newFakeSysfs(t)
	err := Restrict(testName, Perms{Adapters: []int{6}})
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Restrict = %v, want a wrapped ErrNotExist", err)
	}
}

func TestReadPermsRoundTrip(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	dir := makeNode(t, nodeDir, testName)

	// What the kernel shows after granting adapter 6, domains 0 and 1,
	// control domain 0 and ioctls 0-3: full width, newline terminated.
	pad := func(prefix string) string { return "0x" + prefix + strings.Repeat("0", 64-len(prefix)) + "\n" }
	for attr, v := range map[string]string{
		"apmask":    pad("02"),
		"aqmask":    pad("c"),
		"admask":    pad("8"),
		"ioctlmask": pad("f"),
	} {
		if err := os.WriteFile(filepath.Join(dir, attr), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadPerms(testName)
	if err != nil {
		t.Fatalf("ReadPerms: %v", err)
	}
	want := Perms{
		Adapters:       []int{6},
		UsageDomains:   []int{0, 1},
		ControlDomains: []int{0},
		Ioctls:         []int{0, 1, 2, 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadPerms = %+v, want %+v", got, want)
	}
}

func TestDestroy(t *testing.T) {
	classDir, nodeDir, _ := newFakeSysfs(t)
	makeNode(t, nodeDir, testName)

	if err := Destroy(testName); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got := readAttr(t, filepath.Join(classDir, "destroy")); got != testName {
		t.Errorf("destroy = %q, want %q", got, testName)
	}
}

func TestDestroyMissingNodeIsIdempotent(t *testing.T) {
	classDir, _, _ := newFakeSysfs(t)
	if err := Destroy(testName); err != nil {
		t.Fatalf("Destroy of a missing node: %v", err)
	}
	if got := readAttr(t, filepath.Join(classDir, "destroy")); got != "" {
		t.Errorf("destroy = %q, want nothing written for a missing node", got)
	}
}

func TestDestroyRefusesForeignNodes(t *testing.T) {
	classDir, nodeDir, _ := newFakeSysfs(t)
	makeNode(t, nodeDir, "cexlab") // an administrator's zcryptctl node
	if err := Destroy("cexlab"); err == nil {
		t.Fatal("Destroy accepted a node the driver does not own")
	}
	if got := readAttr(t, filepath.Join(classDir, "destroy")); got != "" {
		t.Errorf("destroy = %q, want nothing written", got)
	}
}

func TestList(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	for _, n := range []string{"cexdra-bbb", "cexlab", "zcrypt_0", "cexdra-aaa"} {
		makeNode(t, nodeDir, n)
	}
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"cexdra-aaa", "cexdra-bbb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
}

func TestListWithoutNodeDir(t *testing.T) {
	newFakeSysfs(t)
	NodeDir = filepath.Join(t.TempDir(), "missing")
	got, err := List()
	if err != nil || got != nil {
		t.Errorf("List = %q, %v; want nil, nil", got, err)
	}
}

func TestWaitDevNode(t *testing.T) {
	_, _, devDir := newFakeSysfs(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(devDir, testName), nil, 0o600)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitDevNode(ctx, testName, 5*time.Millisecond); err != nil {
		t.Fatalf("WaitDevNode: %v", err)
	}
}

func TestWaitDevNodeTimeout(t *testing.T) {
	newFakeSysfs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := WaitDevNode(ctx, testName, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDevNode = %v, want DeadlineExceeded", err)
	}
}

func TestFormatAndParseMask(t *testing.T) {
	for _, tc := range []struct {
		ids  []int
		want string
	}{
		{nil, "0x0"},
		{[]int{0}, "0x8"},
		{[]int{3}, "0x1"},
		{[]int{4}, "0x08"},
		{[]int{6}, "0x02"},
		{[]int{0, 1, 2, 3}, "0xf"},
		{[]int{255}, "0x" + strings.Repeat("0", 63) + "1"},
	} {
		got, err := FormatMask(tc.ids)
		if err != nil {
			t.Fatalf("FormatMask(%v): %v", tc.ids, err)
		}
		if got != tc.want {
			t.Errorf("FormatMask(%v) = %q, want %q", tc.ids, got, tc.want)
		}
		back, err := ParseMask(got)
		if err != nil {
			t.Fatalf("ParseMask(%q): %v", got, err)
		}
		if len(tc.ids) == 0 {
			if len(back) != 0 {
				t.Errorf("ParseMask(%q) = %v, want empty", got, back)
			}
			continue
		}
		if !reflect.DeepEqual(back, tc.ids) {
			t.Errorf("ParseMask(FormatMask(%v)) = %v", tc.ids, back)
		}
	}
}

func TestParseMaskRejects(t *testing.T) {
	for _, s := range []string{"", "0x", "0xg0", "0x" + strings.Repeat("f", 65)} {
		if _, err := ParseMask(s); err == nil {
			t.Errorf("ParseMask(%q) succeeded, want error", s)
		}
	}
}

func TestDevNumber(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	dir := makeNode(t, nodeDir, testName)
	if err := os.WriteFile(filepath.Join(dir, "dev"), []byte("246:3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	major, minor, err := DevNumber(testName)
	if err != nil {
		t.Fatalf("DevNumber: %v", err)
	}
	if major != 246 || minor != 3 {
		t.Errorf("DevNumber = %d:%d, want 246:3", major, minor)
	}
}

func TestDevNumberMalformed(t *testing.T) {
	_, nodeDir, _ := newFakeSysfs(t)
	dir := makeNode(t, nodeDir, testName)
	for _, v := range []string{"246", "x:3", "246:y", ""} {
		if err := os.WriteFile(filepath.Join(dir, "dev"), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := DevNumber(testName); err == nil {
			t.Errorf("DevNumber accepted dev %q", v)
		}
	}
}
