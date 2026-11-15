//go:build e2e

package e2e

import (
	"bytes"
	"context"
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

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}
func projectFixture(t *testing.T, h *harness) (string, string, string) {
	t.Helper()
	dir := filepath.Join(h.dir, "project '$%;()")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	branch := "topic/'$%;()"
	fixtureGit(t, dir, "init", "--initial-branch="+branch)
	write(t, filepath.Join(dir, "file with spaces"), []byte("first\n"))
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-m", "first")
	remote := filepath.Join(h.dir, "fixture-remote.git")
	fixtureGit(t, dir, "init", "--bare", remote)
	fixtureGit(t, dir, "remote", "add", "origin", remote)
	fixtureGit(t, dir, "push", "origin", "HEAD")
	fixtureGit(t, dir, "branch", "excluded-branch")
	fixtureGit(t, dir, "tag", "excluded-tag")
	write(t, filepath.Join(dir, ".gitignore"), []byte("ignored-secret\nignored-guest\n"))
	write(t, filepath.Join(dir, "ignored-secret"), []byte("fixture secret, never personal data\n"))
	write(t, filepath.Join(dir, "file with spaces"), []byte("unpushed content\n"))
	write(t, filepath.Join(dir, "executable"), []byte("#!/bin/sh\nprintf 'fixture script\\n'\n"))
	if err := os.Chmod(filepath.Join(dir, "executable"), 0755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-m", "unpushed")
	fixtureGit(t, dir, "config", "remote.origin.url", "https://fixture:password@example.invalid/private")
	fixtureGit(t, dir, "config", "user.name", "Must Not Import")
	write(t, filepath.Join(dir, ".git/hooks/post-checkout"), []byte("#!/bin/sh\ntouch /home/hk/hook-was-run\n"))
	if err := os.Chmod(filepath.Join(dir, ".git/hooks/post-checkout"), 0755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub directory")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	h.cwd = sub
	return dir, branch, fixtureGit(t, dir, "rev-parse", "HEAD")
}

func TestProjectVMLifecycleAndDeletion(t *testing.T) {
	h := newHarness(t)
	host, branch, commit := projectFixture(t, h)
	index := read(t, filepath.Join(host, ".git/index"))
	config := read(t, filepath.Join(host, ".git/config"))
	out := h.must("", "create", "--name", "project-one")
	if !strings.Contains(out, branch) || !strings.Contains(out, commit[:12]) {
		t.Fatalf("missing project success metadata: %s", out)
	}
	pid := vmPID(t, h.state("project-one", "state.json"))
	var state struct {
		Ready   bool
		Project struct{ Branch, Commit, Workspace string }
	}
	if err := json.Unmarshal(read(t, h.state("project-one", "state.json")), &state); err != nil || !state.Ready || state.Project.Commit != commit || state.Project.Branch != branch || state.Project.Workspace != "/home/hk/workspace" {
		t.Fatalf("state=%+v %v", state, err)
	}
	checks := `set -eu
test "$(id -u)" = 1000
test "$HOME" = /home/hk
test "$PWD" = /home/hk/workspace
git --version
test "$(git rev-parse HEAD)" = ` + quote(commit) + `
test "$(git symbolic-ref --short HEAD)" = ` + quote(branch) + `
test "$(git rev-list --count HEAD)" = 2
test -z "$(git status --porcelain)"
test "$(cat 'file with spaces')" = 'unpushed content'
test -x executable
test ! -e ignored-secret
test ! -e /home/hk/hook-was-run
test ! -e /home/hk/.hk-import
test ! -e /home/hk/.hk-workspace
test ! -e .git/objects/info/alternates
test ! -e .git/hooks/post-checkout
test -z "$(git remote)"
test -z "$(git config --get user.name || true)"
test "$(git for-each-ref --format='%(refname)')" = ` + quote("refs/heads/"+branch) + `
git fsck --full
printf 'PROJECT_OK\n'
`
	if out := h.must(checks, "attach", "project-one"); !strings.Contains(out, "PROJECT_OK") || !strings.Contains(out, "git version 2.52.1") {
		t.Fatalf("guest checks: %s", out)
	}
	testTerminal(t, h, "project-one", "/home/hk/workspace")
	if _, err := h.run("test \"$PWD\" = /home/hk/workspace || exit 1\nexit 37\n", "attach", "project-one"); exitCode(err) != 37 {
		t.Fatalf("piped exit code: %v", err)
	}
	h.must("", "create", "--name", "project-two")
	h.must(`set -eu
printf 'guest commit\n' > 'file with spaces'
git add 'file with spaces'
git -c user.name=Fixture -c user.email=fixture@example.invalid commit -m 'guest-only commit'
test -z "$(git status --porcelain)"
`, "attach", "project-one")
	assertUnforcedPipeDeletionRefused(t, h, "project-one", "yes\n")
	h.must("test \"$(cat 'file with spaces')\" = 'guest commit' || exit 1\n", "attach", "project-one")
	h.must(checks, "attach", "project-two")
	if fixtureGit(t, host, "rev-parse", "HEAD") != commit || !bytes.Equal(index, read(t, filepath.Join(host, ".git/index"))) || !bytes.Equal(config, read(t, filepath.Join(host, ".git/config"))) || fixtureGit(t, host, "status", "--porcelain") != "" {
		t.Fatal("guest changed host repository")
	}
	h.must(`set -eu
printf dirty >> 'file with spaces'
touch untracked-guest ignored-guest /home/hk/outside-workspace
`, "attach", "project-one")
	preserve := `set -eu
test -f untracked-guest
test -f ignored-guest
test -f /home/hk/outside-workspace
grep dirty 'file with spaces'
`
	for _, answer := range []string{"n\n", "\n", "\x04", "\x03"} {
		transcript, err := deletePTY(t, h, "project-one", answer)
		if err != nil || strings.Contains(transcript, "Deleted project-one.") {
			t.Fatalf("cancelled delete: %v %s", err, transcript)
		}
		h.must(preserve, "attach", "project-one")
	}
	assertUnforcedPipeDeletionRefused(t, h, "project-one", "y\n")
	if transcript, err := deletePTY(t, h, "project-one", "YeS\n"); err != nil || !strings.Contains(transcript, "Deleted project-one.") {
		t.Fatalf("confirmed delete: %v %s", err, transcript)
	}
	assertGone(t, pid)
	if _, err := os.Stat(h.state("project-one", "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted state retained: %v", err)
	}
	assertMissingWorkspaceCannotAttach(t, h, "project-two")
	h.must("", "delete", "project-two", "--force")
	h.must("", "delete", "project-one")
	h.cwd = h.dir
	testCleanEmptyVMDeletion(t, h)
	testStoppedVMRequiresForce(t, h)
}

func assertUnforcedPipeDeletionRefused(t *testing.T, h *harness, name, input string) {
	t.Helper()
	if out, err := h.run(input, "delete", name); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("unforced pipe deletion: %v %s", err, out)
	}
}

func assertMissingWorkspaceCannotAttach(t *testing.T, h *harness, name string) {
	t.Helper()
	h.must("cd /home/hk && rm -rf workspace\n", "attach", name)
	if out, err := h.run("echo UNEXPECTED\n", "attach", name); err == nil || !strings.Contains(out, "workspace") || strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("missing workspace: %v %s", err, out)
	}
}

