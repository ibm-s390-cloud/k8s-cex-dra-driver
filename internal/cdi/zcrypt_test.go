package cdi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

func TestGenerateZcryptClaimSpec(t *testing.T) {
	root := t.TempDir()
	id, err := GenerateZcryptClaimSpec(root, ZcryptSpec{
		ClaimUID: testUUID,
		Major:    246,
		Minor:    3,
		APQNs:    []string{"06.0000", "07.0000"},
		Mounts: []ReadOnlyMount{
			{HostPath: "/run/shadow/uid/bus/ap", ContainerPath: "/sys/bus/ap"},
			{HostPath: "/run/shadow/uid/devices/ap", ContainerPath: "/sys/devices/ap"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateZcryptClaimSpec: %v", err)
	}
	if want := "ibm.com/zcrypt=" + testUUID; id != want {
		t.Errorf("device ID = %q, want %q", id, want)
	}

	raw, err := os.ReadFile(filepath.Join(root, "ibm.com-zcrypt-"+testUUID+".json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec cdispec.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.Kind != "ibm.com/zcrypt" || len(spec.Devices) != 1 || spec.Devices[0].Name != testUUID {
		t.Fatalf("spec kind/devices = %q/%+v", spec.Kind, spec.Devices)
	}
	edits := spec.Devices[0].ContainerEdits
	if len(edits.DeviceNodes) != 1 {
		t.Fatalf("device nodes = %d, want 1", len(edits.DeviceNodes))
	}
	dn := edits.DeviceNodes[0]
	if dn.Path != "/dev/z90crypt" || dn.Type != "c" || dn.Major != 246 || dn.Minor != 3 {
		t.Errorf("device node = %+v, want /dev/z90crypt c 246:3", dn)
	}
	if dn.HostPath != "" {
		t.Errorf("hostPath = %q, want empty: the node is described by device number", dn.HostPath)
	}
	if dn.FileMode == nil || *dn.FileMode != 0o666 {
		t.Errorf("fileMode = %v, want 0666 so non-root workloads can open the node", dn.FileMode)
	}
	if !reflect.DeepEqual(edits.Env, []string{"CEX_APQNS=06.0000,07.0000"}) {
		t.Errorf("env = %q", edits.Env)
	}
	if len(edits.Mounts) != 2 {
		t.Fatalf("mounts = %d, want 2", len(edits.Mounts))
	}
	for i, want := range [][2]string{
		{"/run/shadow/uid/bus/ap", "/sys/bus/ap"},
		{"/run/shadow/uid/devices/ap", "/sys/devices/ap"},
	} {
		m := edits.Mounts[i]
		if m.HostPath != want[0] || m.ContainerPath != want[1] || m.Type != "bind" {
			t.Errorf("mount %d = %+v, want bind %s -> %s", i, m, want[0], want[1])
		}
		if len(m.Options) == 0 || m.Options[0] != "ro" {
			t.Errorf("mount %d options = %q, want read-only", i, m.Options)
		}
	}
}

func TestDeleteZcryptClaimSpecIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := GenerateZcryptClaimSpec(root, ZcryptSpec{ClaimUID: testUUID, Major: 1, Minor: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := DeleteZcryptClaimSpec(root, testUUID); err != nil {
			t.Fatalf("DeleteZcryptClaimSpec #%d: %v", i+1, err)
		}
	}
	if uids, _ := ZcryptClaimSpecUIDs(root); len(uids) != 0 {
		t.Errorf("specs left = %q, want none", uids)
	}
}

func TestGCStaleZcryptClaimSpecs(t *testing.T) {
	root := t.TempDir()
	for _, uid := range []string{"live-claim", "dead-claim"} {
		if _, err := GenerateZcryptClaimSpec(root, ZcryptSpec{ClaimUID: uid, Major: 1, Minor: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// A vfio-ap spec and a foreign vendor's spec share the directory and must
	// survive untouched.
	for _, name := range []string{"ibm.com-vfio-ap-passthrough-dead-claim.json", "nvidia.com-gpu-x.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := GCStaleZcryptClaimSpecs(root, func(uid string) bool { return uid == "live-claim" }); err != nil {
		t.Fatalf("GC: %v", err)
	}

	uids, err := ZcryptClaimSpecUIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uids, []string{"live-claim"}) {
		t.Errorf("zcrypt specs left = %q, want [live-claim]", uids)
	}
	entries, _ := os.ReadDir(root)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"ibm.com-vfio-ap-passthrough-dead-claim.json", "ibm.com-zcrypt-live-claim.json", "nvidia.com-gpu-x.json"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("files = %q, want %q", names, want)
	}
}
