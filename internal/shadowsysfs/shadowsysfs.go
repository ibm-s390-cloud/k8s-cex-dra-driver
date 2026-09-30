// Package shadowsysfs builds a per-claim copy of the AP bus sysfs that shows a
// container only the crypto cards and queues its claim was allocated.
//
// A container's /sys is the host's sysfs, so without a shadow a workload
// running lszcrypt, libica, openCryptoki or the CCA library sees every AP
// card and queue on the host - including queues that belong to other pods or
// to nobody. The claim's zcrypt node already refuses requests to those queues
// (see package zcrypt), but software that enumerates the bus first and picks
// a queue from the listing then fails on the queue it picked, or numbers
// adapters differently from what the claim implies.
//
// The shadow tree replaces /sys/bus/ap and /sys/devices/ap inside the
// container with read-only bind mounts of a small directory tree that
// mirrors the kernel's layout for exactly the claim's adapters and queues:
//
//	<BaseDir>/<claimUID>/
//	  bus/ap/                    ap_adapter_mask, ap_usage_domain_mask, ...
//	  bus/ap/devices/{cardXX,XX.YYYY}   -> ../../../devices/ap/...
//	  bus/ap/drivers/<drv>/{cardXX,XX.YYYY}
//	  devices/ap/cardXX/         hwtype, type, ap_functions, ...
//	  devices/ap/cardXX/XX.YYYY/ mkvps, online, ...
//
// Symlinks are relative, so they resolve both on the host (inside the tree)
// and in the container (where the two subtrees sit at /sys/bus/ap and
// /sys/devices/ap). Static attributes are copied from the host at prepare
// time, the masks are generated from the claim, and counters and state
// attributes carry fixed "healthy, idle" values. The tree is a snapshot: a
// card that goes offline later still reads online here, the same trade-off
// the CEX device plug-in's static shadow sysfs makes.
//
// The design follows the shadow sysfs of IBM's CEX device plug-in for
// Kubernetes (k8s-cex-dev-plugin, shadowsysfs.go), generalised from one
// APQN to an adapter x domain grid.
package shadowsysfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"k8s-cex-dra-driver/internal/logphase"
)

// Paths the package reads from and writes to. Variables so tests can point
// them at a fake tree; production code sets BaseDir from the command line.
var (
	// HostBusDir and HostDevicesDir are the host's AP sysfs, as mounted into
	// the plugin container.
	HostBusDir     = "/sys/bus/ap"
	HostDevicesDir = "/sys/devices/ap"

	// BaseDir holds one tree per prepared container claim. It must be a host
	// path mounted at the same path into the plugin container: the container
	// runtime resolves the CDI mounts on the host. A tmpfs such as /var/run
	// fits: the trees, like the zcrypt nodes they accompany, do not survive a
	// reboot, and neither do the claims.
	BaseDir = "/var/run/cex-dra/shadowsysfs"

	// SELinuxLabel is set on every file of a tree on SELinux hosts so that a
	// confined container (container_t, any MCS categories) may read the bind
	// mounts. s0 without categories is dominated by every container level,
	// so reads are allowed and nothing else is needed. Empty disables
	// labelling.
	SELinuxLabel = "system_u:object_r:container_file_t:s0"
)

// Paths inside the container that the tree's two halves are mounted over.
const (
	ContainerBusDir     = "/sys/bus/ap"
	ContainerDevicesDir = "/sys/devices/ap"
)

const (
	// completeMarker is written last. A tree without it is a leftover of an
	// interrupted build and is rebuilt; a tree with it is reused as is, so a
	// repeated prepare for a running pod never swaps a mounted tree.
	completeMarker = ".complete"
	tmpPrefix      = ".tmp-"

	dirMode  os.FileMode = 0o755
	fileMode os.FileMode = 0o444

	// maskBits is the width of every AP mask the kernel prints.
	maskBits = 256
)