func testCleanEmptyVMDeletion(t *testing.T, h *harness) {
	t.Helper()
	h.must("", "create", "--name", "empty")
	h.must("test \"$PWD\" = /home/hk || exit 1\n", "attach", "empty")
	if out := h.must("", "delete", "empty"); out != "Deleted empty.\n" {
		t.Fatalf("clean empty VM prompted: %s", out)
	}
}

func testStoppedVMRequiresForce(t *testing.T, h *harness) {
	t.Helper()
	h.must("", "create", "--name", "empty")
	emptyPID := vmPID(t, h.state("empty", "state.json"))
	if err := syscall.Kill(emptyPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if out, err := h.run("", "delete", "empty"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("stopped deletion: %v %s", err, out)
	}
	h.must("", "delete", "--force", "empty")
}

func TestDeleteOnlyPromptsForChanges(t *testing.T) {
	h := newHarness(t)
	projectFixture(t, h)
	h.must("", "create", "--name", "project")
	h.must("git status --short\n", "attach", "project")
	testTerminal(t, h, "project", "/home/hk/workspace")
	for _, tc := range []struct{ name, edit, undo string }{
		{name: "tracked", edit: "echo changed >> 'file with spaces'", undo: "git checkout -- 'file with spaces'"},
		{name: "untracked", edit: "touch untracked", undo: "rm untracked"},
		{name: "ignored", edit: "touch ignored-guest", undo: "rm ignored-guest"},
		{name: "outside workspace", edit: "touch /home/hk/notes", undo: "rm /home/hk/notes"},
		{name: "assume unchanged", edit: "git update-index --assume-unchanged 'file with spaces'; echo changed >> 'file with spaces'", undo: "git update-index --no-assume-unchanged 'file with spaces'; git checkout -- 'file with spaces'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.must("set -eu\n"+tc.edit+"\n", "attach", "project")
			if out, err := h.run("yes\n", "delete", "project"); err == nil || !strings.Contains(out, "has file changes or new commits") {
				t.Fatalf("changed VM was not protected: %v %s", err, out)
			}
			h.must("set -eu\n"+tc.undo+"\n", "attach", "project")
		})
	}
	if out := h.must("", "delete", "project"); out != "Deleted project.\n" {
		t.Fatalf("clean project VM prompted: %s", out)
	}
	h.cwd = h.dir
	h.must("", "create", "--name", "empty")
	h.must("touch notes\n", "attach", "empty")
	if transcript, err := deletePTY(t, h, "empty", "\n"); err != nil || !strings.Contains(transcript, "has file changes or new commits") || strings.Contains(transcript, "Deleted empty.") {
		t.Fatalf("changed empty VM confirmation: %v %s", err, transcript)
	}
	h.must("rm notes\n", "attach", "empty")
	if out := h.must("", "delete", "empty"); out != "Deleted empty.\n" {
		t.Fatalf("empty VM prompted after reverting changes: %s", out)
	}
}

