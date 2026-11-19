package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchStateKeepsInstanceIdentity(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "demo")
	process := startTestProcess(t, firecrackerArgs(dir))
	state := process.state
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := openFetchSession(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if first.state.InstanceID != state.InstanceID {
		t.Fatalf("fetch identity = %q; want %q", first.state.InstanceID, state.InstanceID)
	}
	assertFileContents(t, filepath.Join(dir, "state.json"), before)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, lock, err := lockState(ctx, "other")
	if err != nil {
		t.Fatal("fetch retained global lifecycle lock", err)
	}
	lock.Close()
	if err := first.recheck(); err != nil {
		t.Fatal(err)
	}
	state.InstanceID = strings.Repeat("b", 32)
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := first.recheck(); err == nil {
		t.Fatal("changed instance identity accepted")
	}
	process.assertAlive(t)
}

func TestLifecycleRejectsRecordWithoutInstanceID(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "demo")
	writeTestFile(t, filepath.Join(dir, "rootfs.ext4"), []byte("persistent disk"))
	t.Setenv("HABIKAT_VM_TEST_DISK", filepath.Join(dir, "rootfs.ext4"))
	process := startTestProcess(t, firecrackerArgs(dir))
	state := process.state
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	data := recordWithoutField(t, state, "instance_id")
	writeTestFile(t, filepath.Join(dir, "state.json"), data)
	session, err := openFetchSession(t.Context(), "demo")
	if session != nil {
		session.Close()
	}
	assertErrorContains(t, err, "unsupported or corrupt VM state")
	assertErrorContains(t, Stop(t.Context(), "demo", false), "unsupported or corrupt VM state")
	process.assertAlive(t)
	assertFileContents(t, filepath.Join(dir, "state.json"), data)
	assertFileContents(t, filepath.Join(dir, "rootfs.ext4"), []byte("persistent disk"))
}

func TestFetchStateFailuresPreserveDisk(t *testing.T) {
	for _, kind := range []string{"stopped", "empty", "unready", "missing", "invalid project"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			if kind == "missing" {
				if _, err := openFetchSession(t.Context(), "demo"); err == nil || !strings.Contains(err.Error(), "VM demo not found") {
					t.Fatalf("missing VM: %v", err)
				}
				return
			}
			dir := makeVMDir(t, root, "demo")
			state := currentRecord(t)
			if kind != "stopped" {
				process := startTestProcess(t, firecrackerArgs(dir))
				state = process.state
				state.Ready = kind != "unready"
			}
			if kind != "empty" {
				state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
			}
			if kind == "invalid project" {
				state.Project.Workspace = "/other"
			}
			if err := saveRecord(dir, state); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(dir, "rootfs.ext4"), []byte("keep disk"))
			writeTestFile(t, filepath.Join(dir, "identity"), []byte("keep keys"))
			before, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := openFetchSession(t.Context(), "demo"); err == nil {
				t.Fatal("unsuitable VM accepted")
			} else if kind == "stopped" && (!strings.Contains(err.Error(), "is stopped") || !strings.Contains(err.Error(), "preserved") || !strings.Contains(err.Error(), "previously fetched refs")) {
				t.Fatalf("unclear stopped error: %v", err)
			}
			assertFileContents(t, filepath.Join(dir, "rootfs.ext4"), []byte("keep disk"))
			assertFileContents(t, filepath.Join(dir, "identity"), []byte("keep keys"))
			assertFileContents(t, filepath.Join(dir, "state.json"), before)
		})
	}
}

func TestFetchStateReplacement(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "demo")
	process := startTestProcess(t, firecrackerArgs(dir))
	state := process.state
	state.Ready = true
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	session, err := openFetchSession(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	makeVMDir(t, root, "demo")
	if err := saveRecord(dir, session.state); err != nil {
		t.Fatal(err)
	}
	if err := session.recheck(); err == nil {
		t.Fatal("replaced directory accepted")
	}
}

func TestInstanceIDsDiffer(t *testing.T) {
	first, err := newInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newInstanceID()
	if err != nil || first == second || !validInstanceID.MatchString(second) {
		t.Fatalf("unstable identity %q %q %v", first, second, err)
	}
}

func TestFetchPublicationReleasesLockBeforeApproval(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "demo")
	process := startTestProcess(t, firecrackerArgs(dir))
	state := process.state
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	session, err := openFetchSession(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	installFetchSSH(t, `printf 'HKSTATUS1\n M changed\000'`)
	changes, err := publishFetch(t.Context(), session, nil, false)
	if !errors.Is(err, errFetchGuestBecameDirty) || !changes.Unstaged {
		t.Fatalf("new dirty guest bypassed approval: %+v %v", changes, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, lock, err := lockState(ctx, "sibling")
	if err != nil {
		t.Fatal("global lock held across approval", err)
	}
	lock.Close()
}
