package devicestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"k8s-cex-dra-driver/internal/cryptoconfig"
	"k8s-cex-dra-driver/internal/shadowsysfs"
	"k8s-cex-dra-driver/internal/zcrypt"
)

// zcryptFixture is a fake /sys/class/zcrypt plus /sys/devices/virtual/zcrypt
// tree. A plain file cannot create a directory when written to, so a test
// that wants the prepare path to find its node pre-creates it with addNode,
// the way the kernel would after a create.
type zcryptFixture struct {
	classDir string
	nodeDir  string
}

func newZcryptFixture(t *testing.T) *zcryptFixture {
	t.Helper()
	root := t.TempDir()
	f := &zcryptFixture{
		classDir: filepath.Join(root, "class", "zcrypt"),
		nodeDir:  filepath.Join(root, "devices", "virtual", "zcrypt"),
	}
	for _, d := range []string{f.classDir, f.nodeDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	write(t, filepath.Join(f.classDir, "create"), "")
	write(t, filepath.Join(f.classDir, "destroy"), "")
	t.Cleanup(restore(&zcrypt.ClassDir, f.classDir))
	t.Cleanup(restore(&zcrypt.NodeDir, f.nodeDir))
	newShadowFixture(t, root)
	return f
}

// newShadowFixture is the host AP sysfs the shadow tree copies from: card01
// with queues 01.0002 (the test claim's) and 01.0003 (someone else's), and
// card02, which no claim uses. The shadow base is a fresh directory.
func newShadowFixture(t *testing.T, root string) {
	t.Helper()
	bus := filepath.Join(root, "sys", "bus", "ap")
	devs := filepath.Join(root, "sys", "devices", "ap")
	for _, f := range []string{"ap_interrupts", "ap_max_domain_id", "poll_thread", "poll_timeout"} {
		mkfile(t, filepath.Join(bus, f), "0\n")
	}
	for _, card := range []string{"card01", "card02"} {
		for _, f := range []string{"ap_functions", "depth", "hwtype", "raw_hwtype", "type"} {
			mkfile(t, filepath.Join(devs, card, f), "x\n")
		}
	}
	for _, q := range []string{"card01/01.0002", "card01/01.0003", "card02/02.0002"} {
		for _, f := range []string{"interrupt", "reset"} {
			mkfile(t, filepath.Join(devs, q, f), "x\n")
		}
	}
	t.Cleanup(restore(&shadowsysfs.HostBusDir, bus))
	t.Cleanup(restore(&shadowsysfs.HostDevicesDir, devs))
	t.Cleanup(restore(&shadowsysfs.BaseDir, filepath.Join(root, "shadow")))
}

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	write(t, path, content)
}

