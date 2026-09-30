// Package zcrypt manages additional zcrypt device nodes through the
// /sys/class/zcrypt sysfs interface (Linux 4.20 and later, no Kconfig option;
// userspace tool zcryptctl from s390-tools 2.7).
//
// The default node /dev/z90crypt reaches every AP queue the host can see. An
// additional node is a second character device served by the same zcrypt
// driver, filtered by four per-node masks: the adapters (apmask), usage
// domains (aqmask) and control domains (admask) it may address, and the
// ioctls (ioctlmask) it accepts. Handing a container such a node, mapped to
// /dev/z90crypt inside it, limits the container to the queues it was
// allocated while the queues themselves stay bound to the zcrypt driver.
//
// This package only creates, restricts, inspects and destroys nodes. It does
// not decide which queues a node covers and it never touches queue bindings.
package zcrypt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s-cex-dra-driver/internal/logphase"
)

// ClassDir holds the class attributes that create and destroy nodes. NodeDir
// holds one directory per existing node with its mask attributes. DevDir is
// where udev creates the character device. They are vars, not consts, so a
// test can point the package at a fake tree. Nothing in production reassigns
// them.
var (
	ClassDir = "/sys/class/zcrypt"
	NodeDir  = "/sys/devices/virtual/zcrypt"
	DevDir   = "/dev"
)

const (
	// NamePrefix marks every node this driver creates, so recovery can find
	// its own nodes and never touches nodes an administrator made with
	// zcryptctl.
	NamePrefix = "cexdra-"

	// MaxNameLen is the longest name the kernel keeps: zcdn_create_store
	// copies the name into a ZCDN_MAX_NAME (32) byte buffer including the
	// terminating NUL, silently truncating anything longer.
	MaxNameLen = 31

	// MaskBits is the width of every per-node mask: AP_DEVICES, AP_DOMAINS
	// and AP_IOCTLS are all 256 on the kernels this driver supports.
	MaskBits = 256

	// sysfsWriteMode is the mode passed to os.WriteFile for sysfs
	// attributes. The kernel ignores it. 0o600 satisfies gosec G306.
	sysfsWriteMode os.FileMode = 0o600
)

// Perms is the set of AP resources and ioctls a node may address. Adapters
// are AP adapter ids (apid), domains are AP queue indexes (apqi), all as
// plain numbers in [0, MaskBits).
type Perms struct {
	Adapters       []int
	UsageDomains   []int
	ControlDomains []int
	Ioctls         []int
}

// AllIoctls returns every ioctl number, the setting the CEX device plug-in
// uses for its per-container nodes. The adapter and domain masks still bound
// what those ioctls can reach.
func AllIoctls() []int {
	out := make([]int, MaskBits)
	for i := range out {
		out[i] = i
	}
	return out
}

// NodeName derives the node name for a claim: NamePrefix followed by the
// claim UID's hex digits, cut to MaxNameLen. 24 hex digits (96 bits) keep
// names of distinct claims apart in practice while fitting the kernel limit.
func NodeName(claimUID string) string {
	hex := strings.ReplaceAll(strings.ToLower(claimUID), "-", "")
	name := NamePrefix + hex
	if len(name) > MaxNameLen {
		name = name[:MaxNameLen]
	}
	return name
}

// ValidateName rejects names this package does not manage: anything without
// NamePrefix, longer than MaxNameLen, or outside [a-z0-9-]. The kernel itself
// accepts more, but every name this package writes into sysfs passes here
// first, so a bad name can neither be truncated by the kernel nor escape
// NodeDir as a path.
func ValidateName(name string) error {
	if !strings.HasPrefix(name, NamePrefix) || len(name) == len(NamePrefix) {
		return fmt.Errorf("zcrypt node name %q: want prefix %q and a non-empty suffix", name, NamePrefix)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("zcrypt node name %q: %d characters, the kernel keeps at most %d", name, len(name), MaxNameLen)
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("zcrypt node name %q: character %q outside [a-z0-9-]", name, c)
		}
	}
	return nil
}

