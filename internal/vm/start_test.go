package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const startTestChildEnv = "HABIKAT_START_TEST_CHILD"

var preservedStartFiles = []string{"rootfs.ext4", "seed.ext4", "identity", "known_hosts"}

type startEnv struct {
	root, dir, image string
	state            record
}

func shortPrivateDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

var testExecutableDigest = sync.OnceValues(func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return fileDigest(exe)
})

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func startImageFixture(t *testing.T) (string, launchArtifacts) {
	t.Helper()
	image := shortPrivateDir(t, "hk-img-")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(image, "firecracker")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(image, "vmlinux"), []byte("fixture kernel"))
	firecracker, err := testExecutableDigest()
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := fileDigest(filepath.Join(image, "vmlinux"))
	if err != nil {
		t.Fatal(err)
	}
	writeManifest(t, image, map[string]string{"firecracker": firecracker, "vmlinux": kernel, "shutdown": shutdownCapability})
	launch := testProfileLaunch(&launchArtifacts{
		Firecracker: artifactRef{Path: filepath.Join(image, "firecracker"), SHA256: firecracker},
		Kernel:      artifactRef{Path: filepath.Join(image, "vmlinux"), SHA256: kernel},
	})
	if launch.NixOS != nil {
		data, _ := json.Marshal(launch.NixOS.Boot)
		writeTestFile(t, launch.NixOS.Initrd.Path, []byte("initrd"))
		writeTestFile(t, launch.NixOS.System.Path, []byte("system"))
		writeTestFile(t, launch.NixOS.Metadata.Path, data)
		launch.NixOS.Initrd.SHA256, _ = fileDigest(launch.NixOS.Initrd.Path)
		launch.NixOS.System.SHA256, _ = fileDigest(launch.NixOS.System.Path)
		launch.NixOS.Metadata.SHA256, _ = fileDigest(launch.NixOS.Metadata.Path)
	}
	return image, *launch
}

func startFixture(t *testing.T) startEnv {
	t.Helper()
	root := shortPrivateDir(t, "hk-start-")
	t.Setenv("HK_STATE_DIR", root)
	t.Setenv("HK_IMAGE_DIR", filepath.Join(root, "unrelated-image-override"))
	t.Setenv(startTestChildEnv, "1")
	dir := makeVMDir(t, root, "vm")
	for _, file := range preservedStartFiles {
		writeTestFile(t, filepath.Join(dir, file), []byte("preserved "+file))
	}
	image, launch := startImageFixture(t)
	state := record{
		InstanceID:      strings.Repeat("2", 32),
		BootID:          "00000000-0000-0000-0000-000000000000",
		Launch:          &launch,
		SourceDirectory: "/source",
		Project:         &Project{Branch: "main", Commit: strings.Repeat("a", 40), Workspace: Workspace},
	}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stopProcess(record{}, dir); err != nil {
			t.Errorf("cleanup fixture processes: %v", err)
		}
	})
	return startEnv{root: root, dir: dir, image: image, state: state}
}

func (env startEnv) setChildMode(t *testing.T, mode string) {
	t.Helper()
	writeTestFile(t, filepath.Join(env.dir, "test-child-mode"), []byte(mode))
}

func (env startEnv) launches(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.dir, "launches"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(data), "\n", " "))
}

func (env startEnv) assertDisksAndKeys(t *testing.T) {
	t.Helper()
	for _, file := range preservedStartFiles {
		assertFileContents(t, filepath.Join(env.dir, file), []byte("preserved "+file))
	}
}

func (env startEnv) assertStorage(t *testing.T) {
	t.Helper()
	env.assertDisksAndKeys(t)
	current, err := readRecord(env.dir)
	if err != nil || current.InstanceID != env.state.InstanceID || current.Project == nil || *current.Project != *env.state.Project {
		t.Fatalf("persistent identity changed: %+v %v", current, err)
	}
}

func (env startEnv) assertNoProcess(t *testing.T) {
	t.Helper()
	processes, err := processesForVM(env.dir)
	if err != nil || len(processes) != 0 {
		t.Fatalf("VM processes after failed start: %+v %v", processes, err)
	}
}

