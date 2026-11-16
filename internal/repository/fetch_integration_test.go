//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slashmili/habikato/internal/git"
)

type fetchFixtureData struct {
	source, host, base, bundle string
	commits                    map[string]string
	payload                    []byte
}

func newFetchFixture(t *testing.T) fetchFixtureData {
	t.Helper()
	source := fixture(t)
	gitTest(t, source, "branch", "-m", "main")
	base := gitTest(t, source, "rev-parse", "HEAD")
	host := filepath.Join(t.TempDir(), "host")
	gitTest(t, source, "clone", "--no-local", "--no-hardlinks", "--template=", source, host)
	payload := make([]byte, 160<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] = 'x'
	if err := os.WriteFile(filepath.Join(source, "payload"), payload, 0755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(source, "tracked file"), "guest main\n")
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "-m", "guest main")
	main := gitTest(t, source, "rev-parse", "HEAD")
	gitTest(t, source, "checkout", "-b", "feature/login", base)
	put(t, filepath.Join(source, "feature"), "feature branch\n")
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "-m", "guest feature")
	feature := gitTest(t, source, "rev-parse", "HEAD")
	gitTest(t, source, "checkout", "main")
	gitTest(t, source, "tag", "not-exported")
	bundle := filepath.Join(t.TempDir(), "guest.bundle")
	gitTest(t, source, "bundle", "create", bundle, "--branches")
	return fetchFixtureData{source: source, host: host, base: base, bundle: bundle, commits: map[string]string{"main": main, "feature/login": feature}, payload: payload}
}

func TestFetchImportsBranchesWithoutChangingCheckout(t *testing.T) {
	f := newFetchFixture(t)
	gitTest(t, f.host, "branch", "keep-local")
	gitTest(t, f.host, "tag", "keep-tag")
	gitTest(t, f.host, "update-ref", "refs/remotes/hk/another-instance/keep", f.base)
	put(t, filepath.Join(f.host, ".git/FETCH_HEAD"), "keep fetch head\n")
	if err := os.MkdirAll(filepath.Join(f.host, ".git/info"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.host, ".git/info/exclude"), "ignored-marker\n")
	put(t, filepath.Join(f.host, "tracked file"), "staged host\n")
	gitTest(t, f.host, "add", "tracked file")
	put(t, filepath.Join(f.host, "tracked file"), "unstaged host\n")
	put(t, filepath.Join(f.host, "untracked-marker"), "keep untracked\n")
	put(t, filepath.Join(f.host, "ignored-marker"), "keep ignored\n")
	before := fetchProtectedState(t, f.host)
	sub := filepath.Join(f.host, "subdir")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	destination, err := OpenFetchDestination(t.Context(), sub, f.base)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := destination.Prepare(t.Context(), f.bundle, fetchInstance)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if refs := gitTest(t, f.host, "for-each-ref", "--format=%(refname)", fetchPrefix()); refs != "" {
		t.Fatalf("preparation published refs: %s", refs)
	}
	if err := prepared.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(prepared.Branches()) != len(f.commits) {
		t.Fatalf("reported branches = %+v", prepared.Branches())
	}
	for _, branch := range prepared.Branches() {
		want := f.commits[branch.Name]
		if branch.Commit != want || branch.Previous != "" || branch.Rewritten || branch.Ref != fetchPrefix()+branch.Name {
			t.Fatalf("incorrect imported snapshot: %+v", branch)
		}
		if got := gitTest(t, f.host, "rev-parse", branch.Ref); got != want {
			t.Fatalf("%s = %s, want %s", branch.Ref, got, want)
		}
	}
	if got := gitTest(t, f.host, "rev-list", "--count", fetchPrefix()+"main"); got != "2" {
		t.Fatalf("reachable history = %s", got)
	}
	if got := gitTest(t, f.host, "ls-tree", fetchPrefix()+"main", "payload"); !strings.HasPrefix(got, "100755 blob ") {
		t.Fatalf("executable mode missing: %s", got)
	}
	data, err := (git.Local{Dir: f.host}).Run(t.Context(), "show", fetchPrefix()+"main:payload")
	if err != nil || !bytes.Equal(data, f.payload) {
		t.Fatalf("large binary payload changed: %v", err)
	}
	if got := gitTest(t, f.host, "show", fetchPrefix()+"feature/login:feature"); got != "feature branch" {
		t.Fatalf("feature content = %q", got)
	}
	assertFetchProtectedState(t, f.host, before)
}

func fetchPrefix() string { return "refs/remotes/hk/" + fetchInstance + "/" }