// Supported reports whether the kernel offers additional zcrypt nodes.
func Supported() bool {
	_, err := os.Stat(filepath.Join(ClassDir, "create"))
	return err == nil
}

// Exists reports whether a node with the given name exists.
func Exists(name string) bool {
	_, err := os.Stat(filepath.Join(NodeDir, name))
	return err == nil
}

// DevPath returns the character device path udev creates for a node.
func DevPath(name string) string {
	return filepath.Join(DevDir, name)
}

// DevNumber returns the major and minor number of a node's character device,
// read from the node's sysfs "dev" attribute ("<major>:<minor>"). The
// attribute exists as soon as Create returns, unlike the /dev entry udev adds
// later, so a caller can describe the device to a container runtime without
// waiting for udev or seeing the host's /dev.
func DevNumber(name string) (major, minor int64, err error) {
	if err := ValidateName(name); err != nil {
		return 0, 0, err
	}
	path := filepath.Join(NodeDir, name, "dev")
	// G304: path is NodeDir/<name>/dev and name passed ValidateName above.
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return 0, 0, fmt.Errorf("read dev of zcrypt node %s: %w", name, err)
	}
	majStr, minStr, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
	if !ok {
		return 0, 0, fmt.Errorf("zcrypt node %s: malformed dev %q, want <major>:<minor>", name, strings.TrimSpace(string(raw)))
	}
	if major, err = strconv.ParseInt(majStr, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("zcrypt node %s: major in dev %q: %w", name, strings.TrimSpace(string(raw)), err)
	}
	if minor, err = strconv.ParseInt(minStr, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("zcrypt node %s: minor in dev %q: %w", name, strings.TrimSpace(string(raw)), err)
	}
	return major, minor, nil
}

// Create asks the kernel for a new node. A node starts with every mask empty,
// so it reaches nothing until Restrict grants it resources. Create is not
// idempotent: the kernel rejects an existing name with EEXIST, which the
// returned error wraps, so a caller that retries checks Exists first.
func Create(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	logphase.Logf(logphase.Preparation, "Creating zcrypt node %s", name)
	if err := os.WriteFile(filepath.Join(ClassDir, "create"), []byte(name), sysfsWriteMode); err != nil {
		return fmt.Errorf("create zcrypt node %s: %w", name, err)
	}
	return nil
}

// Restrict sets all four masks of a node to exactly perms. Each mask is
// written in the kernel's absolute form ("0x" + hex), which replaces the
// whole mask rather than adding to it, so Restrict is idempotent and a retry
// after a partial failure converges on the same state. An empty list clears
// its mask. The ioctl mask is written last: until it is set the node accepts
// no ioctl, so a failure part-way leaves a node that cannot be used.
func Restrict(name string, perms Perms) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	for _, m := range []struct {
		attr string
		ids  []int
	}{
		{"apmask", perms.Adapters},
		{"aqmask", perms.UsageDomains},
		{"admask", perms.ControlDomains},
		{"ioctlmask", perms.Ioctls},
	} {
		value, err := FormatMask(m.ids)
		if err != nil {
			return fmt.Errorf("zcrypt node %s %s: %w", name, m.attr, err)
		}
		path := filepath.Join(NodeDir, name, m.attr)
		if err := os.WriteFile(path, []byte(value), sysfsWriteMode); err != nil {
			return fmt.Errorf("write %s of zcrypt node %s: %w", m.attr, name, err)
		}
	}
	logphase.Logf(logphase.Preparation, "Restricted zcrypt node %s to adapters %v, usage domains %v, control domains %v, %d ioctls",
		name, perms.Adapters, perms.UsageDomains, perms.ControlDomains, len(perms.Ioctls))
	return nil
}

