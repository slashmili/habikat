//go:build e2e && nixexperiment

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNixosPersistenceIsolation(t *testing.T) {
	f := newFetchFixture(t)
	started := time.Now()
	f.create("nixos")
	t.Logf("create to SSH+SQL ready: %s", time.Since(started))
	identity := read(t, f.state("nixos", "identity"))
	known := read(t, f.state("nixos", "known_hosts"))
	id := instanceID(t, f.harness, "nixos")
	f.must(`set -eu
 test "$(id -u)" = 1000
 test "$PWD" = /home/hk/workspace
 git --version
 pg_isready -h /run/postgresql -U hk -d hk
 test "$(psql -XAt -d hk -c "select rolsuper from pg_roles where rolname=current_user")" = f
 psql -X -v ON_ERROR_STOP=1 -d hk -c "create table experiment (value text); insert into experiment values ('durable-row'); select * from experiment;"
 printf 'committed guest content\n' > nixos-commit
 git add nixos-commit
 git commit -m 'NixOS guest commit'
 printf 'uncommitted guest content\n' > nixos-uncommitted
 test "$(ls /sys/class/net)" = lo
 test ! -r /run/provision/seed/host_key
 test ! -r /run/habikat-ssh/host_key
 test ! -w /nix/store
 test -z "${SSH_AUTH_SOCK-}"
 test "$(findmnt -n -o FSTYPE /nix/store | sort -u)" = erofs
 test "$(findmnt -rn -t 9p,virtiofs,nfs | wc -l)" = 0
 test ! -e /run/current-system/sw/bin/sudo
 test ! -e /run/current-system/sw/bin/nix
 sshd -G -T -f /etc/ssh/sshd_config | tr '[:upper:]' '[:lower:]' > /tmp/habikat-sshd-effective
 grep -qx 'permitrootlogin no' /tmp/habikat-sshd-effective
 grep -qx 'passwordauthentication no' /tmp/habikat-sshd-effective
 grep -qx 'kbdinteractiveauthentication no' /tmp/habikat-sshd-effective
 grep -qx 'allowusers hk' /tmp/habikat-sshd-effective
 grep -qx 'allowagentforwarding no' /tmp/habikat-sshd-effective
 grep -qx 'allowtcpforwarding no' /tmp/habikat-sshd-effective
 grep -qx 'hostkey /run/habikat-ssh/host_key' /tmp/habikat-sshd-effective
 systemctl cat habikat-ssh@.service | grep -q 'sshd -i -e -f /etc/ssh/sshd_config'
 test "$(systemctl list-sockets --all --no-legend | grep -c 'vsock.*22')" = 1
 cat /etc/machine-id > ~/original-machine-id
 chmod 640 nixos-uncommitted
 free -m
 `, "attach", "nixos")
	commit := guestCommit(t, f.harness, "nixos", "HEAD")
	if out, err := f.run("", "fetch", "nixos"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("dirty fetch accepted: %v %s", err, out)
	}
	f.must("", "fetch", "--force", "nixos")
	namespace := fetchedRef(t, f.harness, "nixos", f.branch)
	assertRef(t, f.host, namespace, commit)
	if got := fixtureGit(t, f.host, "rev-parse", "HEAD"); strings.TrimSpace(got) != f.base {
		t.Fatal("host checkout changed")
	}
	if _, err := os.Stat(filepath.Join(f.host, "nixos-uncommitted")); !os.IsNotExist(err) {
		t.Fatal("uncommitted file transferred")
	}
	t.Run("interactive-terminal", func(t *testing.T) { testTerminal(t, f.harness, "nixos", "/home/hk/workspace") })
	for sample := 0; sample < 3; sample++ {
		state := nixosState(t, f.harness, "nixos")
		pid := int(state["pid"].(float64))
		status := read(t, fmt.Sprintf("/proc/%d/status", pid))
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				t.Logf("sample %d VMM %s", sample+1, line)
			}
		}
		started = time.Now()
		f.must("", "stop", "nixos")
		t.Logf("sample %d normal stop: %s", sample+1, time.Since(started))
		assertGone(t, pid)
		assertNixosCleanShutdown(t, f.harness, "nixos", sample+1)
		elsewhere, err := os.MkdirTemp(f.dir, "elsewhere-")
		if err != nil {
			t.Fatal(err)
		}
		cmd := f.command("start", "nixos")
		cmd.Dir = elsewhere
		cmd.Env = append(cmd.Env, "HK_IMAGE_DIR="+filepath.Join(elsewhere, "absent"))
		started = time.Now()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restart: %v %s", err, out)
		}
		t.Logf("sample %d restart to SSH+SQL ready: %s", sample+1, time.Since(started))
		f.must(`set -eu
 test "$(cat nixos-uncommitted)" = 'uncommitted guest content'
 test "$(stat -c %a nixos-uncommitted)" = 640
 cmp /etc/machine-id ~/original-machine-id
 test "$(psql -XAt -d hk -c "select count(*) from experiment where value='durable-row'")" = 1
 psql -X -v ON_ERROR_STOP=1 -d hk -c "insert into experiment values ('after-restart');"
 `, "attach", "nixos")
		if instanceID(t, f.harness, "nixos") != id || !bytes.Equal(identity, read(t, f.state("nixos", "identity"))) || !bytes.Equal(known, read(t, f.state("nixos", "known_hosts"))) {
			t.Fatal("identity changed across restart")
		}
		if fetchedRef(t, f.harness, "nixos", f.branch) != namespace || guestCommit(t, f.harness, "nixos", "HEAD") != commit {
			t.Fatal("repository identity changed")
		}
	}
	f.create("sibling")
	if bytes.Equal(identity, read(t, f.state("sibling", "identity"))) || bytes.Equal(known, read(t, f.state("sibling", "known_hosts"))) {
		t.Fatal("shared SSH identities")
	}
	first, _ := os.Stat(f.state("nixos", "rootfs.ext4"))
	second, _ := os.Stat(f.state("sibling", "rootfs.ext4"))
	if os.SameFile(first, second) {
		t.Fatal("shared writable disk")
	}
	f.must(`set -eu
 test ! -e nixos-commit
 test ! -e nixos-uncommitted
 test "$(psql -XAt -d hk -c "select to_regclass('public.experiment') is null")" = t
 test ! -r /run/provision/seed/host_key
 test ! -r /run/habikat-ssh/host_key
 `, "attach", "sibling")
	if out := f.must("", "list"); !strings.Contains(out, "nixos") || !strings.Contains(out, "sibling") {
		t.Fatal(out)
	}
	if out, err := f.run("", "delete", "nixos"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("delete bypassed confirmation: %v %s", err, out)
	}
	f.must("", "delete", "--force", "nixos")
	f.must("test ! -e nixos-uncommitted\n", "attach", "sibling")
	if _, err := os.Stat(filepath.Join(nixosImageDirectory(t), "system.erofs")); err != nil {
		t.Fatal("delete removed shared system image", err)
	}
}