func (env startEnv) assertStopped(t *testing.T) {
	t.Helper()
	env.assertNoProcess(t)
	current, err := readRecord(env.dir)
	if err != nil || current.PID != 0 || current.Ready || current.Stopping {
		t.Fatalf("failed start left runtime state: %+v %v", current, err)
	}
}

func readyNow(_ context.Context, dir string) error {
	_, err := os.Lstat(filepath.Join(dir, "vsock.sock"))
	return err
}

func neverReady(context.Context, string) error { return errors.New("SSH is not ready") }

func leaveStaleSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartBootsStoppedVMWithRecordedArtifacts(t *testing.T) {
	env := startFixture(t)
	leaveStaleSocket(t, filepath.Join(env.dir, "api.sock"))
	leaveStaleSocket(t, filepath.Join(env.dir, "vsock.sock"))
	t.Chdir(shortPrivateDir(t, "hk-cwd-"))
	result, err := startVM(t.Context(), "vm", readyNow, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyRunning {
		t.Fatalf("result = %+v", result)
	}
	current, err := readRecord(env.dir)
	if err != nil || !current.Ready || current.Stopping || !sameProcess(current, env.dir) {
		t.Fatalf("started VM not recorded as running: %+v %v", current, err)
	}
	boot, err := hostBootID()
	if err != nil || current.BootID != boot {
		t.Fatalf("boot identity = %q, want %q (%v)", current.BootID, boot, err)
	}
	if current.Launch == nil || !reflect.DeepEqual(current.Launch, env.state.Launch) {
		t.Fatalf("launch artifacts changed: %+v", current.Launch)
	}
	if launches := env.launches(t); len(launches) != 2 || launches[1] != env.state.Launch.Kernel.Path {
		t.Fatalf("launches = %q", launches)
	}
	if err := requireOpenRootDisk(current.PID, env.dir); err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"seed", filepath.Join("..", "unrelated-image-override")} {
		if _, err := os.Lstat(filepath.Join(env.dir, unwanted)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("start provisioned %s: %v", unwanted, err)
		}
	}
	env.assertStorage(t)
}

func TestStartIsNoOpForRunningVM(t *testing.T) {
	env := startFixture(t)
	if _, err := startVM(t.Context(), "vm", readyNow, time.Second); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(env.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := startVM(t.Context(), "vm", func(context.Context, string) error {
		t.Error("readiness rechecked for a running VM")
		return nil
	}, time.Second)
	if err != nil || !result.AlreadyRunning {
		t.Fatalf("second start: %+v %v", result, err)
	}
	if len(env.launches(t)) != 2 {
		t.Fatalf("second start launched another writer: %q", env.launches(t))
	}
	assertFileContents(t, filepath.Join(env.dir, "state.json"), before)
}

func TestStartRejectsUnverifiedArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, env startEnv)
		want   string
	}{
		{"kernel missing", func(t *testing.T, env startEnv) {
			if err := os.Remove(env.state.Launch.Kernel.Path); err != nil {
				t.Fatal(err)
			}
		}, "launch artifact"},
		{"kernel changed", func(t *testing.T, env startEnv) {
			writeTestFile(t, env.state.Launch.Kernel.Path, []byte("replaced kernel"))
		}, "does not match its recorded SHA-256"},
		{"firecracker changed", func(t *testing.T, env startEnv) {
			if err := os.Remove(env.state.Launch.Firecracker.Path); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, env.state.Launch.Firecracker.Path, []byte("#!/bin/sh\nexit 0\n"))
		}, "does not match its recorded SHA-256"},
		{"firecracker not regular", func(t *testing.T, env startEnv) {
			if err := os.Remove(env.state.Launch.Firecracker.Path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(env.state.Launch.Firecracker.Path, 0700); err != nil {
				t.Fatal(err)
			}
		}, "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startFixture(t)
			tc.change(t, env)
			before, err := os.ReadFile(filepath.Join(env.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = startVM(t.Context(), "vm", readyNow, time.Second)
			assertErrorContains(t, err, tc.want)
			assertErrorContains(t, err, "preserved")
			if len(env.launches(t)) != 0 {
				t.Fatal("unverified artifact launched")
			}
			assertFileContents(t, filepath.Join(env.dir, "state.json"), before)
			env.assertStorage(t)
		})
	}
}