// Attribute lists, as the kernel names them. "copy" files must exist on the
// host; "maybe" files depend on kernel version and card type and are copied
// when present. A failed read of a maybe file is logged and skipped: mkvps in
// particular queries the card and can fail transiently.
var (
	busCopyFiles  = []string{"ap_interrupts", "ap_max_domain_id", "poll_thread", "poll_timeout"}
	busMaybeFiles = []string{"ap_max_adapter_id", "features"}

	cardCopyFiles  = []string{"ap_functions", "depth", "hwtype", "raw_hwtype", "type"}
	cardMaybeFiles = []string{"API_ordinalnr", "FW_version", "max_msg_size", "op_modes", "serialnr"}

	queueCopyFiles  = []string{"interrupt", "reset"}
	queueMaybeFiles = []string{"mkvps", "op_modes"}

	// Fixed values for state and counter attributes, on cards and queues
	// alike: configured, online, not check-stopped, idle.
	fixedFiles = []struct{ name, value string }{
		{"chkstop", "0\n"},
		{"config", "1\n"},
		{"load", "0\n"},
		{"online", "1\n"},
		{"pendingq_count", "0\n"},
		{"request_count", "0\n"},
		{"requestq_count", "0\n"},
	}
)

// Grid is what a tree exposes: every queue of Adapters x UsageDomains, plus
// the control domains the claim's zcrypt node grants.
type Grid struct {
	Adapters       []int
	UsageDomains   []int
	ControlDomains []int
}

// Tree is a built shadow tree: the two host directories to bind-mount over
// ContainerBusDir and ContainerDevicesDir.
type Tree struct {
	Dir        string
	BusDir     string
	DevicesDir string
}

// Path returns the tree directory of a claim.
func Path(claimUID string) string {
	return filepath.Join(BaseDir, claimUID)
}

func treeOf(dir string) Tree {
	return Tree{
		Dir:        dir,
		BusDir:     filepath.Join(dir, "bus", "ap"),
		DevicesDir: filepath.Join(dir, "devices", "ap"),
	}
}

