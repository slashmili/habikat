//go:build e2e && !nixexperiment

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startElsewhere(t *testing.T, h *harness, name string) (string, error) {
	t.Helper()
	elsewhere, err := os.MkdirTemp(h.dir, "elsewhere-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, "start", name)
	cmd.Env = append(append([]string(nil), h.env...), "HK_IMAGE_DIR="+filepath.Join(elsewhere, "no-such-image"))
	cmd.Dir = elsewhere
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustStartElsewhere(t *testing.T, h *harness, name string) string {
	t.Helper()
	out, err := startElsewhere(t, h, name)
	if err != nil {
		t.Fatalf("start %s: %v\n%s", name, err, out)
	}
	return out
}

func persistentIdentity(t *testing.T, h *harness, name string) map[string]any {
	t.Helper()
	evidence := map[string]any{}
	for _, file := range []string{"identity", "known_hosts", "seed.ext4"} {
		evidence[file] = sha256.Sum256(read(t, h.state(name, file)))
	}
	state := stopState(t, h, name)
	for _, field := range []string{"instance_id", "project", "source_directory", "shutdown", "launch"} {
		evidence[field] = state[field]
	}
	return evidence
}

func assertPersistentIdentity(t *testing.T, h *harness, name string, want map[string]any) {
	t.Helper()
	if got := persistentIdentity(t, h, name); !reflect.DeepEqual(got, want) {
		t.Fatalf("persistent identity changed:\n%v\nwant:\n%v", got, want)
	}
}

func liveVMProcesses(t *testing.T, h *harness, name string) int {
	t.Helper()
	config := h.state(name, "config.json")
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(data), "\x00")
		if len(args) > 4 && filepath.Base(args[0]) == "firecracker" && args[4] == config {
			count++
		}
	}
	return count
}

func hostBootID(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(read(t, "/proc/sys/kernel/random/boot_id")))
}

func TestStartRestoresProjectVM(t *testing.T) {
	f := newFetchFixture(t)
	f.create("fetch-source")
	f.create("fetch-sibling")
	f.must("set -eu\nprintf 'committed before stop\\n' > 'file with spaces'\ngit commit -am 'before stop'\n", "attach", "fetch-source")
	beforeStop := guestCommit(t, f.harness, "fetch-source", "HEAD")
	assertCleanFetch(t, f, "fetch-source", f.branch, beforeStop)
	namespace := fetchedRef(t, f.harness, "fetch-source", f.branch)
	f.must(`set -eu
printf 'staged value\n' > staged-only
git add staged-only
printf 'unstaged value\n' >> 'file with spaces'
printf 'untracked value\n' > untracked-only
printf 'ignored value\n' > ignored-guest
printf 'outside repository content\n' > /home/hk/outside-fetch
`, "attach", "fetch-source")
	guest := dirtyGuestEvidence(t, f)
	gitConfig := f.must("git config user.email\n", "attach", "fetch-source")
	identity := persistentIdentity(t, f.harness, "fetch-source")
	protectDirtyHost(t, f)
	protected := snapshotHost(t, f)
	first := stopState(t, f.harness, "fetch-source")

	f.must("", "stop", "fetch-source")
	assertGone(t, int(first["pid"].(float64)))
	assertStoppedFilesystem(t, f.harness, "fetch-source")
	if out, err := f.run("", "fetch", "--force", "fetch-source"); err == nil || !strings.Contains(out, "hk start fetch-source") {
		t.Fatalf("stopped fetch did not suggest start: %v %s", err, out)
	}

	if out := mustStartElsewhere(t, f.harness, "fetch-source"); !strings.Contains(out, "Started fetch-source.") || strings.Contains(out, "graceful") {
		t.Fatalf("start output: %s", out)
	}
	second := stopState(t, f.harness, "fetch-source")
	if second["pid"] == first["pid"] && second["start_time"] == first["start_time"] || second["ready"] != true || second["boot_id"] != hostBootID(t) {
		t.Fatalf("runtime identity not renewed: %v -> %v", first, second)
	}
	assertPersistentIdentity(t, f.harness, "fetch-source", identity)
	if got := dirtyGuestEvidence(t, f); got != guest {
		t.Fatalf("restart changed guest files/index/branches:\n%s\nwant:\n%s", got, guest)
	}
	if got := f.must("git config user.email\n", "attach", "fetch-source"); got != gitConfig {
		t.Fatalf("Git configuration changed: %q -> %q", gitConfig, got)
	}
	f.must("set -eu\ntest \"$(cat ignored-guest)\" = 'ignored value'\ntest \"$(cat /home/hk/outside-fetch)\" = 'outside repository content'\n", "attach", "fetch-source")
	t.Run("interactive-terminal", func(t *testing.T) { testTerminal(t, f.harness, "fetch-source", "/home/hk/workspace") })

	if out, err := f.run("", "fetch", "fetch-source"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("dirty fetch after restart bypassed approval: %v %s", err, out)
	}
	f.must("git commit -m 'after restart'\n", "attach", "fetch-source")
	afterStart := guestCommit(t, f.harness, "fetch-source", "HEAD")
	assertDirtyFetchOutput(t, f.must("", "fetch", "--force", "fetch-source"))
	if fetchedRef(t, f.harness, "fetch-source", f.branch) != namespace {
		t.Fatal("fetch namespace changed across restart")
	}
	assertRef(t, f.host, namespace, afterStart)
	assertHostUnchanged(t, f, protected)
	f.must("test -f 'file with spaces' && printf 'SIBLING_ALIVE\\n'\n", "attach", "fetch-sibling")

	pid := vmPID(t, f.state("fetch-source", "state.json"))
	if out := mustStartElsewhere(t, f.harness, "fetch-source"); !strings.Contains(out, "already running") || vmPID(t, f.state("fetch-source", "state.json")) != pid {
		t.Fatalf("repeated start was not a no-op: %s", out)
	}
	f.must("", "stop", "fetch-source")
	assertGone(t, pid)
	assertCleanStoppedFilesystem(t, f.state("fetch-source", "rootfs.ext4"))
	if count := bytes.Count(read(t, f.state("fetch-source", "console.log")), []byte("HABIKAT_SHUTDOWN_CLEAN")); count != 2 {
		t.Fatalf("clean guest shutdowns = %d, want 2", count)
	}
	if out, err := f.run("", "delete", "fetch-source"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("stopped VM lost delete protection: %v %s", err, out)
	}
}

