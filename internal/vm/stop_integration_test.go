//go:build integration

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStopRecoversUnpublishedProcess(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	disk := filepath.Join(dir, "rootfs.ext4")
	writeTestFile(t, disk, []byte("durable fixture"))
	t.Setenv("HABIKAT_VM_TEST_DISK", disk)
	child := startTestProcess(t, firecrackerArgs(dir))
	state := child.state
	state.PID, state.StartTime, state.Ready = 0, "", false
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	assertFileContents(t, disk, []byte("durable fixture"))
	stopped, err := readRecord(dir)
	if err != nil || stopped.PID != 0 || stopped.InstanceID != state.InstanceID {
		t.Fatalf("invalid recovery state: %+v %v", stopped, err)
	}
}

func TestStopRejectsUnverifiedUnpublishedProcess(t *testing.T) {
	dir, child, state := stopProcessFixture(t, false)
	state.PID, state.StartTime, state.Ready = 0, "", false
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", true); err == nil {
		t.Fatal("signaled process without verifying disk ownership")
	}
	child.assertAlive(t)
	assertStopStorage(t, dir, state)
}

func TestStopRejectsMultipleWriters(t *testing.T) {
	dir, child, state := stopFixture(t)
	other := startTestProcess(t, firecrackerArgs(dir))
	if err := Stop(t.Context(), "vm", true); err == nil {
		t.Fatal("ambiguous process set accepted")
	}
	child.assertAlive(t)
	other.assertAlive(t)
	assertStopStorage(t, dir, state)
}

func TestStopRetainsConflictingSocketFile(t *testing.T) {
	dir, child, state := stopFixture(t)
	socket := filepath.Join(dir, "api.sock")
	writeTestFile(t, socket, []byte("unrelated"))
	if err := Stop(t.Context(), "vm", false); err == nil {
		t.Fatal("regular API file accepted")
	}
	child.assertAlive(t)
	assertFileContents(t, socket, []byte("unrelated"))
	assertStopStorage(t, dir, state)
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatal(err)
	}
}

func TestProcReadAfterExit(t *testing.T) {
	root := privateTempDir(t)
	dir := makeVMDir(t, root, "vm")
	child := startTestProcess(t, firecrackerArgs(dir))
	cmdline, err := os.Open(fmt.Sprintf("/proc/%d/cmdline", child.state.PID))
	if err != nil {
		t.Fatal(err)
	}
	defer cmdline.Close()
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.await(t)
	_, err = io.ReadAll(cmdline)
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("post-exit proc read = %v, expected ESRCH", err)
	}
	t.Log("A proc cmdline opened before child exit returns ESRCH after exit; os.ErrNotExist does not match this error")
}

func TestStopToleratesUnrelatedProcessExit(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	if err := saveRecord(dir, currentRecord(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	for range 3 {
		churn := exec.CommandContext(ctx, "/usr/bin/sh", "-c", "while :; do /usr/bin/true; done")
		if err := churn.Start(); err != nil {
			t.Fatal(err)
		}
		workers.Go(func() { _ = churn.Wait() })
	}
	scans := 0
	for ctx.Err() == nil {
		err := Stop(ctx, "vm", false)
		scans++
		if errors.Is(err, syscall.ESRCH) {
			t.Fatalf("unrelated process exit aborted Stop after %d scans: %v", scans, err)
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	t.Logf("completed %d Stop scans under unrelated process churn", scans)
}
