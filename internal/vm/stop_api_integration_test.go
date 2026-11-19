//go:build integration

package vm

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startStopFixture(t *testing.T, mode string) (string, *testProcess) {
	t.Helper()
	root, err := os.MkdirTemp("", "hk-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	writeTestFile(t, filepath.Join(dir, "rootfs.ext4"), []byte("persistent disk"))
	writeTestFile(t, filepath.Join(dir, "identity"), []byte("private key"))
	writeTestFile(t, filepath.Join(dir, "known_hosts"), []byte("host key"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Args = []string{"firecracker", "--api-sock", filepath.Join(dir, "api.sock"), "--config-file", filepath.Join(dir, "config.json")}
	cmd.Env = append(os.Environ(), "HABIKAT_STOP_TEST_CHILD="+mode, "HABIKAT_STOP_TEST_DIR="+dir, "GORACE=atexit_sleep_ms=0")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &testProcess{cmd: cmd, done: make(chan struct{})}
	go func() { child.err = cmd.Wait(); close(child.done) }()
	t.Cleanup(func() { input.Close(); cmd.Process.Kill(); child.await(t) })
	output.(*os.File).SetReadDeadline(time.Now().Add(5 * time.Second))
	ready := make([]byte, 6)
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("fixture ready: %q %v", ready, err)
	}
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	child.state = currentRecordWith(t, func(s *record) { s.PID, s.StartTime, s.Ready = cmd.Process.Pid, start, true })
	if err := saveRecord(dir, child.state); err != nil {
		t.Fatal(err)
	}
	return dir, child
}

func TestGracefulStopRequiresProcessExit(t *testing.T) {
	for _, mode := range []string{"exit", "ack", "reject"} {
		t.Run(mode, func(t *testing.T) {
			dir, child := startStopFixture(t, mode)
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			err := stopVM(ctx, "vm", false, time.Second, requestShutdown)
			state, readErr := readRecord(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "exit" {
				if err != nil || state.PID != 0 || state.Ready || state.Stopping {
					t.Fatalf("stop: %+v %v", state, err)
				}
				child.await(t)
				if child.err != nil {
					t.Fatalf("graceful process killed: %v", child.err)
				}
			} else if mode == "reject" {
				if err == nil || strings.Contains(err.Error(), "may still complete") {
					t.Fatalf("rejected stop: %v", err)
				}
				child.assertAlive(t)
				if state.PID != child.state.PID || state.Stopping {
					t.Fatalf("rejected shutdown left VM unusable: %+v", state)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "may still complete") || !strings.Contains(err.Error(), "--force") {
					t.Fatalf("uncompleted stop: %v", err)
				}
				child.assertAlive(t)
				if state.PID != child.state.PID || !state.Stopping {
					t.Fatalf("lost live state: %+v", state)
				}
			}
			if state.InstanceID != child.state.InstanceID {
				t.Fatal("changed instance")
			}
			assertFileContents(t, filepath.Join(dir, "rootfs.ext4"), []byte("persistent disk"))
			assertFileContents(t, filepath.Join(dir, "identity"), []byte("private key"))
		})
	}
}

func TestStopCancellationAndLifecycleExclusion(t *testing.T) {
	dir, child := startStopFixture(t, "ack")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Stop(ctx, "vm", false) }()
	awaitStopRequest(t, dir)
	assertErrorContains(t, Delete(t.Context(), "vm", nil), "lifecycle operation")
	assertErrorContains(t, Stop(t.Context(), "vm", true), "lifecycle operation")
	globalCtx, globalCancel := context.WithTimeout(t.Context(), time.Second)
	defer globalCancel()
	_, lock, err := lockState(globalCtx, "sibling")
	if err != nil {
		t.Fatalf("stop held global lock: %v", err)
	}
	lock.Close()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "may still complete") {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation stuck")
	}
	child.assertAlive(t)
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatalf("abandoned operation blocks retry: %v", err)
	}
	child.assertKilled(t)
}

func awaitStopRequest(t *testing.T, dir string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, "request.received")); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("shutdown request not observed")
		case <-tick.C:
		}
	}
}

func TestStopRejectsConflictingAPISocket(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "other process"} {
		t.Run(kind, func(t *testing.T) {
			dir, child := startStopFixture(t, "no-api")
			path := filepath.Join(dir, "api.sock")
			switch kind {
			case "file":
				writeTestFile(t, path, []byte("unrelated"))
			case "symlink":
				if err := os.Symlink("identity", path); err != nil {
					t.Fatal(err)
				}
			case "other process":
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			}
			if err := Stop(t.Context(), "vm", false); err == nil {
				t.Fatal("conflicting socket accepted")
			}
			child.assertAlive(t)
			if state, err := readRecord(dir); err != nil || state.Stopping || state.PID != child.state.PID {
				t.Fatalf("undelivered shutdown left VM unusable: %+v %v", state, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("unrelated path removed")
			}
		})
	}
}