func nixosState(t *testing.T, h *harness, name string) map[string]any {
	t.Helper()
	var state map[string]any
	if err := json.Unmarshal(read(t, h.state(name, "state.json")), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func assertNixosCleanShutdown(t *testing.T, h *harness, name string, count int) {
	t.Helper()
	console := string(read(t, h.state(name, "console.log")))
	if strings.Count(console, "database system is shut down") < count {
		t.Fatalf("missing PostgreSQL shutdown evidence in console (wanted %d)", count)
	}
	if !strings.Contains(console, "Stopped PostgreSQL Server") {
		t.Fatal("PostgreSQL service stop not observed")
	}
	disk := h.state(name, "rootfs.ext4")
	for _, args := range [][]string{{"e2fsck", "-fn", disk}, {"dumpe2fs", "-h", disk}} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		if args[0] == "dumpe2fs" && !strings.Contains(string(out), "Filesystem state:         clean") {
			t.Fatalf("unclean stopped filesystem: %s", out)
		}
	}
}

func TestNixosRestartRefusesChangedArtifactsAndHeldDisks(t *testing.T) {
	h := newHarness(t)
	image := filepath.Join(h.dir, "private-image")
	if err := os.Mkdir(image, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"firecracker", "vmlinux", "initrd", "system.erofs", "boot.json", "manifest.json"} {
		source := filepath.Join(nixosImageDirectory(t), name)
		out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", "--", source, filepath.Join(image, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("copy fixture: %v %s", err, out)
		}
	}
	h.env = append(h.env, "HK_IMAGE_DIR="+image)
	h.must("", "create", "--name", "retained")
	h.must("printf 'retained data\\n' > ~/preserve\n", "attach", "retained")
	h.must("", "stop", "retained")
	state := read(t, h.state("retained", "state.json"))
	key := read(t, h.state("retained", "identity"))
	for _, name := range []string{"initrd", "system.erofs", "boot.json"} {
		path := filepath.Join(image, name)
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		for _, corrupt := range []bool{false, true} {
			if corrupt {
				write(t, path, []byte("corrupt immutable artifact"))
			}
			if out, err := h.run("", "start", "retained"); err == nil || !strings.Contains(out, "preserved") {
				t.Fatalf("bad artifact accepted: %v %s", err, out)
			}
			if !bytes.Equal(state, read(t, h.state("retained", "state.json"))) || !bytes.Equal(key, read(t, h.state("retained", "identity"))) {
				t.Fatal("failed validation changed instance")
			}
			if corrupt {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.Rename(path+".saved", path); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rootfs.ext4", "seed.ext4"} {
		disk, err := os.Open(h.state("retained", name))
		if err != nil {
			t.Fatal(err)
		}
		out, startErr := h.run("", "start", "retained")
		disk.Close()
		if startErr == nil || !strings.Contains(out, "second writer") {
			t.Fatalf("held private disk accepted: %v %s", startErr, out)
		}
	}
	h.must("", "start", "retained")
	h.must("test \"$(cat ~/preserve)\" = 'retained data'\n", "attach", "retained")
}

func nixosImageDirectory(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("HK_IMAGE_DIR"); path != "" {
		return path
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(cache, "habikat", "nixos-image")
}