func TestStartEmptyVMIdempotentAndConcurrent(t *testing.T) {
	h := newHarness(t)
	h.must("", "create", "--name", "empty")
	h.must("printf 'empty vm data\\n' > ~/empty-data\nexit\n", "attach", "empty")
	identity := persistentIdentity(t, h, "empty")
	h.must("", "stop", "empty")
	h.must("", "stop", "empty")
	results := make(chan string, 3)
	for range 3 {
		go func() {
			out, err := startElsewhere(t, h, "empty")
			results <- fmt.Sprintf("%v: %s", err, out)
		}()
	}
	started := 0
	for range 3 {
		result := <-results
		switch {
		case strings.HasPrefix(result, "<nil>: Started empty."):
			started++
		case strings.Contains(result, "lifecycle operation"), strings.HasPrefix(result, "<nil>: empty is already running."):
		default:
			t.Fatalf("unexpected concurrent start result: %s", result)
		}
	}
	if started != 1 || liveVMProcesses(t, h, "empty") != 1 {
		t.Fatalf("concurrent starts: started=%d processes=%d", started, liveVMProcesses(t, h, "empty"))
	}
	if out := mustStartElsewhere(t, h, "empty"); !strings.Contains(out, "already running") {
		t.Fatalf("idempotent start: %s", out)
	}
	if out := h.must("cat ~/empty-data\npwd\n", "attach", "empty"); !strings.Contains(out, "empty vm data") || !strings.Contains(out, "/home/hk") {
		t.Fatalf("empty VM lost data: %s", out)
	}
	assertPersistentIdentity(t, h, "empty", identity)
	h.must("", "stop", "empty")
	assertStoppedFilesystem(t, h, "empty")
}

func TestStartReconcilesUnexpectedExitAndStaleRuntime(t *testing.T) {
	h := newHarness(t)
	h.must("", "create", "--name", "crashy")
	h.must("printf 'durable before crash\\n' > ~/durable\nsync\nexit\n", "attach", "crashy")
	identity := persistentIdentity(t, h, "crashy")
	stopFixtureVM(t, h, "crashy")
	if out, err := h.run("exit\n", "attach", "crashy"); err == nil || !strings.Contains(out, "hk start crashy") {
		t.Fatalf("crashed VM did not suggest start: %v %s", err, out)
	}
	mustStartElsewhere(t, h, "crashy")
	if out := h.must("cat ~/durable\n", "attach", "crashy"); !strings.Contains(out, "durable before crash") {
		t.Fatalf("synced data lost after unexpected exit: %s", out)
	}
	assertPersistentIdentity(t, h, "crashy", identity)
	h.must("", "stop", "crashy")

	unrelated := exec.Command("sleep", "300")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	stat := string(read(t, fmt.Sprintf("/proc/%d/stat", unrelated.Process.Pid)))
	state := stopState(t, h, "crashy")
	state["pid"] = unrelated.Process.Pid
	state["start_time"] = strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])[19]
	state["ready"] = true
	state["boot_id"] = "00000000-0000-0000-0000-000000000000"
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	write(t, h.state("crashy", "state.json"), data)
	mustStartElsewhere(t, h, "crashy")
	assertVMAlive(t, unrelated.Process.Pid)
	if pid := vmPID(t, h.state("crashy", "state.json")); pid == unrelated.Process.Pid || liveVMProcesses(t, h, "crashy") != 1 {
		t.Fatalf("stale metadata was trusted: pid %d", pid)
	}
	h.must("cat ~/durable\n", "attach", "crashy")
	t.Log("Simulated stale host-boot/PID metadata only; no host reboot was performed.")
}