func TestStartBootFailureRetainsStorage(t *testing.T) {
	env := startFixture(t)
	env.setChildMode(t, "fail")
	writeTestFile(t, filepath.Join(env.dir, "console.log"), []byte("earlier boot diagnostics\n"))
	_, err := startVM(t.Context(), "vm", neverReady, 5*time.Second)
	assertErrorContains(t, err, "Firecracker exited before SSH was ready")
	assertErrorContains(t, err, "fixture boot failure")
	if strings.Contains(err.Error(), "earlier boot diagnostics") {
		t.Fatalf("failure reported diagnostics from an earlier boot: %v", err)
	}
	if console, err := os.ReadFile(filepath.Join(env.dir, "console.log")); err != nil || !bytes.Contains(console, []byte("earlier boot diagnostics")) {
		t.Fatalf("boot diagnostics were not retained: %q %v", console, err)
	}
	env.assertStopped(t)
	env.assertStorage(t)
	env.setChildMode(t, "boot")
	if _, err := startVM(t.Context(), "vm", readyNow, time.Second); err != nil {
		t.Fatalf("healthy start after failure: %v", err)
	}
}

func TestStartReadinessTimeoutStopsOnlyItsProcess(t *testing.T) {
	env := startFixture(t)
	sibling := makeVMDir(t, env.root, "sibling")
	other := startTestProcess(t, firecrackerArgs(sibling))
	_, err := startVM(t.Context(), "vm", neverReady, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	assertErrorContains(t, err, "preserved")
	env.assertStopped(t)
	env.assertStorage(t)
	for _, name := range []string{"api.sock", "vsock.sock"} {
		if _, err := os.Lstat(filepath.Join(env.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed start left transient %s: %v", name, err)
		}
	}
	other.assertAlive(t)
}

func TestStartCancellationStopsLaunchedProcess(t *testing.T) {
	env := startFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	launched := 0
	_, err := startVM(ctx, "vm", func(ctx context.Context, dir string) error {
		if err := readyNow(ctx, dir); err != nil {
			return err
		}
		launched = len(env.launches(t))
		cancel()
		return ctx.Err()
	}, time.Minute)
	if !errors.Is(err, context.Canceled) || launched != 2 {
		t.Fatalf("cancelled start: %v (launched=%d)", err, launched)
	}
	env.assertStopped(t)
	env.assertStorage(t)
}

func TestStartAdoptsProcessWithUnpublishedPID(t *testing.T) {
	env := startFixture(t)
	if err := writeImageLaunchConfig(env.dir, *env.state.Launch); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(env.dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	pid, err := startDetachedFirecracker(t.Context(), env.state.Launch.Firecracker.Path, filepath.Join(env.dir, "config.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	awaitFixtureBoot(t, env)
	result, err := startVM(t.Context(), "vm", readyNow, time.Second)
	if err != nil || result.AlreadyRunning {
		t.Fatalf("adopt interrupted start: %+v %v", result, err)
	}
	current, err := readRecord(env.dir)
	if err != nil || current.PID != pid || !current.Ready {
		t.Fatalf("adopted record = %+v %v; want pid %d", current, err, pid)
	}
	if len(env.launches(t)) != 2 {
		t.Fatalf("interrupted start recovery launched a second writer: %q", env.launches(t))
	}
}

func awaitFixtureBoot(t *testing.T, env startEnv) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(filepath.Join(env.dir, "vsock.sock")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture Firecracker did not boot")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartRefusesSecondWriterOnDisk(t *testing.T) {
	env := startFixture(t)
	t.Setenv("HABIKAT_VM_TEST_DISK", filepath.Join(env.dir, "rootfs.ext4"))
	holder := startTestProcess(t, []string{"debugfs", "-w", filepath.Join(env.dir, "rootfs.ext4")})
	_, err := startVM(t.Context(), "vm", readyNow, time.Second)
	assertErrorContains(t, err, "second writer")
	if len(env.launches(t)) != 0 {
		t.Fatal("started while another process held the disk")
	}
	holder.assertAlive(t)
	env.assertStorage(t)
}

func TestStartRejectsConflictingSocketPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, env startEnv) string
	}{
		{"regular file", func(t *testing.T, env startEnv) string {
			path := filepath.Join(env.dir, "api.sock")
			writeTestFile(t, path, []byte("not a socket"))
			return path
		}},
		{"symlink to socket", func(t *testing.T, env startEnv) string {
			target := filepath.Join(env.root, "outside.sock")
			leaveStaleSocket(t, target)
			if err := os.Symlink(target, filepath.Join(env.dir, "vsock.sock")); err != nil {
				t.Fatal(err)
			}
			return target
		}},
		{"directory", func(t *testing.T, env startEnv) string {
			path := filepath.Join(env.dir, "vsock.sock")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startFixture(t)
			retained := tc.plant(t, env)
			_, err := startVM(t.Context(), "vm", readyNow, time.Second)
			assertErrorContains(t, err, "not a socket")
			if _, statErr := os.Lstat(retained); statErr != nil {
				t.Fatalf("start removed unrelated path %s: %v", retained, statErr)
			}
			if len(env.launches(t)) != 0 {
				t.Fatal("started with an ambiguous socket path")
			}
			env.assertStorage(t)
		})
	}
}

func TestStartConflictsWithLifecycleOperations(t *testing.T) {
	env := startFixture(t)
	result, err := startVM(t.Context(), "vm", func(context.Context, string) error {
		assertLifecycleLockAvailable(t)
		_, err := Start(t.Context(), "vm")
		assertErrorContains(t, err, "lifecycle operation")
		assertErrorContains(t, Delete(t.Context(), "vm", nil), "lifecycle operation")
		assertErrorContains(t, Stop(t.Context(), "vm", true), "lifecycle operation")
		_, err = Fetch(t.Context(), "vm", nil)
		assertErrorContains(t, err, "lifecycle operation")
		return nil
	}, time.Second)
	if err != nil || result.AlreadyRunning {
		t.Fatalf("%+v %v", result, err)
	}
	processes, err := processesForVM(env.dir)
	if err != nil || len(processes) != 1 {
		t.Fatalf("processes = %+v %v", processes, err)
	}
	env.assertStorage(t)
}

func TestStartRaceLaunchesOneWriter(t *testing.T) {
	env := startFixture(t)
	release := make(chan struct{})
	ready := func(ctx context.Context, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := startVM(t.Context(), "vm", ready, 5*time.Second)
			errs <- err
		}()
	}
	first := <-errs
	close(release)
	second := <-errs
	failures := 0
	for _, err := range []error{first, second} {
		if err != nil {
			assertErrorContains(t, err, "lifecycle operation")
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("concurrent starts: %v / %v", first, second)
	}
	processes, err := processesForVM(env.dir)
	if err != nil || len(processes) != 1 || len(env.launches(t)) != 2 {
		t.Fatalf("concurrent starts ran %+v (launches %q): %v", processes, env.launches(t), err)
	}
}

func TestStartRefusesShuttingDownVM(t *testing.T) {
	dir, child, state := stopFixture(t)
	state.Stopping = true
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	_, err := startVM(t.Context(), "vm", readyNow, time.Second)
	assertErrorContains(t, err, "shutting down")
	child.assertAlive(t)
	assertStopStorage(t, dir, state)
}

func TestStartRefusesMismatchedLiveProcess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*record)
	}{
		{"recycled start time", func(state *record) { state.StartTime = "1" }},
		{"other host boot", func(state *record) { state.BootID = "00000000-0000-0000-0000-000000000000" }},
		{"other pid", func(state *record) { state.PID = os.Getpid() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, child, state := stopFixture(t)
			tc.change(&state)
			if err := saveRecord(dir, state); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = startVM(t.Context(), "vm", readyNow, time.Second)
			assertErrorContains(t, err, "second writer")
			child.assertAlive(t)
			assertFileContents(t, filepath.Join(dir, "state.json"), before)
		})
	}
}

func TestStartIgnoresStaleRuntimeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*record)
	}{
		{"previous host boot", func(state *record) {
			state.PID, state.StartTime, state.Ready = os.Getpid(), "1", true
		}},
		{"exited process", func(state *record) {
			state.PID, state.StartTime, state.Ready, state.Stopping = os.Getpid(), "1", true, true
		}},
		{"interrupted start", func(state *record) {
			state.PID, state.StartTime = os.Getpid(), "1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startFixture(t)
			tc.change(&env.state)
			if err := saveRecord(env.dir, env.state); err != nil {
				t.Fatal(err)
			}
			if _, err := startVM(t.Context(), "vm", readyNow, time.Second); err != nil {
				t.Fatal(err)
			}
			current, err := readRecord(env.dir)
			if err != nil || current.PID == os.Getpid() || !sameProcess(current, env.dir) {
				t.Fatalf("stale identity survived: %+v %v", current, err)
			}
			env.assertStorage(t)
		})
	}
}

func TestStartRefusesRecordWithoutLaunchArtifacts(t *testing.T) {
	env := startFixture(t)
	writeTestFile(t, filepath.Join(env.dir, "state.json"), recordWithoutField(t, env.state, "launch"))
	if err := writeImageLaunchConfig(env.dir, *env.state.Launch); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HK_IMAGE_DIR", env.image)
	before, err := os.ReadFile(filepath.Join(env.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = startVM(t.Context(), "vm", readyNow, time.Second)
	assertErrorContains(t, err, "unsupported or corrupt VM state")
	if len(env.launches(t)) != 0 {
		t.Fatal("started without recorded launch artifacts")
	}
	assertFileContents(t, filepath.Join(env.dir, "state.json"), before)
	env.assertDisksAndKeys(t)
}

func TestStartMissingDoesNotCreateState(t *testing.T) {
	root := filepath.Join(privateTempDir(t), "missing")
	t.Setenv("HK_STATE_DIR", root)
	if _, err := Start(t.Context(), "vm"); err == nil {
		t.Fatal("missing state root accepted")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created missing state root: %v", err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := Start(t.Context(), "vm")
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "VM vm not found") {
		t.Fatalf("missing VM: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "vm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("start created a VM directory")
	}
}

func TestStoppedVMSuggestsStart(t *testing.T) {
	env := startFixture(t)
	err := vmNotRunning("vm", env.dir, env.state)
	assertErrorContains(t, err, "hk start vm")
	assertErrorContains(t, err, "preserved")
	assertErrorContains(t, requireFetchSource("vm", env.dir, env.state), "hk start vm")
}

func TestStartRecordRejectsInvalidLaunchArtifacts(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, tc := range []struct{ name, launch string }{
		{"relative firecracker", `{"firecracker":{"path":"firecracker","sha256":"` + digest + `"},"kernel":{"path":"/i/vmlinux","sha256":"` + digest + `"}}`},
		{"wrong firecracker name", `{"firecracker":{"path":"/bin/sh","sha256":"` + digest + `"},"kernel":{"path":"/i/vmlinux","sha256":"` + digest + `"}}`},
		{"unclean kernel", `{"firecracker":{"path":"/i/firecracker","sha256":"` + digest + `"},"kernel":{"path":"/i/../vmlinux","sha256":"` + digest + `"}}`},
		{"short digest", `{"firecracker":{"path":"/i/firecracker","sha256":"aa"},"kernel":{"path":"/i/vmlinux","sha256":"` + digest + `"}}`},
		{"uppercase digest", `{"firecracker":{"path":"/i/firecracker","sha256":"` + strings.ToUpper(digest) + `"},"kernel":{"path":"/i/vmlinux","sha256":"` + digest + `"}}`},
		{"missing kernel", `{"firecracker":{"path":"/i/firecracker","sha256":"` + digest + `"}}`},
		{"nul path", `{"firecracker":{"path":"/i/firecracker\u0000","sha256":"` + digest + `"},"kernel":{"path":"/i/vmlinux","sha256":"` + digest + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeRecord(currentRecordJSON(t, `{"launch":`+tc.launch+`}`))
			assertErrorContains(t, err, "launch artifacts")
		})
	}
	valid := `{"firecracker":{"path":"/i/firecracker","sha256":"` + digest + `"},"kernel":{"path":"/i/vmlinux","sha256":"` + digest + `"}}`
	var launch launchArtifacts
	if err := json.Unmarshal([]byte(valid), &launch); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(testProfileLaunch(&launch))
	state, err := decodeRecord(currentRecordJSON(t, `{"launch":`+string(encoded)+`}`))
	if err != nil || state.Launch == nil || state.Launch.Kernel.Path != "/i/vmlinux" {
		t.Fatalf("valid launch record: %+v %v", state.Launch, err)
	}
}

func FuzzStartPreservesData(f *testing.F) {
	for _, data := range []string{
		currentRecordSeed(f, "/source"), `{}`, `null`, `{"pid":-1}`, `{"pid":0,"ready":true}`, `{"stopping":true}`, `{"creating":true}`, `{"creating":true,"ready":true}`,
		`{"launch":{"firecracker":{"path":"/bin/sh","sha256":"00"}}}`,
		`{"pid":1,"start_time":"1","ready":true,"boot_id":"00000000-0000-0000-0000-000000000000"}`,
		`{"instance_id":"22222222222222222222222222222222","launch":null}`,
	} {
		f.Add(data, false)
		f.Add(data, true)
	}
	f.Fuzz(func(t *testing.T, data string, live bool) {
		if len(data) > 4096 {
			t.Skip()
		}
		env := startFixture(t)
		env.setChildMode(t, "fail")
		var holder *testProcess
		if live {
			t.Setenv("HABIKAT_VM_TEST_DISK", filepath.Join(env.dir, "rootfs.ext4"))
			holder = startTestProcess(t, firecrackerArgs(env.dir))
		}
		writeTestFile(t, filepath.Join(env.dir, "state.json"), []byte(data))
		_, _ = startVM(t.Context(), "vm", neverReady, 200*time.Millisecond)
		for _, file := range preservedStartFiles {
			assertFileContents(t, filepath.Join(env.dir, file), []byte("preserved "+file))
		}
		if live {
			holder.assertAlive(t)
			if launches := env.launches(t); len(launches) != 0 {
				t.Fatalf("state %q authorized a second writer: %q", data, launches)
			}
			return
		}
		env.assertNoProcess(t)
	})
}

func runStartTestChild(args []string) {
	config, api := "", ""
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "--config-file":
			config = args[i+1]
		case "--api-sock":
			api = args[i+1]
		}
	}
	dir := filepath.Dir(config)
	settings, err := readStartChildConfig(config)
	if err != nil {
		fmt.Println("fixture config:", err)
		os.Exit(2)
	}
	appendLaunch(dir, settings.BootSource.Kernel)
	if mode, _ := os.ReadFile(filepath.Join(dir, "test-child-mode")); string(mode) == "fail" {
		fmt.Println("fixture boot failure")
		os.Exit(1)
	}
	diskPath := ""
	for _, drive := range settings.Drives {
		if !drive.ReadOnly {
			diskPath = drive.Path
			break
		}
	}
	disk, err := os.OpenFile(diskPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Println("fixture disk:", err)
		os.Exit(2)
	}
	defer disk.Close()
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGTERM)
	listeners := make([]net.Listener, 0, 2)
	for _, path := range []string{api, settings.Vsock.Path} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			fmt.Println("fixture socket:", err)
			os.Exit(2)
		}
		defer listener.Close()
		listeners = append(listeners, listener)
	}
	go serveFixtureShutdown(listeners[0], shutdown)
	fmt.Println("fixture booted")
	<-shutdown
	fmt.Println("HABIKAT_SHUTDOWN_CLEAN")
}

