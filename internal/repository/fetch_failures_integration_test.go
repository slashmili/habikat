//go:build integration

package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchRejectsCorruptOrIncompleteBundles(t *testing.T) {
	f := newFetchFixture(t)
	original, err := os.ReadFile(f.bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"truncated", "corrupt pack", "invalid oid", "missing commit", "tree tip", "unreachable objects", "tag ref", "partial bundle", "conflicting branches", "trailing bytes"} {
		t.Run(kind, func(t *testing.T) {
			data := bytes.Clone(original)
			switch kind {
			case "truncated":
				data = data[:len(data)-32]
			case "corrupt pack":
				data[len(data)/2] ^= 255
			case "invalid oid":
				data = bytes.Replace(data, []byte(f.commits["main"]+" refs/heads/main"), []byte(strings.Repeat("z", 40)+" refs/heads/main"), 1)
			case "missing commit":
				data = bytes.Replace(data, []byte(f.commits["main"]+" refs/heads/main"), []byte(strings.Repeat("f", 40)+" refs/heads/main"), 1)
			case "tree tip":
				tree := gitTest(t, f.source, "rev-parse", "main^{tree}")
				data = bytes.Replace(data, []byte(f.commits["main"]+" refs/heads/main"), []byte(tree+" refs/heads/main"), 1)
			case "unreachable objects":
				data = bytes.Replace(data, []byte(f.commits["main"]+" refs/heads/main"), []byte(f.base+" refs/heads/main"), 1)
			case "tag ref":
				data = bytes.Replace(data, []byte("refs/heads/main"), []byte("refs/tags/main"), 1)
			case "partial bundle":
				path := filepath.Join(t.TempDir(), "partial.bundle")
				gitTest(t, f.source, "bundle", "create", path, "main", "^"+f.base)
				data, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			case "conflicting branches":
				data = bytes.Replace(data, []byte("refs/heads/feature/login"), []byte("refs/heads/main/child"), 1)
			case "trailing bytes":
				data = append(data, []byte("untrusted trailing data")...)
			}
			path := filepath.Join(t.TempDir(), "invalid.bundle")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
			if err != nil {
				t.Fatal(err)
			}
			before := fetchProtectedState(t, f.host)
			prepared, err := destination.Prepare(t.Context(), path, fetchInstance)
			if prepared != nil {
				prepared.Close()
			}
			if err == nil || prepared != nil {
				t.Fatalf("accepted %s: %v %v", kind, prepared, err)
			}
			if refs := gitTest(t, f.host, "for-each-ref", "--format=%(refname)", fetchPrefix()); refs != "" {
				t.Fatalf("failed validation published refs: %s", refs)
			}
			assertFetchProtectedState(t, f.host, before)
		})
	}
}

