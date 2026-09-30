package devicestate

import (
	"os"
	"testing"

	"k8s-cex-dra-driver/internal/shadowsysfs"
)

// TestMain points the shadow sysfs base at a scratch directory for the whole
// package, so no test - Unprepare runs its idempotent removal for every
// claim - ever touches the host's /var/run, and disables SELinux labelling,
// which a developer machine may refuse.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cex-dra-shadowsysfs-")
	if err != nil {
		panic(err)
	}
	shadowsysfs.BaseDir = dir
	shadowsysfs.SELinuxLabel = ""
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