func fetchProtectedState(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	state := map[string][]byte{}
	for _, path := range []string{".git/HEAD", ".git/index", ".git/config", ".git/FETCH_HEAD", ".git/info/exclude", "tracked file", "untracked-marker", "ignored-marker"} {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		state[path] = data
	}
	state["protected refs"] = []byte(gitTest(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/", "refs/remotes/origin/", "refs/remotes/hk/another-instance/"))
	state["status"] = []byte(gitTest(t, dir, "status", "--porcelain=v1", "-z", "--ignored"))
	return state
}

func assertFetchProtectedState(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	after := fetchProtectedState(t, dir)
	for key, value := range before {
		if !bytes.Equal(value, after[key]) {
			t.Fatalf("fetch changed %s", key)
		}
	}
}

func importFixtureBundle(t *testing.T, f fetchFixtureData) []FetchedBranch {
	t.Helper()
	destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := destination.Prepare(t.Context(), f.bundle, fetchInstance)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	return prepared.Branches()
}

func TestFetchRepeatedUpdatesRewritesAndNoPruning(t *testing.T) {
	f := newFetchFixture(t)
	importFixtureBundle(t, f)
	log := filepath.Join(f.host, ".git/logs", fetchPrefix()+"main")
	before, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range importFixtureBundle(t, f) {
		if branch.Commit != branch.Previous || branch.Rewritten {
			t.Fatalf("repeat was not a no-op: %+v", branch)
		}
	}
	after, err := os.ReadFile(log)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("repeat changed reflog: %v", err)
	}
	gitTest(t, f.source, "commit", "--allow-empty", "-m", "advance")
	advanced := gitTest(t, f.source, "rev-parse", "HEAD")
	gitTest(t, f.source, "bundle", "create", f.bundle, "--branches")
	for _, branch := range importFixtureBundle(t, f) {
		if branch.Name == "main" && (branch.Commit != advanced || branch.Previous != f.commits["main"] || branch.Rewritten) {
			t.Fatalf("incorrect fast-forward: %+v", branch)
		}
	}
	gitTest(t, f.source, "reset", "--hard", f.base)
	gitTest(t, f.source, "branch", "-D", "feature/login")
	gitTest(t, f.source, "bundle", "create", f.bundle, "--branches")
	branches := importFixtureBundle(t, f)
	if len(branches) != 1 || !branches[0].Rewritten || branches[0].Previous != advanced || branches[0].Commit != f.base {
		t.Fatalf("rewrite not reported: %+v", branches)
	}
	if got := gitTest(t, f.host, "rev-parse", fetchPrefix()+"feature/login"); got != f.commits["feature/login"] {
		t.Fatal("deleted guest branch was pruned")
	}
	if got := gitTest(t, f.host, "rev-parse", fetchPrefix()+"main@{1}"); got != advanced {
		t.Fatalf("old tip missing from reflog: %s", got)
	}
}

func TestFetchDestinationRejectsUnsuitableRepositories(t *testing.T) {
	f := newFetchFixture(t)
	for _, tc := range []struct{ name, dir, base string }{
		{"missing base", fixture(t), f.commits["main"]},
		{"invalid base", f.host, "HEAD"},
		{"outside repository", t.TempDir(), f.base},
		{"wrong object format", f.host, strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if destination, err := OpenFetchDestination(t.Context(), tc.dir, tc.base); err == nil || destination != nil {
				t.Fatalf("accepted unsuitable destination: %v %v", destination, err)
			}
		})
	}
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitTest(t, f.source, "clone", "--bare", f.source, bare)
	if destination, err := OpenFetchDestination(t.Context(), bare, f.base); err == nil || destination != nil {
		t.Fatalf("accepted bare destination: %v %v", destination, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := OpenFetchDestination(ctx, f.host, f.base); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestFetchExcludesTagStashAndReflogOnlyHistory(t *testing.T) {
	f := newFetchFixture(t)
	gitTest(t, f.source, "checkout", "--detach", "main")
	put(t, filepath.Join(f.source, "private"), "private tag and reflog content\n")
	gitTest(t, f.source, "add", ".")
	gitTest(t, f.source, "commit", "-m", "private history")
	private := gitTest(t, f.source, "rev-parse", "HEAD")
	gitTest(t, f.source, "tag", "private-history")
	gitTest(t, f.source, "checkout", "main")
	put(t, filepath.Join(f.source, "tracked file"), "stash-only content\n")
	put(t, filepath.Join(f.source, "untracked"), "untracked stash content\n")
	gitTest(t, f.source, "stash", "push", "-u", "-m", "private stash")
	stash := gitTest(t, f.source, "rev-parse", "refs/stash")
	gitTest(t, f.source, "gc", "--aggressive", "--prune=now")
	gitTest(t, f.source, "bundle", "create", f.bundle, "--branches")
	importFixtureBundle(t, f)
	for _, oid := range []string{private, stash} {
		if _, err := (git.Local{Dir: f.host}).Run(t.Context(), "cat-file", "-e", oid); err == nil {
			t.Fatalf("private history object %s was imported", oid)
		}
	}
	if refs := gitTest(t, f.host, "for-each-ref", "--format=%(refname)", "refs/tags/", "refs/stash"); refs != "" {
		t.Fatalf("private refs were imported: %s", refs)
	}
}
