package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func stopFixture(t *testing.T, api bool) (string, *testProcess, record) {
	t.Helper()
	return stopProcessFixture(t, api, true)
}

func stopProcessFixture(t *testing.T, api, holdsDisk bool) (string, *testProcess, record) {
	t.Helper()
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	args := firecrackerArgs(dir)
	if api {
		args = []string{"firecracker", "--api-sock", filepath.Join(dir, "api.sock"), "--config-file", filepath.Join(dir, "config.json")}
	}
	for _, file := range []string{"rootfs.ext4", "seed.ext4", "identity", "known_hosts"} {
		writeTestFile(t, filepath.Join(dir, file), []byte("preserved "+file))
	}
	disk := ""
	if holdsDisk {
		disk = filepath.Join(dir, "rootfs.ext4")
	}
	t.Setenv("HABIKAT_VM_TEST_DISK", disk)
	child := startTestProcess(t, args)
	state := child.state
	var err error
	state.BootID, err = hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	state.InstanceID = strings.Repeat("1", 32)
	if api {
		state.Shutdown = "ctrl-alt-del-v1"
	}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	return dir, child, state
}

func assertStopStorage(t *testing.T, dir string, state record) {
	t.Helper()
	for _, file := range []string{"rootfs.ext4", "seed.ext4", "identity", "known_hosts"} {
		assertFileContents(t, filepath.Join(dir, file), []byte("preserved "+file))
	}
	current, err := readRecord(dir)
	if err != nil || current.InstanceID != state.InstanceID {
		t.Fatalf("identity changed: %+v %v", current, err)
	}
}

func TestStopGracefulWaitsForExit(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	err := stopVM(t.Context(), "vm", false, time.Second, func(ctx context.Context, got record, gotDir string) error {
		if gotDir != dir || got.PID != state.PID {
			t.Fatal("wrong shutdown target")
		}
		assertLifecycleLockAvailable(t)
		child.assertAlive(t)
		return child.cmd.Process.Signal(syscall.SIGTERM)
	})
	if err != nil {
		t.Fatal(err)
	}
	child.await(t)
	current, err := readRecord(dir)
	if err != nil || current.PID != 0 || current.Ready || current.Stopping {
		t.Fatalf("not stopped: %+v %v", current, err)
	}
	assertStopStorage(t, dir, state)
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeFile, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", false); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(dir, "state.json"), before)
	afterFile, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || !os.SameFile(beforeFile, afterFile) {
		t.Fatalf("stopping a stopped VM rewrote its state: %v", err)
	}
}

func TestStopAcknowledgmentIsNotExit(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	err := stopVM(t.Context(), "vm", false, 30*time.Millisecond, func(context.Context, record, string) error { return nil })
	assertErrorContains(t, err, "may still complete")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error: %v", err)
	}
	child.assertAlive(t)
	current, err := readRecord(dir)
	if err != nil || current.PID != state.PID || !current.Stopping {
		t.Fatalf("lost live identity: %+v %v", current, err)
	}
	assertStopStorage(t, dir, state)
}