// ReadPerms reads a node's four masks back from sysfs. It is the
// restart-surviving record of what a node was granted, the counterpart to
// mdev.ReadMatrix for the vfio-ap path.
func ReadPerms(name string) (Perms, error) {
	if err := ValidateName(name); err != nil {
		return Perms{}, err
	}
	var p Perms
	for _, m := range []struct {
		attr string
		dst  *[]int
	}{
		{"apmask", &p.Adapters},
		{"aqmask", &p.UsageDomains},
		{"admask", &p.ControlDomains},
		{"ioctlmask", &p.Ioctls},
	} {
		path := filepath.Join(NodeDir, name, m.attr)
		// G304: path is NodeDir/<name>/<fixed attribute>, and name passed
		// ValidateName above, so it cannot leave NodeDir.
		raw, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return Perms{}, fmt.Errorf("read %s of zcrypt node %s: %w", m.attr, name, err)
		}
		ids, err := ParseMask(string(raw))
		if err != nil {
			return Perms{}, fmt.Errorf("parse %s of zcrypt node %s: %w", m.attr, name, err)
		}
		*m.dst = ids
	}
	return p, nil
}

// Destroy removes a node. Destroying a node that does not exist succeeds, so
// cleanup can run again after a crash without special-casing.
func Destroy(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if !Exists(name) {
		logphase.Logf(logphase.Preparation, "zcrypt node %s does not exist (idempotent)", name)
		return nil
	}
	logphase.Logf(logphase.Preparation, "Destroying zcrypt node %s", name)
	if err := os.WriteFile(filepath.Join(ClassDir, "destroy"), []byte(name), sysfsWriteMode); err != nil {
		return fmt.Errorf("destroy zcrypt node %s: %w", name, err)
	}
	return nil
}

// List returns the names of the nodes this driver created, sorted, ignoring
// nodes made by anyone else. A missing NodeDir means no node exists.
func List() ([]string, error) {
	entries, err := os.ReadDir(NodeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list zcrypt nodes: %w", err)
	}
	var out []string
	for _, e := range entries {
		if ValidateName(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// WaitDevNode waits until udev has created the character device of a node.
// The sysfs node exists as soon as Create returns, but the /dev entry appears
// asynchronously. ctx bounds the wait.
func WaitDevNode(ctx context.Context, name string, interval time.Duration) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	path := DevPath(name)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

// FormatMask renders ids as the kernel's absolute mask form: "0x" followed by
// hex digits, bit 0 being the most significant bit of the first digit (the AP
// bus numbers bits from the left). Only the digits up to the highest set bit
// are written: the kernel zeroes the mask before applying an absolute value,
// so the omitted tail reads as clear. An empty list yields "0x0".
func FormatMask(ids []int) (string, error) {
	highest := -1
	for _, id := range ids {
		if id < 0 || id >= MaskBits {
			return "", fmt.Errorf("id %d outside [0, %d)", id, MaskBits)
		}
		if id > highest {
			highest = id
		}
	}
	if highest < 0 {
		return "0x0", nil
	}
	digits := make([]byte, highest/4+1)
	for _, id := range ids {
		digits[id/4] |= 0x8 >> (id % 4)
	}
	const hexDigits = "0123456789abcdef"
	var b strings.Builder
	b.WriteString("0x")
	for _, d := range digits {
		b.WriteByte(hexDigits[d])
	}
	return b.String(), nil
}

// ParseMask is the inverse of FormatMask for the form the kernel shows:
// "0x" followed by hex digits, bit 0 leftmost. It returns the set bits in
// ascending order.
func ParseMask(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return nil, errors.New("empty mask")
	}
	if len(s)*4 > MaskBits {
		return nil, fmt.Errorf("mask has %d bits, want at most %d", len(s)*4, MaskBits)
	}
	var out []int
	for i, c := range s {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return nil, fmt.Errorf("invalid hex digit %q in mask", c)
		}
		for bit := 0; bit < 4; bit++ {
			if v&(0x8>>bit) != 0 {
				out = append(out, i*4+bit)
			}
		}
	}
	return out, nil
}
