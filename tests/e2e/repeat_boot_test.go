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
	"strings"
	"testing"
	"time"
)

func TestImageRepeatBootPreservesUserData(t *testing.T) {
	h := newHarness(t)
	h.must("", "create", "--name", "repeat")
	original := stopState(t, h, "repeat")
	identities := repeatBootIdentities(t, h)
	h.must(`set -eu
mkdir -p ~/workspace
printf 'project data\n' > ~/workspace/user-data
printf 'home data\n' > ~/user-data
chmod 640 ~/workspace/user-data
chmod 600 ~/user-data
chmod 750 ~ ~/.ssh
chmod 400 ~/.ssh/authorized_keys
`, "attach", "repeat")
	h.must("", "stop", "repeat")
	assertGone(t, int(original["pid"].(float64)))
	assertStoppedFilesystem(t, h, "repeat")
	done := relaunchRepeatBootFixture(t, h)
	waitRepeatBootSSH(t, h)
	out := h.must(`set -eu
test "$(cat ~/workspace/user-data)" = 'project data'
test "$(cat ~/user-data)" = 'home data'
test "$(/bin/busybox stat -c %a ~)" = 750
test "$(/bin/busybox stat -c %a ~/.ssh)" = 750
test "$(/bin/busybox stat -c %a ~/.ssh/authorized_keys)" = 400
test "$(/bin/busybox stat -c %a ~/workspace/user-data)" = 640
test "$(/bin/busybox stat -c %a ~/user-data)" = 600
test "$(/bin/busybox stat -c %u ~/workspace/user-data)" = 1000
printf 'REPEAT_BOOT_DATA_AND_MODES_OK\n'
`, "attach", "repeat")
	if !strings.Contains(out, "REPEAT_BOOT_DATA_AND_MODES_OK") {
		t.Fatalf("repeat boot checks did not finish: %s", out)
	}
	current := stopState(t, h, "repeat")
	if current["instance_id"] != original["instance_id"] || current["pid"] == original["pid"] && current["start_time"] == original["start_time"] {
		t.Fatal("repeat boot lost instance identity or reused runtime identity")
	}
	if identities != repeatBootIdentities(t, h) {
		t.Fatal("repeat boot replaced keys or seed disk")
	}
	h.must("", "stop", "repeat")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repeat guest did not exit cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("repeat guest did not exit")
	}
	assertStoppedFilesystem(t, h, "repeat")
	if bytes.Count(read(t, h.state("repeat", "console.log")), []byte("HABIKAT_SHUTDOWN_CLEAN")) != 2 {
		t.Fatal("both boots must complete the guest clean shutdown path")
	}
	t.Log("Direct fixture relaunch preserved data, modes, ownership, keys and instance ID; both normal stops completed clean filesystem checks. This does not exercise hk start.")
}

func repeatBootIdentities(t *testing.T, h *harness) [3][32]byte {
	t.Helper()
	var hashes [3][32]byte
	for i, name := range []string{"identity", "known_hosts", "seed.ext4"} {
		hashes[i] = sha256.Sum256(read(t, h.state("repeat", name)))
	}
	return hashes
}

func relaunchRepeatBootFixture(t *testing.T, h *harness) <-chan error {
	t.Helper()
	for _, name := range []string{"api.sock", "vsock.sock", "vsock.sock_22"} {
		path := h.state("repeat", name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("unexpected fixture transient path: %s", path)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	log, err := os.OpenFile(h.state("repeat", "console.log"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(os.Getenv("HK_IMAGE_DIR")+"/firecracker", "--api-sock", h.state("repeat", "api.sock"), "--config-file", h.state("repeat", "config.json"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	})
	stat := string(read(t, fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid)))
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
	if len(fields) < 20 {
		t.Fatalf("invalid fixture process identity: %q", stat)
	}
	state := stopState(t, h, "repeat")
	state["pid"], state["start_time"], state["ready"] = cmd.Process.Pid, fields[19], true
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	write(t, h.state("repeat", "state.json"), data)
	return done
}

func waitRepeatBootSSH(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		cmd := exec.CommandContext(ctx, h.bin, "attach", "repeat")
		cmd.Dir, cmd.Env, cmd.Stdin = h.cwd, h.env, strings.NewReader("exit\n")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("repeat boot SSH did not become ready: %v %s", err, out)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