func serveFixtureShutdown(listener net.Listener, shutdown chan<- os.Signal) {
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.Path != "/actions" || !bytes.Contains(body, []byte("SendCtrlAltDel")) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush()
		shutdown <- syscall.SIGTERM
	}))
}

type startChildConfig struct {
	BootSource struct {
		Kernel string `json:"kernel_image_path"`
	} `json:"boot-source"`
	Drives []struct {
		Path     string `json:"path_on_host"`
		ReadOnly bool   `json:"is_read_only"`
	} `json:"drives"`
	Vsock struct {
		Path string `json:"uds_path"`
	} `json:"vsock"`
}

func readStartChildConfig(path string) (startChildConfig, error) {
	var config startChildConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	if len(config.Drives) == 0 || config.Vsock.Path == "" {
		return config, errors.New("incomplete fixture config")
	}
	return config, nil
}

func appendLaunch(dir, kernel string) {
	file, err := os.OpenFile(filepath.Join(dir, "launches"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, err = io.WriteString(file, fmt.Sprintf("%d %s\n", os.Getpid(), kernel))
	if errors.Join(err, file.Close()) != nil {
		os.Exit(2)
	}
}

func TestStartRefusesIncompleteCreate(t *testing.T) {
	env := startFixture(t)
	incomplete := env.state
	incomplete.Creating = true
	if err := saveRecord(env.dir, incomplete); err != nil {
		t.Fatal(err)
	}
	_, err := startVM(t.Context(), "vm", readyNow, time.Second)
	assertErrorContains(t, err, "not fully created")
	if len(env.launches(t)) != 0 {
		t.Fatal("booted a VM whose create never finished")
	}
	env.assertNoProcess(t)
	env.assertStorage(t)
}

func TestStartRefusesToAdoptIncompleteCreateProcess(t *testing.T) {
	env := startFixture(t)
	incomplete := env.state
	incomplete.Creating = true
	if err := saveRecord(env.dir, incomplete); err != nil {
		t.Fatal(err)
	}
	if err := writeImageLaunchConfig(env.dir, *env.state.Launch); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(env.dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := startDetachedFirecracker(t.Context(), env.state.Launch.Firecracker.Path, filepath.Join(env.dir, "config.json"), log); err != nil {
		t.Fatal(err)
	}
	awaitFixtureBoot(t, env)
	_, err = startVM(t.Context(), "vm", readyNow, time.Second)
	assertErrorContains(t, err, "not fully created")
	current, err := readRecord(env.dir)
	if err != nil || !current.Creating || current.Ready || current.PID != 0 {
		t.Fatalf("incomplete create was adopted: %+v %v", current, err)
	}
}

func TestRecordRejectsReadyWhileCreating(t *testing.T) {
	invalid := currentRecordJSON(t, `{"creating":true,"pid":1,"start_time":"1","ready":true}`)
	_, err := decodeRecord(invalid)
	assertErrorContains(t, err, "invalid VM creation state")
	data, err := encodeRecord(currentRecordWith(t, func(s *record) { s.Creating = true }))
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeRecord(data)
	if err != nil || !state.Creating {
		t.Fatalf("creating marker lost: %+v %v", state, err)
	}
}

func TestInitialCreateRecord(t *testing.T) {
	images, manifest := imageFixture(t)
	image, err := verifyImages(images)
	if err != nil {
		t.Fatal(err)
	}
	state, err := initialCreateRecord(image.launch, "/source")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Creating || state.Ready || state.PID != 0 || state.SourceDirectory != "/source" {
		t.Fatalf("initial create record: %+v", state)
	}
	if !validInstanceID.MatchString(state.InstanceID) || !validBootID.MatchString(state.BootID) {
		t.Fatalf("create identities: %+v", state)
	}
	if state.Launch == nil || state.Launch.Kernel.SHA256 != manifest["vmlinux"] || state.Launch.Firecracker.SHA256 != manifest["firecracker"] {
		t.Fatalf("launch artifacts not recorded: %+v", state.Launch)
	}
}

func TestStartRejectsNonRegularConsoleLog(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"fifo", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "outside")
			writeTestFile(t, target, []byte("outside"))
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { assertFileContents(t, target, []byte("outside")) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startFixture(t)
			tc.setup(t, filepath.Join(env.dir, "console.log"))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err := startVM(ctx, "vm", readyNow, time.Second)
			if err == nil || ctx.Err() != nil {
				t.Fatalf("non-regular console log: err=%v ctx=%v", err, ctx.Err())
			}
			env.assertNoProcess(t)
			env.assertStorage(t)
		})
	}
}

func TestConsoleTailDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret")
	writeTestFile(t, target, []byte("outside secret"))
	if err := os.Symlink(target, filepath.Join(dir, "console.log")); err != nil {
		t.Fatal(err)
	}
	if tail := (consoleSince{dir: dir}).tail(); strings.Contains(tail, "outside secret") {
		t.Fatalf("console tail followed a symlink: %q", tail)
	}
}

func TestConsoleTailDoesNotBlockOnFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "console.log"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- (consoleSince{dir: dir}).tail() }()
	select {
	case tail := <-done:
		if !strings.Contains(tail, "unavailable") {
			t.Fatalf("FIFO console tail = %q", tail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("console tail blocked on a FIFO")
	}
}

func FuzzLaunchArtifactRecord(f *testing.F) {
	for _, seed := range [][2]string{
		{"/images/firecracker", strings.Repeat("a", 64)},
		{"images/firecracker", strings.Repeat("a", 64)},
		{"/images/../firecracker", strings.Repeat("a", 64)},
		{"/images/firecracker\x00", strings.Repeat("a", 64)},
		{"/", ""},
		{"/firecracker", strings.Repeat("G", 64)},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, path, digest string) {
		data, err := encodeRecord(currentRecordWith(t, func(s *record) {
			s.Launch.Firecracker = artifactRef{Path: path, SHA256: digest}
			s.Launch.Kernel = artifactRef{Path: path, SHA256: digest}
		}))
		if err != nil {
			return
		}
		state, err := decodeRecord(data)
		if err != nil {
			return
		}
		launch := state.Launch
		if !filepath.IsAbs(launch.Firecracker.Path) || filepath.Clean(launch.Firecracker.Path) != launch.Firecracker.Path || filepath.Base(launch.Firecracker.Path) != "firecracker" {
			t.Fatalf("accepted unconfined launch binary %q", launch.Firecracker.Path)
		}
		if !validDigest.MatchString(launch.Kernel.SHA256) {
			t.Fatalf("accepted malformed digest %q", launch.Kernel.SHA256)
		}
	})
}

