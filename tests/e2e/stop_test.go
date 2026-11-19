//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStopRealVMFlushesAndPreservesStorage(t *testing.T) {
	h := newHarness(t)
	h.must("", "create", "--name", "stoppable")
	pid := vmPID(t, h.state("stoppable", "state.json"))
	identity := read(t, h.state("stoppable", "identity"))
	known := read(t, h.state("stoppable", "known_hosts"))
	var before map[string]any
	if err := json.Unmarshal(read(t, h.state("stoppable", "state.json")), &before); err != nil {
		t.Fatal(err)
	}
	h.must("printf 'stop-persistent-data\\n' > ~/stop-data\nchmod 640 ~/stop-data\nexit\n", "attach", "stoppable")
	out := h.must("", "stop", "stoppable")
	if !strings.Contains(out, "Stopped stoppable.") {
		t.Fatalf("%s", out)
	}
	assertGone(t, pid)
	if !bytes.Equal(identity, read(t, h.state("stoppable", "identity"))) || !bytes.Equal(known, read(t, h.state("stoppable", "known_hosts"))) {
		t.Fatal("stop changed credentials")
	}
	var after map[string]any
	if err := json.Unmarshal(read(t, h.state("stoppable", "state.json")), &after); err != nil {
		t.Fatal(err)
	}
	if after["instance_id"] != before["instance_id"] || after["pid"] != float64(0) || after["ready"] != false {
		t.Fatalf("invalid stopped state: %+v", after)
	}
	console := string(read(t, h.state("stoppable", "console.log")))
	if !strings.Contains(console, "HABIKAT_SHUTDOWN_CLEAN") {
		t.Fatalf("guest orderly shutdown not observed:\n%s", console)
	}
	check := exec.Command("e2fsck", "-fn", h.state("stoppable", "rootfs.ext4"))
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("stopped filesystem is not clean: %v\n%s", err, out)
	}
	probe := exec.Command("debugfs", "-R", "cat /home/hk/stop-data", h.state("stoppable", "rootfs.ext4"))
	if out, err := probe.CombinedOutput(); err != nil || !strings.Contains(string(out), "stop-persistent-data") {
		t.Fatalf("guest write lost: %v\n%s", err, out)
	}
	state := read(t, h.state("stoppable", "state.json"))
	h.must("", "stop", "stoppable")
	if !bytes.Equal(state, read(t, h.state("stoppable", "state.json"))) {
		t.Fatal("idempotent stop changed state")
	}
	for _, file := range []string{"rootfs.ext4", "seed.ext4", "config.json"} {
		if _, err := os.Stat(h.state("stoppable", file)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStopConflictsWithCreateWithoutGlobalWait(t *testing.T) {
	f := newFetchFixture(t)
	marker := filepath.Join(f.dir, "create-boundary")
	cmd := f.command("create", "--name", "creating", "--with-git-config")
	cmd.Env = append(cmd.Env, "HK_E2E_TRANSFER_BOUNDARY="+marker, "HK_E2E_TRANSFER_CANCEL=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Signal(syscall.SIGTERM); <-done }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("create did not reach controlled import boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	stop := exec.CommandContext(ctx, f.bin, "stop", "--force", "creating")
	stop.Env, stop.Dir = f.env, f.cwd
	out, err := stop.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "lifecycle operation") {
		t.Fatalf("stop waited on global create lock: %v %s", err, out)
	}
}

func TestStopProjectDuringFetchAndAttach(t *testing.T) {
	f := newFetchFixture(t)
	f.create("fetch-source")
	f.create("fetch-sibling")
	f.must("set -eu\nprintf 'committed edit\\n' > 'file with spaces'\ngit commit -am 'before stop'\n", "attach", "fetch-source")
	first := guestCommit(t, f.harness, "fetch-source", "HEAD")
	assertCleanFetch(t, f, "fetch-source", f.branch, first)
	f.must("set -eu\nhead -c 196608 /dev/urandom > payload.bin\ngit add payload.bin\ngit commit -m 'unfetched stop commit'\n", "attach", "fetch-source")
	last := guestCommit(t, f.harness, "fetch-source", "HEAD")
	protectDirtyHost(t, f)
	protected := snapshotHost(t, f)
	refs := hostRefs(t, f)
	paused := startPausedFetch(t, f, "HK_E2E_FETCH_DURING_DOWNLOAD")
	f.must(`set -eu
printf 'staged value\n' > staged-only
git add staged-only
printf 'unstaged value\n' >> 'file with spaces'
printf 'untracked value\n' > untracked-only
printf 'ignored value\n' > ignored-guest
printf 'outside value\n' > /home/hk/outside-stop
`, "attach", "fetch-source")
	indexBefore := f.must("/bin/busybox sha256sum .git/index\n", "attach", "fetch-source")
	finishAttach := openAttachForStop(t, f.harness, "fetch-source")
	identity := read(t, f.state("fetch-source", "identity"))
	known := read(t, f.state("fetch-source", "known_hosts"))
	instance := instanceID(t, f.harness, "fetch-source")
	pid := vmPID(t, f.state("fetch-source", "state.json"))
	f.must("", "stop", "fetch-source")
	finishAttach()
	assertGone(t, pid)
	if out, err := paused.release(t); err == nil {
		t.Fatalf("interrupted fetch succeeded: %s", out)
	}
	if hostRefs(t, f) != refs {
		t.Fatal("stop during fetch published partial refs")
	}
	assertFetchTemporaryFilesRemoved(t, paused)
	assertHostUnchanged(t, f, protected)
	if instanceID(t, f.harness, "fetch-source") != instance || !bytes.Equal(identity, read(t, f.state("fetch-source", "identity"))) || !bytes.Equal(known, read(t, f.state("fetch-source", "known_hosts"))) {
		t.Fatal("stop changed persistent identity")
	}
	for path, want := range map[string]string{
		"/home/hk/workspace/staged-only":                 "staged value",
		"/home/hk/workspace/untracked-only":              "untracked value",
		"/home/hk/workspace/ignored-guest":               "ignored value",
		"/home/hk/outside-stop":                          "outside value",
		"/home/hk/workspace/file with spaces":            "unstaged value",
		"/home/hk/workspace/.git/refs/heads/" + f.branch: last,
		"/home/hk/workspace/.git/config":                 "fetch@example.invalid",
	} {
		data := stoppedGuestFile(t, f.harness, "fetch-source", path)
		if !strings.Contains(string(data), want) {
			t.Fatalf("lost %s: %s", path, data)
		}
	}
	indexHash := sha256.Sum256(stoppedGuestFile(t, f.harness, "fetch-source", "/home/hk/workspace/.git/index"))
	if !strings.Contains(indexBefore, hex.EncodeToString(indexHash[:])) {
		t.Fatal("stop changed the guest index and staged changes")
	}
	assertStoppedFilesystem(t, f.harness, "fetch-source")
	f.must("test -f 'file with spaces' && printf 'SIBLING_ALIVE\\n'\n", "attach", "fetch-sibling")
	if out, err := f.run("", "delete", "fetch-source"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("stopped VM lost delete protection: %v %s", err, out)
	}
}

func stoppedGuestFile(t *testing.T, h *harness, name, path string) []byte {
	t.Helper()
	target := filepath.Join(t.TempDir(), "guest-file")
	request := "dump " + strconv.Quote(path) + " " + strconv.Quote(target)
	if out, err := exec.Command("debugfs", "-R", request, h.state(name, "rootfs.ext4")).CombinedOutput(); err != nil {
		t.Fatalf("read stopped disk: %v %s", err, out)
	}
	return read(t, target)
}

func assertStoppedFilesystem(t *testing.T, h *harness, name string) {
	t.Helper()
	assertCleanStoppedFilesystem(t, h.state(name, "rootfs.ext4"))
	if !strings.Contains(string(read(t, h.state(name, "console.log"))), "HABIKAT_SHUTDOWN_CLEAN") {
		t.Fatal("guest did not finish clean shutdown")
	}
}

func openAttachForStop(t *testing.T, h *harness, name string) func() {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "stop-pty")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, h.bin, "attach", name)
	cmd.Dir, cmd.Env = h.cwd, h.env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		cancel()
		if !finished {
			<-done
		}
	})
	chunks := make(chan string, 8)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				select {
				case chunks <- string(buffer[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	if _, err := master.Write([]byte("stty -echo; printf 'STOP_''ATTACH_READY\\n'; sleep 60\n")); err != nil {
		t.Fatal(err)
	}
	var transcript strings.Builder
	for !strings.Contains(transcript.String(), "STOP_ATTACH_READY") {
		select {
		case part := <-chunks:
			transcript.WriteString(part)
		case <-ctx.Done():
			t.Fatalf("attach not ready: %s", transcript.String())
		}
	}
	return func() {
		t.Helper()
		select {
		case <-done:
			finished = true
		case <-ctx.Done():
			t.Fatal("attach did not exit after stop")
		}
		after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if before.Lflag != after.Lflag || before.Iflag != after.Iflag || before.Oflag != after.Oflag {
			t.Fatal("stop did not restore attach terminal modes")
		}
	}
}

func stopState(t *testing.T, h *harness, name string) map[string]any {
	t.Helper()
	var state map[string]any
	if err := json.Unmarshal(read(t, h.state(name, "state.json")), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func assertCleanStoppedFilesystem(t *testing.T, disk string) {
	t.Helper()
	cmd := exec.Command("e2fsck", "-f", "-n", disk)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read-only filesystem check: %v\n%s", err, out)
	}
	t.Logf("read-only e2fsck passed:\n%s", out)
	cmd = exec.Command("dumpe2fs", "-h", disk)
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Filesystem state:         clean") {
		t.Fatalf("filesystem not marked clean: %v\n%s", err, out)
	}
	t.Log("Filesystem state: clean")
}

func TestRealStopTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"timeout", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			image := stopFixtureImage(t, `#!/bin/sh
echo 'hk: fixture shutdown blocked' > /dev/console
while :; do sleep 60; done
`, true)
			h.env = append(h.env, "HK_IMAGE_DIR="+image)
			h.must("", "create", "--name", "blocked")
			h.must("printf 'durable before force\n' > ~/durable\nsync\nexit\n", "attach", "blocked")
			pid := vmPID(t, h.state("blocked", "state.json"))
			identity := read(t, h.state("blocked", "identity"))
			var output bytes.Buffer
			cmd := h.command("stop", "blocked")
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() { cmd.Process.Kill() })
			awaitConsole(t, h, "blocked", "hk: fixture shutdown blocked")
			if kind == "cancellation" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(output.String(), "may still complete") {
					t.Fatalf("incomplete shutdown: %v %s", err, output.String())
				}
			case <-time.After(40 * time.Second):
				t.Fatal("stop wait was unbounded")
			}
			assertVMAlive(t, pid)
			if state := stopState(t, h, "blocked"); state["pid"] != float64(pid) || state["stopping"] != true {
				t.Fatalf("lost live identity: %v", state)
			}
			out := h.must("", "stop", "--force", "blocked")
			if !strings.Contains(out, "buffered guest data may be lost") {
				t.Fatal("missing force warning")
			}
			assertGone(t, pid)
			if data := stoppedGuestFile(t, h, "blocked", "/home/hk/durable"); string(data) != "durable before force\n" {
				t.Fatalf("lost previously synced fixture: %q", data)
			}
			if !bytes.Equal(identity, read(t, h.state("blocked", "identity"))) {
				t.Fatal("forced fallback changed identity")
			}
			if _, err := os.Stat(h.state("blocked", "rootfs.ext4")); err != nil {
				t.Fatal(err)
			}
			mustStartElsewhere(t, h, "blocked")
			if out := h.must("cat ~/durable\n", "attach", "blocked"); !strings.Contains(out, "durable before force") {
				t.Fatalf("start after forced stop lost durable fixture: %s", out)
			}
		})
	}
}

