package vm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProcessOwnsConfigRequiresEveryArgument(t *testing.T) {
	root := privateTempDir(t)
	dir := makeVMDir(t, root, "vm")
	other := makeVMDir(t, root, "other")
	config, socket := filepath.Join(dir, "config.json"), filepath.Join(dir, "api.sock")
	for _, tc := range []struct {
		name  string
		args  []string
		owned bool
	}{
		{"no api", []string{"firecracker", "--no-api", "--config-file", config}, false},
		{"api", []string{"firecracker", "--api-sock", socket, "--config-file", config}, !nixosExperiment},
		{"pci", []string{"firecracker", "--api-sock", socket, "--config-file", config, "--enable-pci"}, nixosExperiment},
		{"duplicate pci", []string{"firecracker", "--api-sock", socket, "--config-file", config, "--enable-pci", "--enable-pci"}, false},
		{"pci value", []string{"firecracker", "--api-sock", socket, "--config-file", config, "--enable-pci=true"}, false},
		{"pci plus extra", []string{"firecracker", "--api-sock", socket, "--config-file", config, "--enable-pci", "--no-api"}, false},
		{"api wrong socket flag", []string{"firecracker", "--api-socket", socket, "--config-file", config}, false},
		{"api other socket", []string{"firecracker", "--api-sock", filepath.Join(other, "api.sock"), "--config-file", config}, false},
		{"api wrong config flag", []string{"firecracker", "--api-sock", socket, "--config", config}, false},
		{"api other config", []string{"firecracker", "--api-sock", socket, "--config-file", filepath.Join(other, "config.json")}, false},
		{"api extra argument", []string{"firecracker", "--api-sock", socket, "--config-file", config, "--extra"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := startTestProcess(t, tc.args)
			owned, err := processOwnsConfig(child.state.PID, dir)
			if err != nil || owned != tc.owned {
				t.Fatalf("processOwnsConfig(%q) = %t, %v; want %t", tc.args, owned, err, tc.owned)
			}
		})
	}
}

func TestSameProcessRejectsOtherBoot(t *testing.T) {
	dir := makeVMDir(t, privateTempDir(t), "vm")
	child := startTestProcess(t, firecrackerArgs(dir))
	state := child.state
	var err error
	state.BootID, err = hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	if !sameProcess(state, dir) {
		t.Fatal("live process from this boot not recognised")
	}
	state.BootID = "00000000-0000-0000-0000-000000000000"
	if sameProcess(state, dir) {
		t.Fatal("process recorded during another boot treated as the same process")
	}
}

func TestStopRefusesRecycledPID(t *testing.T) {
	dir, child, state := stopFixture(t)
	state.StartTime = "1"
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	assertErrorContains(t, Stop(t.Context(), "vm", true), "identity changed")
	child.assertAlive(t)
	current, err := readRecord(dir)
	if err != nil || current.StartTime != "1" || current.Stopping {
		t.Fatalf("refused stop changed state: %+v %v", current, err)
	}
}

func TestDeleteStopsLiveVMFromThisBoot(t *testing.T) {
	dir, child, state := stopFixture(t)
	if state.BootID == "" {
		t.Fatal("fixture must record the host boot identity")
	}
	if err := Delete(t.Context(), "vm", nil); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("storage kept after deleting a stopped VM: %v", err)
	}
}

func TestLifecycleLockRejectsUnsafeFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"group readable", func(t *testing.T, path string) {
			writeTestFile(t, path, nil)
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard linked", func(t *testing.T, path string) {
			writeTestFile(t, path, nil)
			if err := os.Link(path, path+".link"); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := makeVMDir(t, privateTempDir(t), "vm")
			tc.plant(t, filepath.Join(dir, ".lifecycle"))
			operation, err := lockVMOperation(dir)
			if err == nil {
				_ = operation.Close()
			}
			assertErrorContains(t, err, "invalid VM lifecycle lock")
		})
	}
}
