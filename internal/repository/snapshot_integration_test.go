//go:build integration

package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}
func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project '$%;`x`")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "--initial-branch=topic/'quote%$;()")
	put(t, filepath.Join(dir, "tracked file"), "first\n")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "first")
	return dir
}

func TestSnapshotRestore(t *testing.T) {
	dir := fixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitTest(t, dir, "init", "--bare", remote)
	gitTest(t, dir, "remote", "add", "origin", remote)
	gitTest(t, dir, "push", "origin", "HEAD")
	gitTest(t, dir, "branch", "unrelated")
	gitTest(t, dir, "tag", "private-tag")
	put(t, filepath.Join(dir, "tracked file"), "stash-only content\n")
	gitTest(t, dir, "stash", "push", "-m", "private-stash")
	put(t, filepath.Join(dir, ".gitignore"), "ignored-marker\n")
	put(t, filepath.Join(dir, "ignored-marker"), "fixture-secret\n")
	put(t, filepath.Join(dir, "tracked file"), "unpushed\n")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "unpushed")
	gitTest(t, dir, "config", "remote.origin.url", "https://fixture:password@example.invalid/private")
	put(t, filepath.Join(dir, ".git/hooks/post-checkout"), "#!/bin/sh\nexit 99\n")
	sub := filepath.Join(dir, "sub directory")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	expected := gitTest(t, dir, "rev-parse", "HEAD")
	branch := gitTest(t, dir, "symbolic-ref", "--short", "HEAD")
	index, _ := os.ReadFile(filepath.Join(dir, ".git/index"))
	config, _ := os.ReadFile(filepath.Join(dir, ".git/config"))
	status := gitTest(t, dir, "status", "--porcelain")
	snapshot, err := Capture(t.Context(), sub)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if snapshot == nil || snapshot.Commit != expected || snapshot.Branch != branch {
		t.Fatalf("wrong snapshot: %+v", snapshot)
	}
	restored := filepath.Join(t.TempDir(), "restore")
	gitTest(t, dir, "clone", "--no-tags", "--branch="+branch, snapshot.Bundle, restored)
	gitTest(t, restored, "remote", "remove", "origin")
	if got := gitTest(t, restored, "rev-parse", "HEAD"); got != expected {
		t.Fatalf("commit=%s", got)
	}
	if got := gitTest(t, restored, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("history=%s", got)
	}
	if got := gitTest(t, restored, "for-each-ref", "--format=%(refname)"); got != "refs/heads/"+branch {
		t.Fatalf("extra refs: %q", got)
	}
	for _, path := range []string{"ignored-marker", ".git/objects/info/alternates", ".git/hooks/post-checkout"} {
		if _, err := os.Stat(filepath.Join(restored, path)); !os.IsNotExist(err) {
			t.Fatalf("copied %s: %v", path, err)
		}
	}
	if got := gitTest(t, restored, "status", "--porcelain"); got != "" {
		t.Fatalf("restored dirty: %q", got)
	}
	if got := gitTest(t, dir, "status", "--porcelain"); got != status {
		t.Fatal("host worktree changed")
	}
	nextIndex, _ := os.ReadFile(filepath.Join(dir, ".git/index"))
	nextConfig, _ := os.ReadFile(filepath.Join(dir, ".git/config"))
	if !bytes.Equal(index, nextIndex) || !bytes.Equal(config, nextConfig) || gitTest(t, dir, "rev-parse", "HEAD") != expected {
		t.Fatal("host branch, index, or config changed")
	}
	if data, err := os.ReadFile(filepath.Join(restored, "tracked file")); err != nil || string(data) != "unpushed\n" {
		t.Fatalf("content=%q %v", data, err)
	}
	assertRestoredRepositoryIndependent(t, snapshot, dir, restored)
}

func assertRestoredRepositoryIndependent(t *testing.T, snapshot *Snapshot, source, restored string) {
	t.Helper()
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	gitTest(t, restored, "fsck", "--full")
}

