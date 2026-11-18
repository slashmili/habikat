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

func TestFetchStateLegacyIdentity(t *testing.T) {
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
	first, err := openFetchSession(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if !validInstanceID.MatchString(first.state.InstanceID) {
		t.Fatal("missing valid stable identity")
	}
	second, err := openFetchSession(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.state.InstanceID != second.state.InstanceID {
		t.Fatal("fetch identity changed")
	}
	stored, err := readRecord(dir)
	if err != nil || stored.InstanceID != first.state.InstanceID || *stored.Project != *state.Project {
		t.Fatalf("legacy state was not preserved: %+v %v", stored, err)
	}
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
	stored.InstanceID = strings.Repeat("b", 32)
	if stored.InstanceID == first.state.InstanceID {
		stored.InstanceID = strings.Repeat("c", 32)
	}
	if err := saveRecord(dir, stored); err != nil {
		t.Fatal(err)
	}
	if err := first.recheck(); err == nil {
		t.Fatal("changed instance identity accepted")
	}
	process.assertAlive(t)
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
			state := record{}
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
	dir := privateTempDir(t)
	data := []byte(`{"pid":0,"start_time":"","ready":false,"project":null}`)
	writeTestFile(t, filepath.Join(dir, "state.json"), data)
	state, err := readRecord(dir)
	if err != nil || state.InstanceID != "" {
		t.Fatalf("legacy record failed: %+v %v", state, err)
	}
	_, err = ensureInstanceID(filepath.Join(dir, "absent"), state)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed persistence hidden: %v", err)
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

func TestConcurrentLegacyFetchIdentity(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "demo")
	process := startTestProcess(t, firecrackerArgs(dir))
	state := process.state
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 8)
	for range 8 {
		go func() {
			session, err := openFetchSession(t.Context(), "demo")
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{id: session.state.InstanceID, err: session.Close()}
		}()
	}
	var expected string
	for range 8 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if expected == "" {
			expected = got.id
		}
		if got.id != expected {
			t.Fatal("concurrent fetches assigned different IDs")
		}
	}
}