func TestInvalidProjectBeforeAllocation(t *testing.T) {
	h := newHarness(t)
	host, _, _ := projectFixture(t, h)
	for _, kind := range []string{"dirty", "detached", "partial"} {
		switch kind {
		case "dirty":
			write(t, filepath.Join(host, "untracked"), []byte("change"))
		case "detached":
			fixtureGit(t, host, "checkout", "--detach")
		case "partial":
			fixtureGit(t, host, "config", "remote.origin.promisor", "true")
		}
		if out, err := h.run("", "create", "--name", "invalid"); err == nil {
			t.Fatalf("%s create succeeded: %s", kind, out)
		}
		if _, err := os.Stat(h.state("invalid", "")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s allocated a VM: %v", kind, err)
		}
		switch kind {
		case "dirty":
			if err := os.Remove(filepath.Join(host, "untracked")); err != nil {
				t.Fatal(err)
			}
		case "detached":
			fixtureGit(t, host, "checkout", "-")
		}
	}
}

func TestProvisioningFailureAndCancellation(t *testing.T) {
	h := newHarness(t)
	projectFixture(t, h)
	h.must("", "create", "--name", "survivor")
	survivor := vmPID(t, h.state("survivor", "state.json"))
	for _, kind := range []string{"failure", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			marker := filepath.Join(h.dir, "boundary-"+kind)
			tmp := filepath.Join(h.dir, "bundles-"+kind)
			if err := os.Mkdir(tmp, 0700); err != nil {
				t.Fatal(err)
			}
			cmd := h.command("create", "--name", "doomed")
			cmd.Env = append(cmd.Env, "HK_E2E_TRANSFER_BOUNDARY="+marker, "TMPDIR="+tmp)
			if kind == "cancellation" {
				cmd.Env = append(cmd.Env, "HK_E2E_TRANSFER_CANCEL=1")
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			pid := 0
			deadline := time.NewTimer(90 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			running := true
			for running {
				select {
				case err := <-done:
					if kind == "cancellation" || err == nil {
						t.Fatalf("create exited before boundary: %v %s", err, output.String())
					}
					running = false
				case <-tick.C:
					if data, err := os.ReadFile(h.state("doomed", "state.json")); err == nil {
						var state struct {
							PID   int
							Ready bool
						}
						if json.Unmarshal(data, &state) == nil {
							if state.Ready {
								t.Fatal("published ready before import completed")
							}
							if state.PID > 0 {
								pid = state.PID
							}
						}
					}
					if _, err := os.Stat(marker); err == nil && kind == "cancellation" {
						if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
						select {
						case err := <-done:
							if err == nil {
								t.Fatal("cancelled create succeeded")
							}
						case <-deadline.C:
							t.Fatal("cancelled transfer did not stop")
						}
						running = false
					}
				case <-deadline.C:
					t.Fatal("provisioning boundary deadline")
				}
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("never reached authenticated receiver: %v %s", err, output.String())
			}
			if strings.Contains(output.String(), "Created ") {
				t.Fatalf("failed import reported ready: %s", output.String())
			}
			if pid == 0 {
				t.Fatal("did not observe booted process")
			}
			assertGone(t, pid)
			if _, err := os.Stat(h.state("doomed", "")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state/disk/keys leaked: %v", err)
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("host bundle leaked: %v %v", entries, err)
			}
			if vmPID(t, h.state("survivor", "state.json")) != survivor {
				t.Fatal("unrelated VM changed")
			}
			h.must("test \"$PWD\" = /home/hk/workspace || exit 1\n", "attach", "survivor")
		})
	}
}

func deletePTY(t *testing.T, h *harness, name, answer string) (string, error) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "delete-pty")
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
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, "delete", name)
	cmd.Env = h.env
	cmd.Dir = h.cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	chunks := make(chan string, 32)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				select {
				case chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var transcript strings.Builder
	sent := false
	for chunk := range chunks {
		transcript.WriteString(chunk)
		if !sent && strings.Contains(transcript.String(), "[y/N]") {
			if _, err := master.Write([]byte(answer)); err != nil {
				t.Fatal(err)
			}
			sent = true
		}
	}
	err = cmd.Wait()
	if !sent {
		t.Fatalf("no deletion prompt: %s", transcript.String())
	}
	return transcript.String(), err
}
