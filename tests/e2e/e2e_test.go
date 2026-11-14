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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type harness struct {
	t        *testing.T
	dir, bin string
	env      []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "hk-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, bin: filepath.Join(dir, "hk")}
	// Deliberately exercise literal shell-sensitive state paths.
	state := filepath.Join(dir, "state '$%")
	h.env = append(os.Environ(), "HK_STATE_DIR="+state)
	build := exec.Command("go", "build", "-o", h.bin, "../../cmd/hk")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, out)
	}
	t.Cleanup(func() {
		entries, err := os.ReadDir(state)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				cmd := h.command("delete", entry.Name())
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("cleanup %s: %v: %s; retained %s", entry.Name(), err, out, dir)
					return
				}
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *harness) state(name, file string) string {
	return filepath.Join(h.dir, "state '$%", name, file)
}

func (h *harness) command(args ...string) *exec.Cmd {
	cmd := exec.Command(h.bin, args...)
	cmd.Env = h.env
	return cmd
}

func (h *harness) run(input string, args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 100*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *harness) must(input string, args ...string) string {
	h.t.Helper()
	out, err := h.run(input, args...)
	if err != nil {
		h.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func vmPID(t *testing.T, path string) int {
	t.Helper()
	var state struct{ PID int }
	if err := json.Unmarshal(read(t, path), &state); err != nil {
		t.Fatal(err)
	}
	return state.PID
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
		if len(fields) == 0 || fields[0] != "Z" {
			t.Fatalf("VM process %d is still live", pid)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestRealVMLifecycle(t *testing.T) {
	h := newHarness(t)
	h.must("", "create", "--name", "one")
	pid := vmPID(t, h.state("one", "state.json"))
	identity := read(t, h.state("one", "identity"))
	known := read(t, h.state("one", "known_hosts"))
	if out, err := h.run("", "create", "--name", "one"); err == nil {
		t.Fatalf("duplicate name accepted: %s", out)
	}
	if !bytes.Equal(identity, read(t, h.state("one", "identity"))) || vmPID(t, h.state("one", "state.json")) != pid {
		t.Fatal("duplicate create changed the existing VM")
	}
	out := h.must(`set -eu
test "$(id -u)" = 1000
test "$(id -un)" = hk
test "$HOME" = /home/hk
test ! -w /
test ! -w /home
test ! -w /etc
test ! -w /bin
test ! -r /etc/ssh/host_key
test ! -r /run/provision/seed/host_key
test "$(ls /sys/class/net)" = lo
printf 'persistent guest data\n' > "$HOME/probe"
uname -r
printf 'NONROOT_OK\n'
exit
`, "attach", "one")
	if !strings.Contains(out, "6.1.188") || !strings.Contains(out, "NONROOT_OK") {
		t.Fatalf("guest checks did not run: %s", out)
	}
	if out := h.must("cat ~/probe\nexit\n", "attach", "one"); !strings.Contains(out, "persistent guest data") {
		t.Fatalf("reattach lost guest files: %s", out)
	}
	if _, err := h.run("exit 37\n", "attach", "one"); exitCode(err) != 37 {
		t.Fatalf("remote exit code: %v", err)
	}
	t.Run("interactive-terminal", func(t *testing.T) { testTerminal(t, h, "one") })

	h.must("", "create", "--name", "two")
	otherIdentity := read(t, h.state("two", "identity"))
	otherKnown := read(t, h.state("two", "known_hosts"))
	if bytes.Equal(identity, otherIdentity) || bytes.Equal(known, otherKnown) {
		t.Fatal("VMs shared client or host identities")
	}
	h.must("test ! -e ~/probe || exit 1\nexit\n", "attach", "two")
	write(t, h.state("one", "known_hosts"), otherKnown)
	if out, err := h.run("echo UNSAFE\nexit\n", "attach", "one"); exitCode(err) != 255 || strings.Contains(out, "UNSAFE") {
		t.Fatalf("wrong host key accepted: %v %s", err, out)
	}
	write(t, h.state("one", "known_hosts"), known)
	write(t, h.state("one", "identity"), otherIdentity)
	if out, err := h.run("echo UNSAFE\nexit\n", "attach", "one"); exitCode(err) != 255 || strings.Contains(out, "UNSAFE") {
		t.Fatalf("another VM's client key accepted: %v %s", err, out)
	}
	write(t, h.state("one", "identity"), identity)
	h.must("", "delete", "one")
	assertGone(t, pid)
	if _, err := os.Stat(h.state("one", "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete retained VM keys/disk: %v", err)
	}
	h.must("", "delete", "one")
	if _, err := h.run("", "attach", "one"); err == nil {
		t.Fatal("attached a deleted VM")
	}
	h.must("", "create", "--name", "one")
	if bytes.Equal(identity, read(t, h.state("one", "identity"))) {
		t.Fatal("recreate reused credentials")
	}
	h.must("test ! -e ~/probe || exit 1\nexit\n", "attach", "one")
	// Simulate the persisted record at the small crash window after spawn and
	// before PID publication. Recovery must find only this VM's config path.
	pid = vmPID(t, h.state("one", "state.json"))
	write(t, h.state("one", "state.json"), []byte(`{"pid":0,"start_time":"","ready":false}`))
	h.must("", "delete", "one")
	assertGone(t, pid)
	h.must("echo STILL_RUNNING\nexit\n", "attach", "two")
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

func TestInterruptedCreate(t *testing.T) {
	h := newHarness(t)
	cmd := h.command("create", "--name", "interrupted")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(20 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(h.state("interrupted", "state.json"))
		if err == nil {
			var state struct {
				PID   int
				Ready bool
			}
			if json.Unmarshal(data, &state) == nil && state.PID > 0 && !state.Ready {
				pid = state.PID
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("did not observe a booting VM")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("cancelled create succeeded: %s", output.String())
	}
	assertGone(t, pid)
	if _, err := os.Stat(h.state("interrupted", "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancellation retained disposable state: %v", err)
	}
}

func TestBootFailureCleanup(t *testing.T) {
	h := newHarness(t)
	images := os.Getenv("HK_IMAGE_DIR")
	if images == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		images = filepath.Join(cache, "habikato", "image")
	}
	broken := filepath.Join(h.dir, "broken-image")
	if err := os.Mkdir(broken, 0700); err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(read(t, filepath.Join(images, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"firecracker", "rootfs.ext4"} {
		if err := os.Symlink(filepath.Join(images, file), filepath.Join(broken, file)); err != nil {
			t.Fatal(err)
		}
	}
	badKernel := []byte("not an ELF kernel")
	write(t, filepath.Join(broken, "vmlinux"), badKernel)
	hash := sha256.Sum256(badKernel)
	manifest["vmlinux"] = hex.EncodeToString(hash[:])
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(broken, "manifest.json"), data)
	h.env = append(h.env, "HK_IMAGE_DIR="+broken)
	if out, err := h.run("", "create", "--name", "badboot"); err == nil || !strings.Contains(out, "Firecracker") {
		t.Fatalf("expected actual Firecracker boot failure: %v %s", err, out)
	}
	if _, err := os.Stat(h.state("badboot", "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("boot failure retained disposable state: %v", err)
	}
}

func testTerminal(t *testing.T, h *harness, name string) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "pty-master")
	defer master.Close()
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
	defer slave.Close()
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	resize := func(rows, cols uint16) {
		t.Helper()
		if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
			t.Fatal(err)
		}
	}
	resize(24, 80)
	cmd := h.command("attach", name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	chunks := make(chan string, 128)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunks <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	var transcript strings.Builder
	await := func(marker string) {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for !strings.Contains(transcript.String(), marker) {
			select {
			case chunk, open := <-chunks:
				if !open {
					t.Fatalf("terminal closed before %q: %s", marker, transcript.String())
				}
				transcript.WriteString(chunk)
			case <-timer.C:
				t.Fatalf("terminal timeout awaiting %q: %s", marker, transcript.String())
			}
		}
	}
	send := func(value string) {
		t.Helper()
		if _, err := master.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	send("stty -echo; test -t 0 && test -t 1 && printf 'PT''Y_READY\\n'; stty size\n")
	await("PTY_READY")
	await("24 80")
	resize(41, 101)
	send("stty size\n")
	await("41 101")
	send("printf 'SL''EEPING\\n'; sleep 30\n")
	await("SLEEPING")
	send("\x03")
	send("printf 'INT''ERRUPTED\\n'\n")
	await("INTERRUPTED")
	send("exit 23\n")
	if err := cmd.Wait(); exitCode(err) != 23 {
		t.Fatalf("PTY exit status: %v: %s", err, transcript.String())
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if before.Lflag != after.Lflag || before.Iflag != after.Iflag || before.Oflag != after.Oflag {
		t.Fatal("attach did not restore host terminal modes")
	}
}
