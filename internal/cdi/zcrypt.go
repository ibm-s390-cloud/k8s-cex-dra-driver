package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"k8s-cex-dra-driver/internal/logphase"
)

const (
	zcryptClass = "zcrypt"
	zcryptKind  = cdiVendor + "/" + zcryptClass

	// ZcryptContainerPath is where a container claim's zcrypt node appears
	// inside the container. It is the path libica, openCryptoki and the
	// s390-tools open by default, so workloads need no configuration.
	ZcryptContainerPath = "/dev/z90crypt"

	// zcryptFileMode is the mode of the device node inside the container.
	// udev creates the host node 0600 root:root, which a workload running as
	// a non-root user cannot open. Access is bounded by the node's masks -
	// the claim's adapters, domains and ioctls - not by the file mode, so the
	// node is world read-writable inside the container, like /dev/z90crypt
	// on a host.
	zcryptFileMode os.FileMode = 0o666
)

// ZcryptSpec is what a container claim's CDI spec carries: the zcrypt node's
// device number, the APQNs its masks grant, and the read-only bind mounts
// that replace the container's AP sysfs with the claim's shadow tree.
type ZcryptSpec struct {
	ClaimUID string
	Major    int64
	Minor    int64
	APQNs    []string
	Mounts   []ReadOnlyMount
}

// ReadOnlyMount is a host directory bind-mounted read-only into the
// container.
type ReadOnlyMount struct {
	HostPath      string
	ContainerPath string
}

// readOnlyBindOptions are the options of every ReadOnlyMount. The shadow
// trees hold plain files and relative symlinks only, so nothing inside needs
// to execute, and no device node may appear there.
var readOnlyBindOptions = []string{"ro", "rbind", "nosuid", "nodev", "noexec"}

// GenerateZcryptClaimSpec writes the CDI spec that maps a container claim's
// zcrypt node to ZcryptContainerPath, adds the claim's read-only mounts, and
// returns the fully-qualified CDI device ID. The node is described by type and device number rather than by
// host path, so the runtime creates it inside the container without looking
// the node up in the host's /dev: udev adds that entry asynchronously and the
// plugin container does not mount the host's /dev.
func GenerateZcryptClaimSpec(cdiRoot string, zs ZcryptSpec) (string, error) {
	mode := zcryptFileMode
	mounts := make([]*cdispec.Mount, 0, len(zs.Mounts))
	for _, m := range zs.Mounts {
		mounts = append(mounts, &cdispec.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Type:          "bind",
			Options:       readOnlyBindOptions,
		})
	}
	spec := &cdispec.Spec{
		Version: cdiVersion,
		Kind:    zcryptKind,
		Devices: []cdispec.Device{
			{
				Name: zs.ClaimUID,
				ContainerEdits: cdispec.ContainerEdits{
					DeviceNodes: []*cdispec.DeviceNode{
						{
							Path:        ZcryptContainerPath,
							Type:        "c",
							Major:       zs.Major,
							Minor:       zs.Minor,
							FileMode:    &mode,
							Permissions: "rw",
						},
					},
					Env:    []string{"CEX_APQNS=" + strings.Join(zs.APQNs, ",")},
					Mounts: mounts,
				},
			},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal CDI spec: %w", err)
	}
	if err := os.MkdirAll(cdiRoot, cdiDirMode); err != nil {
		return "", fmt.Errorf("create CDI directory %s: %w", cdiRoot, err)
	}
	filePath := zcryptSpecPath(cdiRoot, zs.ClaimUID)
	if err := os.WriteFile(filePath, data, cdiSpecFileMode); err != nil {
		return "", fmt.Errorf("write CDI spec %s: %w", filePath, err)
	}

	deviceID := fmt.Sprintf("%s=%s", zcryptKind, zs.ClaimUID)
	logphase.Logf(logphase.Preparation, "CDI spec written: %s (device ID: %s, zcrypt node %d:%d, APQNs %v, %d read-only mounts)",
		filePath, deviceID, zs.Major, zs.Minor, zs.APQNs, len(mounts))
	return deviceID, nil
}

// DeleteZcryptClaimSpec removes a container claim's CDI spec. A missing file
// is not an error, so it is safe to call for any claim.
func DeleteZcryptClaimSpec(cdiRoot, claimUID string) error {
	filePath := zcryptSpecPath(cdiRoot, claimUID)
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove CDI spec %s: %w", filePath, err)
	}
	return nil
}

// ZcryptClaimSpecUIDs returns the claim UIDs that have a zcrypt CDI spec in
// cdiRoot. Only files matching this driver's vendor-class prefix are
// considered, because cdiRoot is shared with other CDI producers.
func ZcryptClaimSpecUIDs(cdiRoot string) ([]string, error) {
	prefix := zcryptSpecPrefix()
	matches, err := filepath.Glob(filepath.Join(cdiRoot, prefix+"*.json"))
	if err != nil {
		return nil, fmt.Errorf("glob zcrypt CDI specs under %s: %w", cdiRoot, err)
	}
	var uids []string
	for _, p := range matches {
		uid := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), prefix), ".json")
		if uid != "" {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// GCStaleZcryptClaimSpecs removes zcrypt CDI specs whose node no longer
// exists, the container-path twin of GCStaleClaimSpecs. Best-effort per file.
// An error is returned only when the listing itself fails.
func GCStaleZcryptClaimSpecs(cdiRoot string, live func(claimUID string) bool) error {
	uids, err := ZcryptClaimSpecUIDs(cdiRoot)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if live(uid) {
			continue
		}
		if err := DeleteZcryptClaimSpec(cdiRoot, uid); err != nil {
			logphase.Warnf(logphase.Preparation, "%v", err)
			continue
		}
		logphase.Logf(logphase.Preparation, "removed stale zcrypt CDI spec for claim %s (no zcrypt node)", uid)
	}
	return nil
}

func zcryptSpecPrefix() string {
	return cdiVendor + "-" + zcryptClass + "-"
}

func zcryptSpecPath(cdiRoot, claimUID string) string {
	return filepath.Join(cdiRoot, zcryptSpecPrefix()+claimUID+".json")
}