func copiedImage(t *testing.T) string {
	t.Helper()
	base := os.Getenv("HK_IMAGE_DIR")
	if base == "" {
		t.Fatal("start E2E requires explicit HK_IMAGE_DIR")
	}
	dir := t.TempDir()
	for _, name := range []string{"firecracker", "vmlinux", "rootfs.ext4", "manifest.json"} {
		if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", "--preserve=mode", "--", filepath.Join(base, name), filepath.Join(dir, name)).CombinedOutput(); err != nil {
			t.Fatalf("copy image: %v %s", err, out)
		}
	}
	return dir
}

func storageEvidence(t *testing.T, h *harness, name string) map[string][32]byte {
	t.Helper()
	evidence := map[string][32]byte{}
	for _, file := range []string{"rootfs.ext4", "seed.ext4", "identity", "known_hosts", "state.json"} {
		evidence[file] = sha256.Sum256(read(t, h.state(name, file)))
	}
	return evidence
}

func assertStartRefused(t *testing.T, h *harness, name, fragment string) {
	t.Helper()
	want := storageEvidence(t, h, name)
	out, err := startElsewhere(t, h, name)
	if err == nil || !strings.Contains(out, fragment) || !strings.Contains(out, "preserved") {
		t.Fatalf("start was not refused with %q: %v %s", fragment, err, out)
	}
	if liveVMProcesses(t, h, name) != 0 {
		t.Fatal("refused start left a VM process")
	}
	if got := storageEvidence(t, h, name); !reflect.DeepEqual(got, want) {
		t.Fatal("refused start changed disks, keys, or state")
	}
}

func TestStartFailuresPreserveDisk(t *testing.T) {
	h := newHarness(t)
	image := copiedImage(t)
	h.env = append(h.env, "HK_IMAGE_DIR="+image)
	h.must("", "create", "--name", "fragile")
	h.must("printf 'durable before failures\\n' > ~/durable\nexit\n", "attach", "fragile")
	identity := persistentIdentity(t, h, "fragile")
	h.must("", "stop", "fragile")

	kernel := read(t, filepath.Join(image, "vmlinux"))
	write(t, filepath.Join(image, "vmlinux"), append(append([]byte(nil), kernel...), 0))
	assertStartRefused(t, h, "fragile", "does not match its recorded SHA-256")
	write(t, filepath.Join(image, "vmlinux"), kernel)
	if err := os.Rename(filepath.Join(image, "firecracker"), filepath.Join(image, "firecracker.moved")); err != nil {
		t.Fatal(err)
	}
	assertStartRefused(t, h, "fragile", "unavailable")
	if err := os.Rename(filepath.Join(image, "firecracker.moved"), filepath.Join(image, "firecracker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.state("fragile", "api.sock")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	write(t, h.state("fragile", "api.sock"), []byte("not a socket"))
	assertStartRefused(t, h, "fragile", "not a socket")
	if err := os.Remove(h.state("fragile", "api.sock")); err != nil {
		t.Fatal(err)
	}

	original := read(t, h.state("fragile", "state.json"))
	brokenKernel := filepath.Join(t.TempDir(), "vmlinux")
	write(t, brokenKernel, []byte("not an ELF kernel"))
	var state map[string]any
	if err := json.Unmarshal(original, &state); err != nil {
		t.Fatal(err)
	}
	launch := state["launch"].(map[string]any)
	launch["kernel"] = map[string]any{"path": brokenKernel, "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte("not an ELF kernel")))}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	write(t, h.state("fragile", "state.json"), data)
	if out, err := startElsewhere(t, h, "fragile"); err == nil || !strings.Contains(out, "Firecracker exited before SSH was ready") || !strings.Contains(out, "preserved") {
		t.Fatalf("real boot failure: %v %s", err, out)
	}
	if liveVMProcesses(t, h, "fragile") != 0 || stopState(t, h, "fragile")["pid"] != float64(0) {
		t.Fatal("failed boot left runtime state")
	}
	write(t, h.state("fragile", "state.json"), original)

	cancelStartAfterLaunch(t, h, "fragile")
	if liveVMProcesses(t, h, "fragile") != 0 || stopState(t, h, "fragile")["pid"] != float64(0) {
		t.Fatal("cancelled start left its process or runtime state")
	}
	assertPersistentIdentity(t, h, "fragile", identity)
	mustStartElsewhere(t, h, "fragile")
	if out := h.must("cat ~/durable\n", "attach", "fragile"); !strings.Contains(out, "durable before failures") {
		t.Fatalf("healthy start after failures lost data: %s", out)
	}
	assertPersistentIdentity(t, h, "fragile", identity)
}

func cancelStartAfterLaunch(t *testing.T, h *harness, name string) {
	t.Helper()
	cmd := h.command("start", name)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(20 * time.Second)
	pid := 0
	for pid == 0 {
		if state := stopState(t, h, name); state["pid"] != float64(0) && state["ready"] == false {
			pid = int(state["pid"].(float64))
		}
		if time.Now().After(deadline) {
			t.Fatal("did not observe a launched, unready start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(output.String(), "preserved") {
			t.Fatalf("cancelled start: %v %s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled start did not return")
	}
	assertGone(t, pid)
}