func TestRejectedRepositories(t *testing.T) {
	for _, kind := range []string{"modified", "deleted", "staged", "untracked", "assume-unchanged", "skip-worktree", "detached", "empty", "bare", "shallow", "partial", "promisor", "submodule", "lfs", "lfs-config", "lfs-pointer", "lfs-unmatched", "broken", "broken-directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixture(t)
			want := "clean"
			switch kind {
			case "modified":
				put(t, filepath.Join(dir, "tracked file"), "dirty")
			case "deleted":
				if err := os.Remove(filepath.Join(dir, "tracked file")); err != nil {
					t.Fatal(err)
				}
			case "staged":
				put(t, filepath.Join(dir, "tracked file"), "dirty")
				gitTest(t, dir, "add", ".")
			case "untracked":
				put(t, filepath.Join(dir, "extra"), "dirty")
			case "assume-unchanged", "skip-worktree":
				gitTest(t, dir, "update-index", "--"+kind, "tracked file")
				put(t, filepath.Join(dir, "tracked file"), "dirty")
			case "detached":
				gitTest(t, dir, "checkout", "--detach")
				want = "attached local branch"
			case "empty":
				dir = t.TempDir()
				gitTest(t, dir, "init")
				want = "existing commit"
			case "bare":
				dir = t.TempDir()
				gitTest(t, dir, "init", "--bare")
				want = "non-bare"
			case "shallow":
				dst := filepath.Join(t.TempDir(), "shallow")
				gitTest(t, dir, "clone", "--depth=1", "file://"+dir, dst)
				dir = dst
				want = "shallow"
			case "partial":
				gitTest(t, dir, "config", "extensions.partialClone", "origin")
				want = "partial"
			case "promisor":
				put(t, filepath.Join(dir, ".git/objects/pack/pack-test.promisor"), "")
				want = "partial"
			case "submodule":
				gitTest(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+gitTest(t, dir, "rev-parse", "HEAD")+",child")
				gitTest(t, dir, "commit", "-m", "submodule")
				want = "submodules"
			case "lfs":
				put(t, filepath.Join(dir, ".gitattributes"), "* filter=lfs\n")
				gitTest(t, dir, "add", ".")
				gitTest(t, dir, "commit", "-m", "lfs")
				want = "Git LFS"
			case "lfs-pointer":
				put(t, filepath.Join(dir, "pointer"), "version https://git-lfs.github.com/spec/v1\noid sha256:fixture\nsize 1\n")
				gitTest(t, dir, "add", ".")
				gitTest(t, dir, "commit", "-m", "pointer")
				want = "Git LFS"
			case "lfs-unmatched":
				put(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=lfs\n")
				gitTest(t, dir, "add", ".")
				gitTest(t, dir, "commit", "-m", "attributes")
				want = "Git LFS"
			case "broken-directory":
				dir = t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				want = "discover repository"
			case "lfs-config":
				gitTest(t, dir, "config", "filter.lfs.process", "must-not-run")
				want = "Git LFS"
			case "broken":
				dir = t.TempDir()
				put(t, filepath.Join(dir, ".git"), "gitdir: missing\n")
				want = "discover repository"
			}
			snapshot, err := Capture(t.Context(), dir)
			if err == nil || snapshot != nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("snapshot=%v error=%v; want %s", snapshot, err, want)
			}
		})
	}
}

func TestOutsideAndGitfile(t *testing.T) {
	if snapshot, err := Capture(t.Context(), t.TempDir()); snapshot != nil || err != nil {
		t.Fatalf("outside=%v %v", snapshot, err)
	}
	dir := fixture(t)
	work := filepath.Join(t.TempDir(), "separate git directory")
	gitTest(t, dir, "init", "--separate-git-dir", work)
	snapshot, err := Capture(t.Context(), dir)
	if err != nil || snapshot == nil {
		t.Fatalf("gitfile=%v %v", snapshot, err)
	}
	defer snapshot.Close()
}
func TestSourceMovementAndFailureCleanup(t *testing.T) {
	for _, kind := range []string{"branch", "dirty", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixture(t)
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			snapshot, err := capture(t.Context(), dir, func() {
				switch kind {
				case "branch":
					gitTest(t, dir, "commit", "--allow-empty", "-m", "move")
				case "dirty":
					put(t, filepath.Join(dir, "tracked file"), "changed")
				case "cancel":
					gitTest(t, dir, "checkout", "--detach")
				}
			})
			if snapshot != nil || err == nil {
				t.Fatalf("movement accepted: %v %v", snapshot, err)
			}
			entries, err := os.ReadDir(temp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary bundle leaked: %v %v", entries, err)
			}
		})
	}
}

func TestHostCommandsAreNotExecuted(t *testing.T) {
	dir := fixture(t)
	marker := filepath.Join(t.TempDir(), "executed")
	command := "touch '" + marker + "'"
	for _, key := range []string{"core.fsmonitor", "filter.test.clean", "filter.test.process", "gpg.program", "core.sshCommand"} {
		gitTest(t, dir, "config", key, command)
	}
	put(t, filepath.Join(dir, ".git/info/attributes"), "* filter=test\n")
	snapshot, err := Capture(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host command executed: %v", err)
	}
}

func TestCancelAfterBundleCapture(t *testing.T) {
	dir := fixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	snapshot, err := capture(ctx, dir, cancel)
	if snapshot != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v %v", snapshot, err)
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled bundle retained: %v %v", entries, err)
	}
}
