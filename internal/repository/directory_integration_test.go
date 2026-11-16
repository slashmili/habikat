//go:build integration

package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceDirectory(t *testing.T) {
	repo := fixture(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	other := fixture(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	gitTest(t, repo, "worktree", "add", "-b", "second", worktree)
	outside := t.TempDir()
	child := filepath.Join(outside, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(repo, "dirty-untracked"), "dirty")
	index, err := os.ReadFile(filepath.Join(repo, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	for _, tc := range []struct{ name, dir, want string }{
		{name: "root", dir: repo, want: repo},
		{name: "subdirectory", dir: sub, want: repo},
		{name: "symlink", dir: filepath.Join(alias, "sub"), want: repo},
		{name: "distinct repository", dir: other, want: other},
		{name: "distinct worktree", dir: worktree, want: worktree},
		{name: "outside Git", dir: outside, want: outside},
		{name: "outside child", dir: child, want: child},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SourceDirectory(t.Context(), tc.dir)
			if err != nil || got != tc.want {
				t.Fatalf("SourceDirectory()=%q, %v; want %q", got, err, tc.want)
			}
		})
	}
	nextIndex, err := os.ReadFile(filepath.Join(repo, ".git/index"))
	if err != nil || !bytes.Equal(index, nextIndex) {
		t.Fatalf("directory discovery changed index: %v", err)
	}
}

func TestSourceDirectoryRejectsBrokenRepositoryAndCancellation(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, ".git"), "gitdir: /missing-habikato-fixture\n")
	if _, err := SourceDirectory(t.Context(), dir); err == nil {
		t.Fatal("broken Git marker treated as an empty directory")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := SourceDirectory(ctx, fixture(t)); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
}

func TestSourceDirectoryBoundsGitDiscovery(t *testing.T) {
	bin := t.TempDir()
	command := filepath.Join(bin, "git")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexec /usr/bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	start := time.Now()
	_, err := SourceDirectory(t.Context(), t.TempDir())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 7*time.Second {
		t.Fatalf("Git discovery was not bounded: elapsed=%v error=%v", time.Since(start), err)
	}
}