// addNode creates a node directory the way the kernel does after a create:
// four empty masks and the device number.
func (f *zcryptFixture) addNode(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(f.nodeDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, attr := range []string{"apmask", "aqmask", "admask", "ioctlmask"} {
		write(t, filepath.Join(dir, attr), "0x"+strings.Repeat("0", 64)+"\n")
	}
	write(t, filepath.Join(dir, "dev"), "246:3\n")
	return dir
}

func (f *zcryptFixture) read(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(b)
}

// TestPrepareContainerBuildsZcryptNode walks the whole container prepare path
// against the fake tree: masks granting exactly the one allocated queue (card
// 01, domain 0002), control access to that domain under the default mode,
// every ioctl, and a CDI spec mapping the node's device number to
// /dev/z90crypt.
func TestPrepareContainerBuildsZcryptNode(t *testing.T) {
	enableContainerWorkload(t)
	f := newZcryptFixture(t)
	s := cdTestState(t)
	claim := claimWithMode(t, DeviceClassContainer, "")
	node := f.addNode(t, zcrypt.NodeName(string(claim.UID)))

	devices, err := s.Prepare(t.Context(), claim)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("prepared %d devices, want 1", len(devices))
	}
	if ids := devices[0].GetCdiDeviceIds(); len(ids) != 1 || ids[0] != "ibm.com/zcrypt="+string(claim.UID) {
		t.Errorf("CDI device IDs = %v, want [ibm.com/zcrypt=%s]", ids, claim.UID)
	}

	for attr, want := range map[string]string{
		"apmask":    "0x4", // adapter 0x01
		"aqmask":    "0x2", // domain 0x0002
		"admask":    "0x2", // usage-and-control is the default
		"ioctlmask": "0x" + strings.Repeat("f", 64),
	} {
		if got := f.read(t, node, attr); got != want {
			t.Errorf("%s = %q, want %q", attr, got, want)
		}
	}

	raw := f.read(t, s.cdiRoot, "ibm.com-zcrypt-"+string(claim.UID)+".json")
	var spec cdispec.Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("unmarshal CDI spec: %v", err)
	}
	dn := spec.Devices[0].ContainerEdits.DeviceNodes[0]
	if dn.Path != "/dev/z90crypt" || dn.Major != 246 || dn.Minor != 3 {
		t.Errorf("device node = %+v, want /dev/z90crypt 246:3", dn)
	}
	if env := spec.Devices[0].ContainerEdits.Env; len(env) != 1 || env[0] != "CEX_APQNS=01.0002" {
		t.Errorf("env = %q, want [CEX_APQNS=01.0002]", env)
	}

	// The shadow tree lists the claim's queue and nothing else, and the spec
	// mounts it over the container's AP sysfs.
	tree := shadowsysfs.Path(string(claim.UID))
	if _, err := os.Stat(filepath.Join(tree, "devices", "ap", "card01", "01.0002")); err != nil {
		t.Errorf("claimed queue missing from shadow sysfs: %v", err)
	}
	for _, other := range []string{"card01/01.0003", "card02"} {
		if _, err := os.Stat(filepath.Join(tree, "devices", "ap", other)); !os.IsNotExist(err) {
			t.Errorf("%s visible in shadow sysfs (stat err %v)", other, err)
		}
	}
	mounts := spec.Devices[0].ContainerEdits.Mounts
	if len(mounts) != 2 ||
		mounts[0].HostPath != filepath.Join(tree, "bus", "ap") || mounts[0].ContainerPath != "/sys/bus/ap" ||
		mounts[1].HostPath != filepath.Join(tree, "devices", "ap") || mounts[1].ContainerPath != "/sys/devices/ap" {
		t.Errorf("mounts = %+v, want the shadow tree over /sys/bus/ap and /sys/devices/ap", mounts)
	}
}