func TestStopCancellationAfterRequest(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := stopVM(ctx, "vm", false, time.Second, func(context.Context, record, string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	assertErrorContains(t, err, "may still complete")
	child.assertAlive(t)
	assertStopStorage(t, dir, state)
}

func TestStopForcePreservesStorage(t *testing.T) {
	dir, child, state := stopFixture(t, false)
	assertErrorContains(t, Stop(t.Context(), "vm", false), "does not support graceful shutdown")
	child.assertAlive(t)
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	assertStopStorage(t, dir, state)
}

func TestStopMissingDoesNotCreateState(t *testing.T) {
	root := filepath.Join(privateTempDir(t), "missing")
	t.Setenv("HK_STATE_DIR", root)
	if err := Stop(t.Context(), "vm", false); err == nil {
		t.Fatal("missing VM accepted")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created missing state root: %v", err)
	}
}

func TestStopRejectsConflictingLifecycle(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	err := stopVM(t.Context(), "vm", false, time.Second, func(context.Context, record, string) error {
		assertErrorContains(t, Stop(t.Context(), "vm", true), "lifecycle operation")
		assertErrorContains(t, Delete(t.Context(), "vm", nil), "lifecycle operation")
		assertStopStorage(t, dir, state)
		return child.cmd.Process.Signal(syscall.SIGTERM)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStopRefusesForeignBootProcess(t *testing.T) {
	dir, child, state := stopFixture(t, false)
	state.BootID = "00000000-0000-0000-0000-000000000000"
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", true); err == nil {
		t.Fatal("ambiguous live process accepted")
	}
	child.assertAlive(t)
	assertStopStorage(t, dir, state)
}

func FuzzStopPreservesData(f *testing.F) {
	for _, data := range []string{`{}`, `null`, `{"pid":-1}`, `{"pid":0,"ready":true}`, `{"instance_id":"bad"}`, `{"pid":0,"start_time":""}`} {
		f.Add(data, false)
		f.Add(data, true)
	}
	f.Fuzz(func(t *testing.T, data string, force bool) {
		if len(data) > 4096 {
			t.Skip()
		}
		root := privateTempDir(t)
		t.Setenv("HK_STATE_DIR", root)
		dir := makeVMDir(t, root, "vm")
		writeTestFile(t, filepath.Join(dir, "state.json"), []byte(data))
		writeTestFile(t, filepath.Join(dir, "rootfs.ext4"), []byte("valuable"))
		_ = Stop(t.Context(), "vm", force)
		assertFileContents(t, filepath.Join(dir, "rootfs.ext4"), []byte("valuable"))
	})
}

func TestStopDoesNotOverwriteLinkedTemporaryFile(t *testing.T) {
	dir, child, state := stopFixture(t, false)
	sentinel := filepath.Join(privateTempDir(t), "valuable")
	writeTestFile(t, sentinel, []byte("keep"))
	if err := os.Symlink(sentinel, filepath.Join(dir, "state.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	assertFileContents(t, sentinel, []byte("keep"))
	assertStopStorage(t, dir, state)
}

func TestStopRejectsMalformedLifecycleState(t *testing.T) {
	for _, data := range []string{`{"shutdown":"unknown"}`, `{"stopping":true,"pid":0}`} {
		if _, err := decodeRecord([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestStopRefusesSymlinkRecordAndSocket(t *testing.T) {
	dir, child, _ := stopFixture(t, true)
	state := filepath.Join(dir, "state.json")
	backup := filepath.Join(dir, "state.backup")
	if err := os.Rename(state, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, state); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", true); err == nil {
		t.Fatal("symlink state accepted")
	}
	child.assertAlive(t)
}

func TestForceDeleteRefusesConflictingBootIdentity(t *testing.T) {
	dir, child, _ := stopFixture(t, false)
	state := child.state
	state.BootID = "00000000-0000-0000-0000-000000000000"
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := Delete(t.Context(), "vm", nil); err == nil {
		t.Fatal("deleted live VM whose boot identity disagrees")
	}
	child.assertAlive(t)
	assertFileContents(t, filepath.Join(dir, "rootfs.ext4"), []byte("preserved rootfs.ext4"))
}

func TestStoppingVMRejectsFetch(t *testing.T) {
	dir, _, state := stopFixture(t, true)
	state.Stopping = true
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := requireFetchSource("vm", dir, state); err == nil {
		t.Fatal("fetch accepted stopping source")
	}
	next := state
	next.BootID = "00000000-0000-0000-0000-000000000000"
	if sameFetchIdentity(state, next) {
		t.Fatal("fetch identity ignored host boot identity")
	}
}

func TestAttachStoppedDoesNotSuggestDiscardingWork(t *testing.T) {
	dir, child, state := stopFixture(t, false)
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	err := Attach("vm")
	if err == nil || strings.Contains(err.Error(), "delete") || !strings.Contains(err.Error(), "preserved") {
		t.Fatalf("unsafe recovery advice: %v", err)
	}
	if !strings.Contains(err.Error(), "is stopped") || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("stopped VM implies it can be resumed: %v", err)
	}
	if strings.Contains(err.Error(), "restart") || strings.Contains(err.Error(), "recreate") {
		t.Fatalf("unsupported recovery advice: %v", err)
	}
	assertStopStorage(t, dir, state)
}

func TestUnavailableVMExplainsState(t *testing.T) {
	dir, _, state := stopFixture(t, true)
	for _, tc := range []struct {
		name   string
		change func(*record)
		want   string
	}{
		{"stopping", func(state *record) { state.Stopping = true }, "shutting down"},
		{"starting", func(state *record) { state.Ready = false }, "still starting"},
		{"stopped", func(state *record) { state.PID, state.StartTime, state.Ready = 0, "", false }, "is stopped"},
		{"process gone", func(state *record) { state.StartTime = "stale" }, "is stopped"},
		{"exited after pending shutdown", func(state *record) { state.Stopping, state.StartTime = true, "stale" }, "is stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := state
			current.Ready = true
			tc.change(&current)
			err := vmNotRunning("vm", dir, current)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	running := state
	running.Ready = true
	if err := vmNotRunning("vm", dir, running); err != nil {
		t.Fatalf("running VM reported unavailable: %v", err)
	}
}

func TestDeleteWaitDoesNotHoldGlobalLock(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	if err := saveRecord(dir, record{}); err != nil {
		t.Fatal(err)
	}
	if err := deleteState(t.Context(), "vm", func(record, string) error { assertLifecycleLockAvailable(t); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestFetchDoesNotMigrateIdentityDuringLifecycleOperation(t *testing.T) {
	dir, _, state := stopFixture(t, true)
	state.InstanceID = ""
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lockVMOperation(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	session, err := openFetchSession(t.Context(), "vm")
	if session != nil {
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertErrorContains(t, err, "lifecycle operation")
	assertFileContents(t, filepath.Join(dir, "state.json"), before)
}

func TestProcessDisappearanceErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		gone bool
	}{
		{name: "missing directory", err: os.ErrNotExist, gone: true},
		{name: "exited during read", err: &os.PathError{Op: "read", Path: "/proc/123/stat", Err: syscall.ESRCH}, gone: true},
		{name: "zombie", err: errProcessExited, gone: true},
		{name: "permission denied", err: os.ErrPermission},
		{name: "invalid identity", err: errors.New("invalid process identity")},
		{name: "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := processDisappeared(tc.err); got != tc.gone {
				t.Fatalf("processDisappeared(%v) = %v, want %v", tc.err, got, tc.gone)
			}
		})
	}
}

func TestDeleteRetainsStorageWhenProcessIdentityChanged(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	state.StartTime = "stale-start-time"
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := Delete(t.Context(), "vm", nil); err == nil {
		t.Fatal("delete discarded storage while the VM process was still alive")
	}
	child.assertAlive(t)
	assertStopStorage(t, dir, state)
}

func TestStopRequiresRecordedRootDisk(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprint(force), func(t *testing.T) {
			for _, kind := range []string{"no handle", "replaced inode", "missing disk"} {
				t.Run(kind, func(t *testing.T) {
					dir, child, _ := stopProcessFixture(t, true, kind != "no handle")
					disk := filepath.Join(dir, "rootfs.ext4")
					if kind != "no handle" {
						if err := os.Rename(disk, filepath.Join(dir, "previous.ext4")); err != nil {
							t.Fatal(err)
						}
						if kind == "replaced inode" {
							writeTestFile(t, disk, []byte("replacement"))
						}
					}
					before, err := os.ReadFile(filepath.Join(dir, "state.json"))
					if err != nil {
						t.Fatal(err)
					}
					err = stopVM(t.Context(), "vm", force, time.Second, func(context.Context, record, string) error {
						t.Error("unverified process received shutdown request")
						return child.cmd.Process.Signal(syscall.SIGTERM)
					})
					if err == nil {
						t.Error("stop authorized process without the expected open disk inode")
					}
					child.assertAlive(t)
					assertFileContents(t, filepath.Join(dir, "state.json"), before)
				})
			}
		})
	}
}

func TestDeleteAllowsSiblingStopDuringWait(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	target := makeVMDir(t, root, "deleting")
	sibling := makeVMDir(t, root, "sibling")
	for _, dir := range []string{target, sibling} {
		if err := saveRecord(dir, record{}); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "rootfs.ext4"), []byte("preserve"))
	}
	err := deleteState(t.Context(), "deleting", func(record, string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		defer cancel()
		return Stop(ctx, "sibling", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(sibling, "rootfs.ext4"), []byte("preserve"))
}

func runStopTestChild() {
	dir := os.Getenv("HABIKAT_STOP_TEST_DIR")
	mode := os.Getenv("HABIKAT_STOP_TEST_CHILD")
	var disk *os.File
	if mode != "no-disk" {
		var err error
		disk, err = os.Open(filepath.Join(dir, "rootfs.ext4"))
		if err != nil {
			panic(err)
		}
		defer disk.Close()
	}
	if mode == "legacy" || mode == "no-disk" || mode == "no-api" {
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	done := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var action struct {
			Action string `json:"action_type"`
		}
		if r.Method != http.MethodPut || r.URL.Path != "/actions" || json.Unmarshal(body, &action) != nil || action.Action != "SendCtrlAltDel" {
			w.WriteHeader(400)
			return
		}
		_ = os.WriteFile(filepath.Join(dir, "request.received"), body, 0600)
		if mode == "reject" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush()
		if mode == "exit" {
			done <- struct{}{}
		}
	})}
	go server.Serve(listener)
	fmt.Fprintln(os.Stdout, "ready")
	if mode == "exit" {
		<-done
	} else {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	runtime.KeepAlive(disk)
}

func TestStopMissingAndMalformedPreservesStorage(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"missing", ""}, {"invalid json", "{"}, {"null", "null"}, {"invalid pid", `{"pid":-1}`},
		{"invalid boot", `{"boot_id":"wrong"}`}, {"stopping without pid", `{"stopping":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			dir := makeVMDir(t, root, "vm")
			writeTestFile(t, filepath.Join(dir, "rootfs.ext4"), []byte("preserve"))
			if tc.data != "" {
				writeTestFile(t, filepath.Join(dir, "state.json"), []byte(tc.data))
			}
			if err := Stop(t.Context(), "vm", true); err == nil {
				t.Fatal("malformed state accepted")
			}
			assertFileContents(t, filepath.Join(dir, "rootfs.ext4"), []byte("preserve"))
			if err := Stop(t.Context(), "missing", false); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "VM missing not found") {
				t.Fatalf("missing VM: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing VM created")
			}
		})
	}
}

func TestStopUndeliveredRequestKeepsVMUsable(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	state.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	err := stopVM(t.Context(), "vm", false, time.Second, func(context.Context, record, string) error {
		return fmt.Errorf("%w: connection refused", errShutdownNotDelivered)
	})
	if err == nil || strings.Contains(err.Error(), "may still complete") || !strings.Contains(err.Error(), "VM is still running") {
		t.Fatalf("undelivered request: %v", err)
	}
	child.assertAlive(t)
	current, err := readRecord(dir)
	if err != nil || current.Stopping || current.PID != state.PID {
		t.Fatalf("VM left unusable: %+v %v", current, err)
	}
	if err := requireFetchSource("vm", dir, current); err != nil {
		t.Fatalf("fetch refused a running VM: %v", err)
	}
}

func TestStopUndeliveredRetryKeepsPendingShutdown(t *testing.T) {
	dir, child, state := stopFixture(t, true)
	err := stopVM(t.Context(), "vm", false, 30*time.Millisecond, func(context.Context, record, string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first stop: %v", err)
	}
	err = stopVM(t.Context(), "vm", false, time.Second, func(context.Context, record, string) error {
		return fmt.Errorf("%w: connection refused", errShutdownNotDelivered)
	})
	retryErr := err
	child.assertAlive(t)
	current, err := readRecord(dir)
	if err != nil || !current.Stopping || current.PID != state.PID {
		t.Fatalf("retry discarded pending shutdown: %+v %v", current, err)
	}
	current.Project = &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	if err := requireFetchSource("vm", dir, current); err == nil {
		t.Fatal("fetch accepted a VM with a pending shutdown")
	}
	assertErrorContains(t, retryErr, "may still complete")
}

func TestLifecycleCommandsReportCreateInProgress(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	operation, err := lockVMOperation(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	assertErrorContains(t, Delete(t.Context(), "vm", nil), "lifecycle operation")
	assertErrorContains(t, Stop(t.Context(), "vm", true), "lifecycle operation")
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("VM directory removed during create: %v", err)
	}
}

func TestDeleteReleasesNameBeforeRemovingStorage(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	if err := saveRecord(dir, record{}); err != nil {
		t.Fatal(err)
	}
	original := removeVMStorage
	t.Cleanup(func() { removeVMStorage = original })
	removeVMStorage = func(path string) error {
		operation, err := lockVMOperation(dir)
		if err == nil {
			operation.Close()
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("VM name still usable while its storage is removed: %v", err)
		}
		return original(path)
	}
	if err := deleteState(t.Context(), "vm", func(record, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".lock" {
			t.Errorf("leftover state entry %q", entry.Name())
		}
	}
}
