package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("HABIKAT_VM_TEST_CHILD") == "1" {
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestImageDir(t *testing.T) {
	root := privateTempDir(t)
	t.Chdir(root)
	for _, tc := range []struct{ name, image, cache, home, want string }{
		{name: "explicit", image: "images/../chosen", cache: "ignored", want: filepath.Join(root, "chosen")},
		{name: "xdg", cache: root, home: "ignored", want: filepath.Join(root, "habikat", "image")},
		{name: "home", home: root, want: filepath.Join(root, ".cache", "habikat", "image")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HK_IMAGE_DIR", tc.image)
			t.Setenv("XDG_CACHE_HOME", tc.cache)
			t.Setenv("HOME", tc.home)
			got, err := imageDir()
			if err != nil || got != tc.want {
				t.Fatalf("imageDir() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestImageVerification(t *testing.T) {
	dir, _ := imageFixture(t)
	if err := verifyImages(dir); err != nil {
		t.Fatal(err)
	}
}

func TestImageVerificationRejectsArtifactChanges(t *testing.T) {
	for _, name := range []string{"firecracker", "vmlinux", "rootfs.ext4"} {
		t.Run(name, func(t *testing.T) {
			dir, manifest := imageFixture(t)
			writeTestFile(t, filepath.Join(dir, name), []byte("corrupted"))
			assertErrorContains(t, verifyImages(dir), "SHA-256 mismatch for "+name)
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if err := verifyImages(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing artifact error = %v", err)
			}
			delete(manifest, name)
			writeManifest(t, dir, manifest)
			assertErrorContains(t, verifyImages(dir), "missing SHA-256 for "+name)
		})
	}
}

func TestImageVerificationRejectsManifest(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{name: "syntax", data: "{", want: "read image manifest:"},
		{name: "array", data: "[]", want: "read image manifest:"},
		{name: "wrong type", data: `{"firecracker":1}`, want: "read image manifest:"},
		{name: "null", data: "null", want: "missing SHA-256 for firecracker"},
		{name: "short digest", data: `{"firecracker":"00"}`, want: "missing SHA-256 for firecracker"},
		{name: "nonhex digest", data: `{"firecracker":"` + strings.Repeat("z", 64) + `"}`, want: "SHA-256 mismatch for firecracker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := imageFixture(t)
			writeTestFile(t, filepath.Join(dir, "manifest.json"), []byte(tc.data))
			assertErrorContains(t, verifyImages(dir), tc.want)
		})
	}
}

func FuzzImageIntegrity(f *testing.F) {
	f.Add(uint8(0), []byte("firecracker"))
	f.Add(uint8(1), []byte{})
	f.Add(uint8(2), []byte{0, 255, 10})
	f.Fuzz(func(t *testing.T, selected uint8, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		dir, manifest := imageFixture(t)
		name := []string{"firecracker", "vmlinux", "rootfs.ext4"}[int(selected)%3]
		writeTestFile(t, filepath.Join(dir, name), data)
		hash := sha256.Sum256(data)
		manifest[name] = hex.EncodeToString(hash[:])
		writeManifest(t, dir, manifest)
		if err := verifyImages(dir); err != nil {
			t.Fatalf("valid artifact rejected: %v", err)
		}
		changed := append([]byte(nil), data...)
		if len(changed) == 0 {
			changed = []byte{1}
		} else {
			changed[int(selected)%len(changed)] ^= 1
		}
		writeTestFile(t, filepath.Join(dir, name), changed)
		assertErrorContains(t, verifyImages(dir), "SHA-256 mismatch for "+name)
	})
}

func imageFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := privateTempDir(t)
	manifest := map[string]string{}
	for _, name := range []string{"firecracker", "vmlinux", "rootfs.ext4"} {
		data := []byte("artifact " + name)
		writeTestFile(t, filepath.Join(dir, name), data)
		hash := sha256.Sum256(data)
		manifest[name] = hex.EncodeToString(hash[:])
	}
	writeManifest(t, dir, manifest)
	return dir, manifest
}

func writeManifest(t *testing.T, dir string, manifest map[string]string) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "manifest.json"), data)
}

