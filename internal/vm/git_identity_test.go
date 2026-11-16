package vm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slashmili/habikat/internal/git"
	"github.com/slashmili/habikat/internal/repository"
)

func TestGuestGitIdentityRoundTrip(t *testing.T) {
	for _, name := range []string{"Fixture User", "Zoë 李 ' $HOME `touch escaped`;", "--help", "trailing spaces  "} {
		t.Run(name, func(t *testing.T) {
			assertGuestGitIdentityRoundTrip(t, repository.GitIdentity{Name: name, Email: "test+tag'`$@example.invalid"})
		})
	}
}

func FuzzGuestGitIdentity(f *testing.F) {
	for _, name := range []string{"Fixture", "Zoë 李 $`'", "'; touch escaped; #", "--help", "", "bad\x00value", "bad\rvalue", "bad\nvalue"} {
		f.Add(name, "test+tag@example.invalid")
	}
	f.Fuzz(func(t *testing.T, name, email string) {
		if len(name)+len(email) > 8192 {
			t.Skip()
		}
		identity := repository.GitIdentity{Name: name, Email: email}
		if identity.Validate() != nil {
			if command, err := gitIdentityCommand(identity); err == nil || command != "" {
				t.Fatal("invalid identity produced usable command")
			}
			return
		}
		assertGuestGitIdentityRoundTrip(t, identity)
	})
}

func assertGuestGitIdentityRoundTrip(t *testing.T, identity repository.GitIdentity) {
	t.Helper()
	dir := t.TempDir()
	g := git.Local{Dir: dir}
	if _, err := g.Run(t.Context(), "init", "--template="); err != nil {
		t.Fatal(err)
	}
	command, err := gitIdentityCommand(identity)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(command, "cd /home/hk/workspace\n") != 1 {
		t.Fatal("identity target is not confined")
	}
	command = strings.Replace(command, "cd /home/hk/workspace\n", "cd "+shellQuote(dir)+"\n", 1)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
	cmd.Env = git.HostEnvironment()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure: %v %s", err, out)
	}
	for key, want := range map[string]string{"user.name": identity.Name, "user.email": identity.Email} {
		got, err := g.Run(t.Context(), "config", "--local", "--null", "--get", key)
		if err != nil || !bytes.Equal(got, append([]byte(want), 0)) {
			t.Fatalf("roundtrip %s=%q %v", key, got, err)
		}
	}
	keys, err := g.Run(t.Context(), "config", "--local", "--name-only", "--get-regexp", "^(user|credential|alias|include|filter|gpg|commit)\\.")
	if err != nil || string(keys) != "user.name\nuser.email\n" {
		t.Fatalf("extra settings: %q %v", keys, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("identity created files: %v %v", entries, err)
	}
}

func TestConfigureGitIdentityFailure(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "ssh")
	writeTestFile(t, path, []byte("#!/bin/sh\necho 'identity read-back failed' >&2\nexit 42\n"))
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	err := configureGitIdentity(t.Context(), dir, repository.GitIdentity{Name: "Fixture", Email: "fixture@example.invalid"})
	if err == nil || !strings.Contains(err.Error(), "identity read-back failed") {
		t.Fatalf("configuration failure lost: %v", err)
	}
}

func TestCreateGitIdentityFailsBeforeVMAllocation(t *testing.T) {
	t.Chdir(t.TempDir())
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("HK_STATE_DIR", state)
	if _, err := Create(t.Context(), "fixture", CreateOptions{WithGitConfig: true}); err == nil || !strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("allocated state before checking identity: %v", err)
	}
	if snapshot, err := captureProject(t.Context(), ".", CreateOptions{}); err != nil || snapshot != nil {
		t.Fatalf("ordinary empty create changed: %v %v", snapshot, err)
	}
}

func TestCreateGitIdentityMissingConfigAndDefaultIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	g := git.Local{Dir: dir}
	for _, args := range [][]string{
		{"init", "--template=", "--initial-branch=main"},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"},
	} {
		if _, err := g.Run(t.Context(), args...); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("HK_STATE_DIR", state)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if _, err := Create(t.Context(), "fixture", CreateOptions{WithGitConfig: true}); err == nil || !strings.Contains(err.Error(), "user.name") {
		t.Fatalf("missing identity: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("allocated VM state: %v", err)
	}
	global := filepath.Join(t.TempDir(), "broken-global")
	writeTestFile(t, global, []byte("[invalid configuration\n"))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	snapshot, err := captureProject(t.Context(), dir, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if snapshot.GitIdentity != nil {
		t.Fatal("default capture imported identity")
	}
}
