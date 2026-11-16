//go:build integration

package repository

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchNeverExecutesHostGitConfiguration(t *testing.T) {
	f := newFetchFixture(t)
	temp := t.TempDir()
	marker := filepath.Join(temp, "executed")
	script := filepath.Join(temp, "sentinel")
	put(t, script, "#!/bin/sh\nprintf executed >> '"+marker+"'\nexit 71\n")
	hooks := filepath.Join(temp, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"reference-transaction", "post-checkout", "post-merge", "post-rewrite", "post-index-change", "pre-auto-gc"} {
		put(t, filepath.Join(hooks, hook), "#!/bin/sh\nexec '"+script+"'\n")
	}
	for key, value := range map[string]string{
		"core.hooksPath":                   hooks,
		"core.fsmonitor":                   script,
		"filter.fixture.clean":             script,
		"filter.fixture.smudge":            script,
		"filter.fixture.process":           script,
		"filter.fixture.required":          "true",
		"credential.helper":                "!" + script,
		"core.askPass":                     script,
		"core.sshCommand":                  script,
		"gpg.program":                      script,
		"commit.gpgSign":                   "true",
		"tag.gpgSign":                      "true",
		"maintenance.auto":                 "true",
		"gc.auto":                          "1",
		"fetch.writeCommitGraph":           "true",
		"remote.origin.url":                "ext::" + script,
		"remote.origin.promisor":           "true",
		"remote.origin.partialclonefilter": "blob:none",
		"user.name":                        ";",
		"user.email":                       "invalid<>",
	} {
		gitTest(t, f.host, "config", key, value)
	}
	if err := os.MkdirAll(filepath.Join(f.host, ".git/info"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.host, ".git/info/attributes"), "* filter=fixture\n")
	config, err := os.ReadFile(filepath.Join(f.host, ".git/config"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(f.host, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	importFixtureBundle(t, f)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host executable Git configuration ran: %v", err)
	}
	afterConfig, err := os.ReadFile(filepath.Join(f.host, ".git/config"))
	if err != nil || !bytes.Equal(config, afterConfig) {
		t.Fatalf("host config changed: %v", err)
	}
	afterIndex, err := os.ReadFile(filepath.Join(f.host, ".git/index"))
	if err != nil || !bytes.Equal(index, afterIndex) {
		t.Fatalf("host index changed: %v", err)
	}
}

func TestFetchSHA256Bundle(t *testing.T) {
	source := t.TempDir()
	gitTest(t, source, "init", "--object-format=sha256", "--initial-branch=main")
	gitTest(t, source, "commit", "--allow-empty", "-m", "base")
	base := gitTest(t, source, "rev-parse", "HEAD")
	host := filepath.Join(t.TempDir(), "host")
	gitTest(t, source, "clone", "--no-local", "--no-hardlinks", source, host)
	put(t, filepath.Join(source, "file"), "sha256 content\n")
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "-m", "guest")
	tip := gitTest(t, source, "rev-parse", "HEAD")
	bundle := filepath.Join(t.TempDir(), "sha256.bundle")
	gitTest(t, source, "bundle", "create", bundle, "--branches")
	f := fetchFixtureData{source: source, host: host, base: base, bundle: bundle}
	branches := importFixtureBundle(t, f)
	if len(branches) != 1 || branches[0].Commit != tip || len(tip) != 64 {
		t.Fatalf("incorrect SHA-256 import: %+v", branches)
	}
	if got := gitTest(t, host, "show", fetchPrefix()+"main:file"); got != "sha256 content" {
		t.Fatalf("wrong SHA-256 contents: %q", got)
	}
	sha1 := newFetchFixture(t)
	destination, err := OpenFetchDestination(t.Context(), sha1.host, sha1.base)
	if err != nil {
		t.Fatal(err)
	}
	if prepared, err := destination.Prepare(t.Context(), bundle, fetchInstance); err == nil || prepared != nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("mixed object formats accepted: %v %v", prepared, err)
	}
}

func TestFetchLinkedWorktreeDestination(t *testing.T) {
	f := newFetchFixture(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	gitTest(t, f.host, "worktree", "add", "-b", "linked", worktree)
	put(t, filepath.Join(worktree, "tracked file"), "linked dirty contents\n")
	indexPath := gitTest(t, worktree, "rev-parse", "--git-path", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	f.host = worktree
	importFixtureBundle(t, f)
	if got := gitTest(t, worktree, "rev-parse", "HEAD"); got != f.base {
		t.Fatal("linked checkout HEAD changed")
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("linked index changed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "tracked file"))
	if err != nil || string(content) != "linked dirty contents\n" {
		t.Fatalf("linked worktree changed: %q %v", content, err)
	}
}