func stopFixtureImage(t *testing.T, shutdown string, capable bool) string {
	t.Helper()
	base := os.Getenv("HK_IMAGE_DIR")
	if base == "" {
		t.Fatal("stop E2E requires explicit HK_IMAGE_DIR")
	}
	dir := t.TempDir()
	for _, name := range []string{"firecracker", "vmlinux"} {
		if err := os.Symlink(filepath.Join(base, name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	disk := filepath.Join(dir, "rootfs.ext4")
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", "--", filepath.Join(base, "rootfs.ext4"), disk).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture image: %v %s", err, out)
	}
	script := filepath.Join(dir, "shutdown")
	write(t, script, []byte(shutdown))
	for _, command := range []string{"rm /etc/rcShutdown", "write " + script + " /etc/rcShutdown", "set_inode_field /etc/rcShutdown mode 0100755"} {
		if out, err := exec.Command("debugfs", "-w", "-R", command, disk).CombinedOutput(); err != nil {
			t.Fatalf("fixture shutdown: %v %s", err, out)
		}
	}
	var manifest map[string]string
	if err := json.Unmarshal(read(t, filepath.Join(base, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["rootfs.ext4"] = fmt.Sprintf("%x", sha256.Sum256(read(t, disk)))
	if !capable {
		delete(manifest, "shutdown")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "manifest.json"), data)
	return dir
}

func awaitConsole(t *testing.T, h *harness, name, marker string) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		data, _ := os.ReadFile(h.state(name, "console.log"))
		if strings.Contains(string(data), marker) {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("console never reached %q:\n%s", marker, data)
		}
	}
}

func assertVMAlive(t *testing.T, pid int) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if len(fields) == 0 || fields[0] == "Z" {
		t.Fatal("ordinary stop killed an unresponsive guest")
	}
}