// TestPrepareContainerControlDomainModes pins how each mode maps to the
// node's control-domain mask. A mode the driver does not serve grants
// nothing rather than failing: a claim may land on either binding.
func TestPrepareContainerControlDomainModes(t *testing.T) {
	enableContainerWorkload(t)
	for _, tc := range []struct {
		mode string
		want string
	}{
		{cryptoconfig.ControlDomainModeUsageAndControl, "0x2"},
		{cryptoconfig.ControlDomainModeUsageOnly, "0x0"},
		{cryptoconfig.ControlDomainModeAllControlDomains, "0x0"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newZcryptFixture(t)
			s := cdTestState(t)
			claim := claimWithMode(t, DeviceClassContainer, tc.mode)
			node := f.addNode(t, zcrypt.NodeName(string(claim.UID)))

			if _, err := s.Prepare(t.Context(), claim); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got := f.read(t, node, "admask"); got != tc.want {
				t.Errorf("admask = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrepareContainerCreatesMissingNode checks the create half on its own:
// with no node present Prepare asks the kernel for one by name. The fake
// class attribute cannot produce the node, so the restrict that follows
// fails and the error surfaces, but the create request is what is pinned.
func TestPrepareContainerCreatesMissingNode(t *testing.T) {
	enableContainerWorkload(t)
	f := newZcryptFixture(t)
	s := cdTestState(t)
	claim := claimWithMode(t, DeviceClassContainer, "")

	_, err := s.Prepare(t.Context(), claim)
	if err == nil {
		t.Fatal("Prepare succeeded although the node never appeared")
	}
	if got := f.read(t, f.classDir, "create"); got != zcrypt.NodeName(string(claim.UID)) {
		t.Errorf("create = %q, want %q", got, zcrypt.NodeName(string(claim.UID)))
	}
	if _, statErr := os.Stat(filepath.Join(s.cdiRoot, "ibm.com-zcrypt-"+string(claim.UID)+".json")); statErr == nil {
		t.Error("CDI spec written for a node that was never restricted")
	}
}

// TestPrepareContainerRefusesVFIOAPQueue pins that the container path never
// hands out a queue the last scan saw on vfio_ap: the node could not reach it,
// and the path does not rebind.
func TestPrepareContainerRefusesVFIOAPQueue(t *testing.T) {
	enableContainerWorkload(t)
	f := newZcryptFixture(t)
	s := cdTestState(t)
	dev := s.allocatable["cex-01-0002"]
	dev.Attributes["cex.ibm.com/driver"] = resourceapi.DeviceAttribute{StringValue: ptr("vfio_ap")}
	s.allocatable["cex-01-0002"] = dev
	claim := claimWithMode(t, DeviceClassContainer, "")
	f.addNode(t, zcrypt.NodeName(string(claim.UID)))

	_, err := s.Prepare(t.Context(), claim)
	if err == nil || !strings.Contains(err.Error(), "vfio_ap") {
		t.Fatalf("Prepare = %v, want an error naming vfio_ap", err)
	}
	if got := f.read(t, f.classDir, "create"); got != "" {
		t.Errorf("create = %q, want nothing written before the binding check", got)
	}
}

// TestUnprepareContainerClaim pins the teardown: spec removed, node destroyed,
// and no vfio-ap reconcile for a claim that never touched vfio-ap.
func TestUnprepareContainerClaim(t *testing.T) {
	enableContainerWorkload(t)
	f := newZcryptFixture(t)
	newUnprepareFixture(t, nil)
	s := cdTestState(t)
	claim := claimWithMode(t, DeviceClassContainer, "")
	name := zcrypt.NodeName(string(claim.UID))
	f.addNode(t, name)
	if _, err := s.Prepare(t.Context(), claim); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := s.Unprepare(t.Context(), string(claim.UID)); err != nil {
		t.Fatalf("Unprepare: %v", err)
	}
	if got := f.read(t, f.classDir, "destroy"); got != name {
		t.Errorf("destroy = %q, want %q", got, name)
	}
	if _, err := os.Stat(filepath.Join(s.cdiRoot, "ibm.com-zcrypt-"+string(claim.UID)+".json")); !os.IsNotExist(err) {
		t.Errorf("CDI spec still present after Unprepare (stat err %v)", err)
	}
	if _, err := os.Stat(shadowsysfs.Path(string(claim.UID))); !os.IsNotExist(err) {
		t.Errorf("shadow sysfs still present after Unprepare (stat err %v)", err)
	}
}

// TestUnprepareContainerClaimIgnoresGate mirrors the VM-side guarantee:
// cleanup of a claim prepared while the gate was on still runs after it is
// switched off.
func TestUnprepareContainerClaimIgnoresGate(t *testing.T) {
	f := newZcryptFixture(t)
	newUnprepareFixture(t, nil)
	s := cdTestState(t)
	name := zcrypt.NodeName("claim-under-test")
	f.addNode(t, name)

	if err := s.Unprepare(t.Context(), "claim-under-test"); err != nil {
		t.Fatalf("Unprepare with the gate off: %v", err)
	}
	if got := f.read(t, f.classDir, "destroy"); got != name {
		t.Errorf("destroy = %q, want %q", got, name)
	}
}

// TestReconcileZcryptNodes covers the three crash leftovers: a pair survives,
// a spec without its node is deleted, a node without its spec is destroyed.
// An administrator's node is never touched.
func TestReconcileZcryptNodes(t *testing.T) {
	f := newZcryptFixture(t)
	cdiRoot := t.TempDir()

	f.addNode(t, zcrypt.NodeName("paired"))
	f.addNode(t, zcrypt.NodeName("orphan-node"))
	f.addNode(t, "cexlab")
	for _, uid := range []string{"paired", "orphan-spec"} {
		write(t, filepath.Join(cdiRoot, "ibm.com-zcrypt-"+uid+".json"), "{}")
	}
	for _, uid := range []string{"paired", "orphan-tree"} {
		mkfile(t, filepath.Join(shadowsysfs.Path(uid), ".complete"), "")
	}

	if err := ReconcileZcryptNodes(cdiRoot); err != nil {
		t.Fatalf("ReconcileZcryptNodes: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cdiRoot, "ibm.com-zcrypt-paired.json")); err != nil {
		t.Errorf("paired spec removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cdiRoot, "ibm.com-zcrypt-orphan-spec.json")); !os.IsNotExist(err) {
		t.Errorf("orphan spec kept (stat err %v)", err)
	}
	// The fake destroy attribute keeps only the last write, so exactly one
	// destroy - the orphan node - may have happened.
	if got := f.read(t, f.classDir, "destroy"); got != zcrypt.NodeName("orphan-node") {
		t.Errorf("destroy = %q, want only %q", got, zcrypt.NodeName("orphan-node"))
	}
	if _, err := os.Stat(shadowsysfs.Path("paired")); err != nil {
		t.Errorf("paired shadow sysfs removed: %v", err)
	}
	if _, err := os.Stat(shadowsysfs.Path("orphan-tree")); !os.IsNotExist(err) {
		t.Errorf("orphan shadow sysfs kept (stat err %v)", err)
	}
}