func TestProcessIdentity(t *testing.T) {
	dir := privateTempDir(t)
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "exact", args: firecrackerArgs(dir), want: true},
		{name: "different config", args: firecrackerArgs(privateTempDir(t))},
		{name: "different executable", args: []string{"other", "--no-api", "--config-file", filepath.Join(dir, "config.json")}},
		{name: "extra argument", args: append(firecrackerArgs(dir), "--extra")},
		{name: "missing argument", args: []string{"firecracker", "--no-api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := startTestProcess(t, tc.args)
			if got := ownsConfig(child.state.PID, dir); got != tc.want {
				t.Fatalf("ownsConfig() = %v, want %v", got, tc.want)
			}
			if got := sameProcess(child.state, dir); got != tc.want {
				t.Fatalf("sameProcess() = %v, want %v", got, tc.want)
			}
			stale := child.state
			stale.StartTime = "different-start-time"
			if sameProcess(stale, dir) {
				t.Fatal("accepted a stale process start time")
			}
		})
	}
}

func TestStopProcess(t *testing.T) {
	for _, kind := range []string{"owned", "stale start", "different config", "different executable"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateTempDir(t)
			args := firecrackerArgs(dir)
			if kind == "different config" {
				args = firecrackerArgs(privateTempDir(t))
			}
			if kind == "different executable" {
				args[0] = "unrelated"
			}
			child := startTestProcess(t, args)
			state := child.state
			if kind == "stale start" {
				state.StartTime = "different-start-time"
			}
			if err := stopProcess(state, dir); err != nil {
				t.Fatal(err)
			}
			if kind == "owned" {
				child.assertKilled(t)
				if sameProcess(state, dir) {
					t.Fatal("exited process still matches")
				}
				return
			}
			child.assertAlive(t)
		})
	}
}

func TestStopUnrecordedProcesses(t *testing.T) {
	dir := privateTempDir(t)
	owned := startTestProcess(t, firecrackerArgs(dir))
	other := startTestProcess(t, firecrackerArgs(privateTempDir(t)))
	entriesDir := privateTempDir(t)
	for _, name := range []string{strconv.Itoa(owned.state.PID), strconv.Itoa(other.state.PID), "self"} {
		writeTestFile(t, filepath.Join(entriesDir, name), nil)
	}
	entries, err := os.ReadDir(entriesDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := stopUnrecordedProcesses(dir, entries); err != nil {
		t.Fatal(err)
	}
	owned.assertKilled(t)
	other.assertAlive(t)
}

func TestDeleteStopsOwnedProcess(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "owned")
	child := startTestProcess(t, firecrackerArgs(dir))
	if err := saveRecord(dir, child.state); err != nil {
		t.Fatal(err)
	}
	if err := Delete(t.Context(), "owned", nil); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted VM directory remains: %v", err)
	}
}

func TestDeleteSkipsConfirmationWhenClean(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "clean")
	child := startTestProcess(t, firecrackerArgs(dir))
	state := child.state
	state.Ready = true
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	bin := privateTempDir(t)
	writeTestFile(t, filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf 'clean\\n'\n"))
	if err := os.Chmod(filepath.Join(bin, "ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := Delete(t.Context(), "clean", func() error {
		return errors.New("clean VM must not ask for confirmation")
	}); err != nil {
		t.Fatal(err)
	}
	child.assertKilled(t)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean VM was not deleted: %v", err)
	}
}

func TestDeleteStatePreservesUnreadableVM(t *testing.T) {
	for _, kind := range []string{"missing record", "corrupt record", "null record", "directory symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			dir := filepath.Join(root, "vm")
			storage := dir
			if kind == "directory symlink" {
				storage = privateTempDir(t)
				if err := os.Symlink(storage, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				makeVMDir(t, root, "vm")
			}
			writeTestFile(t, filepath.Join(storage, "disk"), []byte("keep"))
			if kind == "corrupt record" || kind == "null record" {
				data := "{"
				if kind == "null record" {
					data = "null"
				}
				writeTestFile(t, filepath.Join(storage, "state.json"), []byte(data))
			}
			if err := deleteState(t.Context(), "vm", unexpectedStop(t)); err == nil {
				t.Fatal("deletion accepted unreadable VM state")
			}
			assertFileContents(t, filepath.Join(storage, "disk"), []byte("keep"))
			if _, err := os.Lstat(dir); err != nil {
				t.Fatalf("VM path was removed: %v", err)
			}
		})
	}
}