func TestStartRefusesToAdoptProcessNotHoldingRootDisk(t *testing.T) {
	env := startFixture(t)
	if err := writeImageLaunchConfig(env.dir, *env.state.Launch); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(shortPrivateDir(t, "hk-decoy-"), "decoy.ext4")
	writeTestFile(t, decoy, []byte("decoy"))
	configPath := filepath.Join(env.dir, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, configPath, bytes.Replace(data, []byte(filepath.Join(env.dir, "rootfs.ext4")), []byte(decoy), 1))
	log, err := os.OpenFile(filepath.Join(env.dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	pid, err := startDetachedFirecracker(t.Context(), env.state.Launch.Firecracker.Path, configPath, log)
	if err != nil {
		t.Fatal(err)
	}
	awaitFixtureBoot(t, env)
	_, err = startVM(t.Context(), "vm", readyNow, time.Second)
	assertErrorContains(t, err, "unrecorded process")
	current, err := readRecord(env.dir)
	if err != nil || current.PID != 0 || current.Ready {
		t.Fatalf("adopted a process that does not hold the root disk (pid %d): %+v %v", pid, current, err)
	}
}

func TestStartRejectsNonRegularDisks(t *testing.T) {
	for _, disk := range []string{"rootfs.ext4", "seed.ext4"} {
		for _, kind := range []string{"symlink", "directory"} {
			t.Run(disk+"/"+kind, func(t *testing.T) {
				env := startFixture(t)
				path := filepath.Join(env.dir, disk)
				target := filepath.Join(shortPrivateDir(t, "hk-disk-"), "target.ext4")
				writeTestFile(t, target, []byte("outside disk"))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				_, err := startVM(t.Context(), "vm", readyNow, time.Second)
				assertErrorContains(t, err, "must be a regular file")
				if len(env.launches(t)) != 0 {
					t.Fatal("booted a non-regular disk")
				}
				assertFileContents(t, target, []byte("outside disk"))
			})
		}
	}
}

func TestStartDetectsRunningVMThroughAliasedStateRoot(t *testing.T) {
	env := startFixture(t)
	if _, err := startVM(t.Context(), "vm", readyNow, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(shortPrivateDir(t, "hk-alias-"), "p")
	if err := os.Symlink(filepath.Dir(env.root), parent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HK_STATE_DIR", filepath.Join(parent, filepath.Base(env.root)))
	t.Cleanup(func() {
		if err := stopProcess(record{}, filepath.Join(parent, filepath.Base(env.root), "vm")); err != nil {
			t.Errorf("cleanup aliased processes: %v", err)
		}
	})
	_, err := startVM(t.Context(), "vm", readyNow, 5*time.Second)
	if err == nil {
		processes, _ := processesForVM(env.dir)
		t.Fatalf("start through an aliased state root succeeded; writers via real path: %d", len(processes))
	}
	processes, err := processesForVM(env.dir)
	if err != nil || len(processes) != 1 {
		t.Fatalf("aliased start changed writers: %+v %v", processes, err)
	}
	if launches := env.launches(t); len(launches) != 2 {
		t.Fatalf("aliased start launched another writer: %q", launches)
	}
	if _, err := os.Lstat(filepath.Join(env.dir, "api.sock")); err != nil {
		t.Fatalf("aliased start removed the live API socket: %v", err)
	}
}