func TestFetchRejectsSymbolicDestinationsAndConflictsAtomically(t *testing.T) {
	for _, kind := range []string{"symbolic", "broken symbolic", "parent conflict", "child conflict"} {
		t.Run(kind, func(t *testing.T) {
			f := newFetchFixture(t)
			before := fetchProtectedState(t, f.host)
			switch kind {
			case "symbolic":
				gitTest(t, f.host, "symbolic-ref", fetchPrefix()+"main", "refs/heads/main")
			case "broken symbolic":
				gitTest(t, f.host, "symbolic-ref", fetchPrefix()+"main", "refs/heads/missing")
			case "parent conflict":
				gitTest(t, f.host, "update-ref", fetchPrefix()+"feature", f.base)
			case "child conflict":
				gitTest(t, f.host, "update-ref", fetchPrefix()+"main/child", f.base)
			}
			refs := gitTest(t, f.host, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", fetchPrefix())
			destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := destination.Prepare(t.Context(), f.bundle, fetchInstance)
			if err == nil {
				defer prepared.Close()
				err = prepared.Publish(t.Context())
			}
			if err == nil {
				t.Fatal("unsafe destination was accepted")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("ref validation failure was reported as user cancellation: %v", err)
			}
			if got := gitTest(t, f.host, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", fetchPrefix()); got != refs {
				t.Fatalf("failed publication changed refs:\n%s\nwant:\n%s", got, refs)
			}
			if kind == "broken symbolic" {
				if got := gitTest(t, f.host, "symbolic-ref", fetchPrefix()+"main"); got != "refs/heads/missing" {
					t.Fatal("broken symbolic ref was changed")
				}
			}
			assertFetchProtectedState(t, f.host, before)
		})
	}
}

func TestFetchExpectedRefRacePublishesNothing(t *testing.T) {
	f := newFetchFixture(t)
	importFixtureBundle(t, f)
	advanceFetchBranches(t, f)
	destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := destination.Prepare(t.Context(), f.bundle, fetchInstance)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	gitTest(t, f.host, "update-ref", fetchPrefix()+"main", f.base)
	before := gitTest(t, f.host, "for-each-ref", "--format=%(refname) %(objectname)")
	if err := prepared.Publish(t.Context()); err == nil {
		t.Fatal("concurrent target change was overwritten")
	}
	if got := gitTest(t, f.host, "for-each-ref", "--format=%(refname) %(objectname)"); got != before {
		t.Fatal("ref race caused a partial publication")
	}
}

func TestFetchSymbolicRaceWithMatchingOIDIsRejectedUnderLock(t *testing.T) {
	f := newFetchFixture(t)
	importFixtureBundle(t, f)
	advanceFetchBranches(t, f)
	gitTest(t, f.host, "branch", "protected", f.commits["main"])
	destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := destination.Prepare(t.Context(), f.bundle, fetchInstance)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	err = prepared.publish(t.Context(), func() {
		gitTest(t, f.host, "symbolic-ref", fetchPrefix()+"main", "refs/heads/protected")
	})
	if err == nil || !strings.Contains(err.Error(), "symbolic") {
		t.Fatalf("matching-OID symbolic race accepted: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("symbolic ref failure was reported as user cancellation: %v", err)
	}
	if got := gitTest(t, f.host, "symbolic-ref", fetchPrefix()+"main"); got != "refs/heads/protected" {
		t.Fatal("symbolic destination was converted")
	}
	if got := gitTest(t, f.host, "rev-parse", "refs/heads/protected"); got != f.commits["main"] {
		t.Fatal("local branch was changed through symbolic destination")
	}
	if got := gitTest(t, f.host, "rev-parse", fetchPrefix()+"feature/login"); got != f.commits["feature/login"] {
		t.Fatal("symbolic race partially published another branch")
	}
}

func advanceFetchBranches(t *testing.T, f fetchFixtureData) {
	t.Helper()
	for _, branch := range []string{"feature/login", "main"} {
		gitTest(t, f.source, "checkout", branch)
		gitTest(t, f.source, "commit", "--allow-empty", "-m", "advance "+branch)
	}
	gitTest(t, f.source, "bundle", "create", f.bundle, "--branches")
}

func TestFetchCancellationAndTemporaryCleanup(t *testing.T) {
	f := newFetchFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if prepared, err := destination.Prepare(ctx, f.bundle, fetchInstance); !errors.Is(err, context.Canceled) || prepared != nil {
		t.Fatalf("preparation cancellation = %v, %v", prepared, err)
	}
	assertFetchTempEmpty(t, temp)
	prepared, err := destination.Prepare(t.Context(), f.bundle, fetchInstance)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	err = prepared.publish(ctx, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("publication cancellation = %v", err)
	}
	if refs := gitTest(t, f.host, "for-each-ref", "--format=%(refname)", fetchPrefix()); refs != "" {
		t.Fatalf("cancelled transaction published refs: %s", refs)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	assertFetchTempEmpty(t, temp)
	if err := prepared.Publish(t.Context()); err == nil {
		t.Fatal("closed import was published")
	}
}

func assertFetchTempEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary data remains: %v %v", entries, err)
	}
}

func TestFetchPreparationSizeBoundAndCleanup(t *testing.T) {
	f := newFetchFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	destination, err := OpenFetchDestination(t.Context(), f.host, f.base)
	if err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(t.TempDir(), "oversized.bundle")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Truncate(maxFetchBundle+1), file.Close()); err != nil {
		t.Fatal(err)
	}
	if prepared, err := destination.Prepare(t.Context(), oversized, fetchInstance); err == nil || prepared != nil {
		t.Fatalf("oversized bundle accepted: %v %v", prepared, err)
	}
	assertFetchTempEmpty(t, temp)
}