func TestDeleteStateOrdersCleanup(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "vm")
	sibling := makeVMDir(t, root, "other")
	writeTestFile(t, filepath.Join(sibling, "disk"), []byte("other VM"))
	state := record{PID: 42, StartTime: "123", Ready: true}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "disk"), []byte("keep until stopped"))
	stopErr := errors.New("cannot stop owned process")
	for _, failure := range []error{stopErr, nil} {
		calls := 0
		stop := func(got record, path string) error {
			calls++
			if got != state || path != dir {
				t.Fatalf("stop received %+v, %q", got, path)
			}
			assertFileContents(t, filepath.Join(dir, "disk"), []byte("keep until stopped"))
			return failure
		}
		if err := deleteState(t.Context(), "vm", stop); !errors.Is(err, failure) {
			t.Fatalf("delete error = %v, want %v", err, failure)
		}
		if calls != 1 {
			t.Fatalf("stop called %d times", calls)
		}
		if failure != nil {
			assertFileContents(t, filepath.Join(dir, "disk"), []byte("keep until stopped"))
		}
	}
	assertFileContents(t, filepath.Join(sibling, "disk"), []byte("other VM"))
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VM directory remains: %v", err)
	}
	if err := deleteState(t.Context(), "vm", unexpectedStop(t)); err != nil {
		t.Fatalf("repeated delete: %v", err)
	}
}

func unexpectedStop(t *testing.T) func(record, string) error {
	t.Helper()
	return func(state record, dir string) error {
		t.Errorf("unexpected stop: %+v, %q", state, dir)
		return errors.New("unexpected stop")
	}
}

func makeVMDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func firecrackerArgs(dir string) []string {
	return []string{"firecracker", "--no-api", "--config-file", filepath.Join(dir, "config.json")}
}

type testProcess struct {
	cmd   *exec.Cmd
	state record
	done  chan struct{}
	err   error
}

func startTestProcess(t *testing.T, args []string) *testProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Args = args
	cmd.Env = append(os.Environ(), "HABIKAT_VM_TEST_CHILD=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &testProcess{cmd: cmd, done: make(chan struct{})}
	t.Cleanup(func() {
		_ = input.Close()
		_ = cmd.Process.Kill()
		child.await(t)
	})
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	if err := output.(*os.File).SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("child readiness = %q, %v", ready, err)
	}
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	child.state = record{PID: cmd.Process.Pid, StartTime: start, Ready: true}
	return child
}

func (p *testProcess) await(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned test process was not reaped")
	}
}

func (p *testProcess) assertKilled(t *testing.T) {
	t.Helper()
	p.await(t)
	var exit *exec.ExitError
	if !errors.As(p.err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("owned child exit = %v; want SIGKILL", p.err)
	}
}

func (p *testProcess) assertAlive(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("unrelated owned child exited: %v", p.err)
	default:
	}
	start, err := processStart(p.state.PID)
	if err != nil || start != p.state.StartTime {
		t.Fatalf("unrelated owned child was signaled: start = %q, %v", start, err)
	}
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestDeleteConfirmationInstanceAndLock(t *testing.T) {
	for _, kind := range []string{"reject", "cancel", "replace", "identity", "approve", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			dir := filepath.Join(root, "vm")
			if kind != "missing" {
				makeVMDir(t, root, "vm")
				if err := saveRecord(dir, record{}); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(dir, "disk"), []byte("valuable"))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, stops := 0, 0
			err := deleteConfirmed(ctx, "vm", func() error {
				calls++
				assertLifecycleLockAvailable(t)
				switch kind {
				case "reject":
					return errors.New("no")
				case "cancel":
					cancel()
				case "replace":
					if err := os.Rename(dir, dir+"-original"); err != nil {
						t.Fatal(err)
					}
					makeVMDir(t, root, "vm")
					if err := saveRecord(dir, record{}); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, filepath.Join(dir, "disk"), []byte("valuable"))
				case "identity":
					if err := saveRecord(dir, record{PID: 42, StartTime: "replacement"}); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}, func(context.Context, string, record) (bool, error) { return true, nil }, func(record, string) error { stops++; return nil })
			if kind == "approve" {
				if err != nil || stops != 1 || calls != 1 {
					t.Fatalf("approved deletion: %v %d %d", err, stops, calls)
				}
			} else if kind == "missing" {
				if err != nil || stops != 0 || calls != 0 {
					t.Fatalf("missing: %v %d %d", err, stops, calls)
				}
			} else {
				if err == nil || stops != 0 {
					t.Fatalf("unsafe deletion: %v stops=%d", err, stops)
				}
				assertFileContents(t, filepath.Join(dir, "disk"), []byte("valuable"))
			}
		})
	}
}

func assertLifecycleLockAvailable(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, lock, err := lockState(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
