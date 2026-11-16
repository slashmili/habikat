//go:build integration

package vm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slashmili/habikato/internal/git"
)

type exportFixture struct {
	home, repo, base string
	env              []string
}

func newExportFixture(t *testing.T) exportFixture {
	t.Helper()
	home := t.TempDir()
	fixture := exportFixture{home: home, repo: filepath.Join(home, "workspace")}
	fixture.env = append(git.HostEnvironment(), "HOME="+home, "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	fixture.must(t, "git init --initial-branch=main workspace; cd workspace; printf base > file; printf ignored > .gitignore; git add .; git commit -m base")
	fixture.base = strings.TrimSpace(fixture.must(t, "cd workspace; git rev-parse HEAD"))
	return fixture
}

func (fixture exportFixture) command(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", script)
	cmd.Dir, cmd.Env = fixture.home, fixture.env
	return cmd.CombinedOutput()
}

func (fixture exportFixture) must(t *testing.T, script string) string {
	t.Helper()
	out, err := fixture.command(t, script)
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	return string(out)
}

func TestFetchGuestStatus(t *testing.T) {
	for _, tc := range []struct {
		name, edit string
		want       GuestChanges
		fail       bool
	}{
		{name: "clean"},
		{name: "ignored and outside", edit: "touch workspace/ignored outside"},
		{name: "staged", edit: "cd workspace; printf staged > file; git add file", want: GuestChanges{Staged: true}},
		{name: "unstaged", edit: "printf changed > workspace/file", want: GuestChanges{Unstaged: true}},
		{name: "untracked", edit: "touch workspace/new", want: GuestChanges{Untracked: true}},
		{name: "rename", edit: "cd workspace; git mv file renamed", want: GuestChanges{Staged: true}},
		{name: "new branch commit", edit: "cd workspace; git switch -c feature; git commit --allow-empty -m branch"},
		{name: "reachable detached", edit: "cd workspace; git checkout --detach"},
		{name: "unreachable detached", edit: "cd workspace; git checkout --detach; git commit --allow-empty -m detached", fail: true},
		{name: "skip worktree", edit: "cd workspace; git update-index --skip-worktree file; printf hidden > file", fail: true},
		{name: "assume unchanged", edit: "cd workspace; git update-index --assume-unchanged file; printf hidden > file", fail: true},
		{name: "missing workspace", edit: "mv workspace elsewhere", fail: true},
		{name: "broken repository", edit: "mv workspace/.git workspace/.broken", fail: true},
		{name: "filters", edit: "cd workspace; git config filter.fixture.clean 'touch ../executed'; printf '* filter=fixture' > .git/info/attributes", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newExportFixture(t)
			if tc.edit != "" {
				fixture.must(t, tc.edit)
			}
			out, err := fixture.command(t, fetchStatusCommand(fixture.base))
			if tc.fail {
				if err == nil {
					t.Fatalf("unsafe guest accepted: %s", out)
				}
			} else {
				if err != nil {
					t.Fatalf("inspect guest: %v %s", err, out)
				}
				if !bytes.HasPrefix(out, []byte("HKSTATUS1\n")) {
					t.Fatalf("invalid status envelope: %q", out)
				}
				got, err := parseFetchStatus(bytes.TrimPrefix(out, []byte("HKSTATUS1\n")))
				if err != nil || got != tc.want {
					t.Fatalf("status=%+v %v; want %+v", got, err, tc.want)
				}
			}
			if _, err := os.Stat(filepath.Join(fixture.home, "executed")); !os.IsNotExist(err) {
				t.Fatal("guest inspection ran a filter")
			}
		})
	}
}

func TestGuestExportContainsOnlyCommittedBranches(t *testing.T) {
	fixture := newExportFixture(t)
	fixture.must(t, "cd workspace; git branch "+shellQuote("feature/with-quote'")+"; git commit --allow-empty -m main-progress; git tag secret-tag; printf secret > file; git stash; printf staged > staged; git add staged; printf unstaged > file; touch untracked ignored")
	index, err := os.ReadFile(filepath.Join(fixture.repo, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	before := fixture.must(t, "cd workspace; git status --porcelain=v1 --ignored; git for-each-ref --format='%(objectname) %(refname)'")
	bundle := filepath.Join(t.TempDir(), "project.bundle")
	file, err := os.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", strings.ReplaceAll(exportCommand(fixture.base), "/bin/busybox ", ""))
	cmd.Dir, cmd.Env, cmd.Stdout = fixture.home, fixture.env, file
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		file.Close()
		t.Fatalf("export: %v %s", err, &diagnostic)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	g := git.Local{Dir: fixture.repo}
	heads, err := g.Run(t.Context(), "bundle", "list-heads", bundle)
	if err != nil || strings.Contains(string(heads), "secret-tag") || strings.Contains(string(heads), "stash") || !strings.Contains(string(heads), "refs/heads/main") || !strings.Contains(string(heads), "refs/heads/feature/with-quote'") {
		t.Fatalf("heads=%s %v", heads, err)
	}
	if _, err := g.Run(t.Context(), "bundle", "verify", bundle); err != nil {
		t.Fatal(err)
	}
	after := fixture.must(t, "cd workspace; git status --porcelain=v1 --ignored; git for-each-ref --format='%(objectname) %(refname)'")
	nextIndex, err := os.ReadFile(filepath.Join(fixture.repo, ".git/index"))
	if err != nil || !bytes.Equal(index, nextIndex) || before != after {
		t.Fatal("export changed guest refs, index, or files")
	}
}

func TestGuestExportRejectsSourceMovement(t *testing.T) {
	fixture := newExportFixture(t)
	g := git.ExportRemote()
	createBundle := g.Command("bundle", "create")
	move := g.WithConfig("user.name=Fixture").WithConfig("user.email=fixture@example.invalid").Command("commit", "--allow-empty", "-m", "concurrent commit") + " >/dev/null\n"
	script := strings.Replace(exportScript(fixture.base), createBundle, move+createBundle, 1)
	script = strings.ReplaceAll(script, "/bin/busybox ", "")
	out, err := fixture.command(t, cleanGuestCommand(script))
	if err == nil || !strings.Contains(string(out), "branches changed during export") {
		t.Fatalf("source movement accepted or unclear: %v %s", err, out)
	}
}