// validateUID refuses a claim UID that would escape BaseDir or collide with
// the package's own bookkeeping names. Kubernetes UIDs never do; the check
// guards the RemoveAll below against a malformed caller.
func validateUID(claimUID string) error {
	if claimUID == "" || claimUID == "." || claimUID == ".." ||
		strings.ContainsAny(claimUID, `/\`) || strings.HasPrefix(claimUID, ".") {
		return fmt.Errorf("invalid claim UID %q for a shadow sysfs tree", claimUID)
	}
	return nil
}

// Build creates the shadow tree for a claim, or returns the existing one.
//
// The tree is assembled under a temporary name and renamed into place once
// complete, so a crash never leaves a half-built tree under the final name,
// and a complete tree is never rebuilt: the kubelet may call prepare again
// for a claim whose pod is running with the tree mounted, and a claim's
// allocation cannot change.
func Build(claimUID string, g Grid) (Tree, error) {
	if err := validateUID(claimUID); err != nil {
		return Tree{}, err
	}
	if len(g.Adapters) == 0 || len(g.UsageDomains) == 0 {
		return Tree{}, fmt.Errorf("shadow sysfs for claim %s: empty grid (adapters %v, domains %v)", claimUID, g.Adapters, g.UsageDomains)
	}
	final := Path(claimUID)
	if _, err := os.Stat(filepath.Join(final, completeMarker)); err == nil {
		return treeOf(final), nil
	}

	if err := os.MkdirAll(BaseDir, dirMode); err != nil {
		return Tree{}, fmt.Errorf("create shadow sysfs base %s: %w", BaseDir, err)
	}
	tmp := filepath.Join(BaseDir, tmpPrefix+claimUID)
	if err := os.RemoveAll(tmp); err != nil {
		return Tree{}, fmt.Errorf("remove stale %s: %w", tmp, err)
	}
	if err := build(tmp, g); err != nil {
		_ = os.RemoveAll(tmp)
		return Tree{}, fmt.Errorf("build shadow sysfs for claim %s: %w", claimUID, err)
	}
	if err := label(tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return Tree{}, fmt.Errorf("label shadow sysfs for claim %s: %w", claimUID, err)
	}
	// An incomplete tree under the final name is a crash leftover no
	// container can have mounted: CDI specs are written only after Build.
	if err := os.RemoveAll(final); err != nil {
		_ = os.RemoveAll(tmp)
		return Tree{}, fmt.Errorf("remove incomplete %s: %w", final, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return Tree{}, fmt.Errorf("move shadow sysfs into place: %w", err)
	}
	logphase.Logf(logphase.Preparation, "shadow sysfs for claim %s built at %s (adapters %v, usage domains %v)",
		claimUID, final, g.Adapters, g.UsageDomains)
	return treeOf(final), nil
}

// Remove deletes a claim's tree. A missing tree is not an error, so it is
// safe for every claim, VM claims included.
func Remove(claimUID string) error {
	if err := validateUID(claimUID); err != nil {
		return err
	}
	for _, dir := range []string{Path(claimUID), filepath.Join(BaseDir, tmpPrefix+claimUID)} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove shadow sysfs %s: %w", dir, err)
		}
	}
	return nil
}

// List returns the claim UIDs that have a tree, complete or not, and removes
// temporary build directories left by a crash along the way. A missing
// BaseDir lists nothing.
func List() ([]string, error) {
	entries, err := os.ReadDir(BaseDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list shadow sysfs trees under %s: %w", BaseDir, err)
	}
	var uids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, tmpPrefix) {
			if err := os.RemoveAll(filepath.Join(BaseDir, name)); err != nil {
				logphase.Warnf(logphase.Preparation, "remove leftover shadow sysfs build %s: %v", name, err)
			}
			continue
		}
		if validateUID(name) == nil {
			uids = append(uids, name)
		}
	}
	sort.Strings(uids)
	return uids, nil
}

// build writes the whole tree under dir.
func build(dir string, g Grid) error {
	t := treeOf(dir)
	adapters := sortedUnique(g.Adapters)
	domains := sortedUnique(g.UsageDomains)

	// /sys/bus/ap
	if err := os.MkdirAll(t.BusDir, dirMode); err != nil {
		return err
	}
	if err := copyFiles(HostBusDir, t.BusDir, busCopyFiles, busMaybeFiles); err != nil {
		return err
	}
	for _, m := range []struct {
		name string
		ids  []int
		all  bool
	}{
		{"ap_adapter_mask", adapters, false},
		{"ap_usage_domain_mask", domains, false},
		{"ap_control_domain_mask", sortedUnique(g.ControlDomains), false},
		// apmask/aqmask assign queues to the default drivers. All ones
		// matches a host where nothing is reserved for vfio_ap, which is
		// true of every queue in this tree.
		{"apmask", nil, true},
		{"aqmask", nil, true},
	} {
		if err := writeFile(filepath.Join(t.BusDir, m.name), formatMask(m.ids, m.all)); err != nil {
			return err
		}
	}
	// ap_domain is the default domain the kernel picks for requests that do
	// not name one. Tools read it as "the" domain of a single-domain guest.
	if err := writeFile(filepath.Join(t.BusDir, "ap_domain"), fmt.Sprintf("%d\n", domains[0])); err != nil {
		return err
	}
	for _, sub := range []string{"devices", "drivers"} {
		if err := os.MkdirAll(filepath.Join(t.BusDir, sub), dirMode); err != nil {
			return err
		}
	}

	// /sys/devices/ap
	if err := os.MkdirAll(t.DevicesDir, dirMode); err != nil {
		return err
	}
	for _, a := range adapters {
		card := fmt.Sprintf("card%02x", a)
		hostCard := filepath.Join(HostDevicesDir, card)
		if _, err := os.Stat(hostCard); err != nil {
			return fmt.Errorf("adapter %02x: %w", a, err)
		}
		shadowCard := filepath.Join(t.DevicesDir, card)
		if err := os.MkdirAll(shadowCard, dirMode); err != nil {
			return err
		}
		if err := copyFiles(hostCard, shadowCard, cardCopyFiles, cardMaybeFiles); err != nil {
			return err
		}
		if err := writeFixed(shadowCard); err != nil {
			return err
		}
		cardDrv := driverName(hostCard, "cex4card")
		if err := linkDevice(t, shadowCard, card, cardDrv, "../../../bus/ap",
			"../../../devices/ap/"+card, "../../../../devices/ap/"+card); err != nil {
			return err
		}

		for _, d := range domains {
			queue := fmt.Sprintf("%02x.%04x", a, d)
			hostQueue := filepath.Join(hostCard, queue)
			if _, err := os.Stat(hostQueue); err != nil {
				return fmt.Errorf("queue %s: %w", queue, err)
			}
			shadowQueue := filepath.Join(shadowCard, queue)
			if err := os.MkdirAll(shadowQueue, dirMode); err != nil {
				return err
			}
			if err := copyFiles(hostQueue, shadowQueue, queueCopyFiles, queueMaybeFiles); err != nil {
				return err
			}
			if err := writeFixed(shadowQueue); err != nil {
				return err
			}
			queueDrv := driverName(hostQueue, "cex4queue")
			if err := linkDevice(t, shadowQueue, queue, queueDrv, "../../../../bus/ap",
				"../../../devices/ap/"+card+"/"+queue, "../../../../devices/ap/"+card+"/"+queue); err != nil {
				return err
			}
		}
	}

	return writeFile(filepath.Join(dir, completeMarker), "")
}

// linkDevice creates a device's four links: its driver and subsystem links,
// its entry under bus/ap/devices, and its entry under its driver's directory.
// up is the relative path from the device directory to bus/ap; busLink and
// drvLink are the relative targets from bus/ap/devices and
// bus/ap/drivers/<drv> back to the device.
func linkDevice(t Tree, devDir, name, drv, up, busLink, drvLink string) error {
	drvDir := filepath.Join(t.BusDir, "drivers", drv)
	if err := os.MkdirAll(drvDir, dirMode); err != nil {
		return err
	}
	for _, l := range []struct{ at, to string }{
		{filepath.Join(devDir, "driver"), up + "/drivers/" + drv},
		{filepath.Join(devDir, "subsystem"), up},
		{filepath.Join(t.BusDir, "devices", name), busLink},
		{filepath.Join(drvDir, name), drvLink},
	} {
		if err := os.Symlink(l.to, l.at); err != nil {
			return err
		}
	}
	return nil
}

// driverName returns the basename of a host device's driver link, the name
// its shadow links to, or def when the device has none - a queue in
// transition, or a fake tree.
func driverName(hostDev, def string) string {
	target, err := os.Readlink(filepath.Join(hostDev, "driver"))
	if err != nil || filepath.Base(target) == "" || filepath.Base(target) == "." {
		return def
	}
	return filepath.Base(target)
}

func copyFiles(src, dst string, required, maybe []string) error {
	for _, f := range required {
		data, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Join(src, f), err)
		}
		if err := writeFile(filepath.Join(dst, f), string(data)); err != nil {
			return err
		}
	}
	for _, f := range maybe {
		p := filepath.Join(src, f)
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			logphase.Warnf(logphase.Preparation, "shadow sysfs: skipping %s: %v", p, err)
			continue
		}
		if err := writeFile(filepath.Join(dst, f), string(data)); err != nil {
			return err
		}
	}
	return nil
}

func writeFixed(dir string) error {
	for _, f := range fixedFiles {
		if err := writeFile(filepath.Join(dir, f.name), f.value); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes a read-only file. The mode is applied with chmod so the
// process umask cannot widen or narrow it.
func writeFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
		return err
	}
	return os.Chmod(path, fileMode)
}

// formatMask renders a 256-bit AP mask the way the kernel prints it: "0x",
// 64 hex digits, most significant bit = id 0, and a newline.
func formatMask(ids []int, all bool) string {
	var mask [maskBits / 8]byte
	if all {
		for i := range mask {
			mask[i] = 0xff
		}
	}
	for _, id := range ids {
		if id >= 0 && id < maskBits {
			mask[id/8] |= 0x80 >> (id % 8)
		}
	}
	var b strings.Builder
	b.Grow(2 + len(mask)*2 + 1)
	b.WriteString("0x")
	for _, v := range mask {
		fmt.Fprintf(&b, "%02x", v)
	}
	b.WriteString("\n")
	return b.String()
}

func sortedUnique(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// selinuxEnabled reports whether dir carries an SELinux label, which every
// file has on a host with SELinux enabled and none has otherwise. Asking the
// file system works inside a container, where /sys/fs/selinux may not be
// mounted, and cannot mistake another LSM's /proc/self/attr for SELinux.
var selinuxEnabled = func(dir string) bool {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(dir, "security.selinux", buf)
	return err == nil && n > 0
}

// label sets SELinuxLabel on every file, directory and symlink of the tree.
// Files the plugin writes under /var/run get var_run_t, which container_t
// may not read, so without this the bind mounts would read as EACCES inside
// an enforcing node's pods. The plugin runs as spc_t, which may relabel.
func label(dir string) error {
	if SELinuxLabel == "" || !selinuxEnabled(dir) {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := unix.Lsetxattr(p, "security.selinux", []byte(SELinuxLabel), 0); err != nil {
			return fmt.Errorf("set %s on %s: %w", SELinuxLabel, p, err)
		}
		return nil
	})
}