func TestCreateRefusesImageWithoutGracefulShutdown(t *testing.T) {
	h := newHarness(t)
	image := stopFixtureImage(t, "#!/bin/sh\nexit 0\n", false)
	h.env = append(h.env, "HK_IMAGE_DIR="+image)
	if out, err := h.run("", "create", "--name", "incapable"); err == nil || !strings.Contains(out, "does not support graceful shutdown") {
		t.Fatalf("create from image without graceful shutdown: %v %s", err, out)
	}
	if _, err := os.Lstat(filepath.Dir(h.state("incapable", "state.json"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused create left VM state: %v", err)
	}
}

func TestCreateRefusesRootDiskChangedAfterImageVerification(t *testing.T) {
	h := newHarness(t)
	legacy := stopFixtureImage(t, "#!/bin/sh\nexit 0\n", false)
	image := t.TempDir()
	for _, name := range []string{"firecracker", "vmlinux", "rootfs.ext4", "manifest.json"} {
		if err := os.Symlink(filepath.Join(os.Getenv("HK_IMAGE_DIR"), name), filepath.Join(image, name)); err != nil {
			t.Fatal(err)
		}
	}
	realCopy, err := exec.LookPath("cp")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	copyScript := filepath.Join(bin, "cp")
	write(t, copyScript, []byte(`#!/bin/sh
set -eu
ln -sfn "$LEGACY_ROOTFS" "$HK_IMAGE_DIR/rootfs.ext4"
ln -sfn "$LEGACY_MANIFEST" "$HK_IMAGE_DIR/manifest.json"
exec "$REAL_CP" "$@"
`))
	if err := os.Chmod(copyScript, 0700); err != nil {
		t.Fatal(err)
	}
	h.env = append(h.env,
		"HK_IMAGE_DIR="+image, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_CP="+realCopy, "LEGACY_ROOTFS="+filepath.Join(legacy, "rootfs.ext4"),
		"LEGACY_MANIFEST="+filepath.Join(legacy, "manifest.json"),
	)
	if out, err := h.run("", "create", "--name", "swapped"); err == nil || !strings.Contains(out, "SHA-256 mismatch for rootfs.ext4") {
		t.Fatalf("create from a changed image: %v %s", err, out)
	}
	if _, err := os.Lstat(filepath.Dir(h.state("swapped", "state.json"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused create left VM state: %v", err)
	}
}
