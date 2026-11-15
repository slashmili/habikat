//go:build integration

package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangesCommand(t *testing.T) {
	for _, tc := range []struct {
		name, edit     string
		project, dirty bool
	}{
		{name: "empty"},
		{name: "empty after shell use", edit: "printf history > .ash_history"},
		{name: "empty with file", edit: "touch notes", dirty: true},
		{name: "empty with workspace", edit: "mkdir workspace", dirty: true},
		{name: "new SSH key", edit: "touch .ssh/id_ed25519", dirty: true},
		{name: "clean repository", project: true},
		{name: "repository after status", project: true, edit: "cd workspace; git status --short"},
		{name: "modified", project: true, edit: "echo change >> workspace/file", dirty: true},
		{name: "deleted", project: true, edit: "rm workspace/file", dirty: true},
		{name: "staged", project: true, edit: "cd workspace; echo change >> file; git add file", dirty: true},
		{name: "untracked", project: true, edit: "touch 'workspace/new file'", dirty: true},
		{name: "ignored", project: true, edit: "touch workspace/ignored", dirty: true},
		{name: "executable mode", project: true, edit: "chmod +x workspace/file", dirty: true},
		{name: "new commit", project: true, edit: "cd workspace; echo change >> file; git commit -am changed", dirty: true},
		{name: "commit retained in reflog", project: true, edit: "cd workspace; echo change >> file; git commit -am changed; git reset --hard HEAD~", dirty: true},
		{name: "assume unchanged", project: true, edit: "cd workspace; git update-index --assume-unchanged file; echo change >> file", dirty: true},
		{name: "skip worktree", project: true, edit: "cd workspace; git update-index --skip-worktree file; echo change >> file", dirty: true},
		{name: "commit on other branch", project: true, edit: "cd workspace; git checkout -b other; echo change >> file; git commit -am changed; git checkout main", dirty: true},
		{name: "stash", project: true, edit: "cd workspace; echo change >> file; git stash", dirty: true},
		{name: "file outside checkout", project: true, edit: "touch notes", dirty: true},
		{name: "missing checkout", project: true, edit: "rm -rf workspace", dirty: true},
		{name: "missing git metadata", project: true, edit: "rm -rf workspace/.git", dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			var env []string
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "HOME=") {
					env = append(env, value)
				}
			}
			env = append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
			run := func(script string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", script)
				cmd.Dir, cmd.Env = home, env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("guest script: %v\n%s", err, out)
				}
				return string(out)
			}
			run("mkdir .ssh; touch .ssh/authorized_keys")
			var project *Project
			if tc.project {
				run("git init --initial-branch=main workspace; cd workspace; echo original > file; echo ignored > .gitignore; git add .; git commit -m initial")
				project = &Project{Branch: "main", Commit: strings.TrimSpace(run("cd workspace; git rev-parse HEAD")), Workspace: Workspace}
			}
			if tc.edit != "" {
				run(tc.edit)
			}
			want := "clean\n"
			if tc.dirty {
				want = "changed\n"
			}
			if got := run(changesCommand(project)); got != want {
				t.Fatalf("status = %q, want %q", got, want)
			}
			if _, err := os.Stat(filepath.Join(home, ".ssh", "authorized_keys")); err != nil {
				t.Fatal("change check modified the guest home:", err)
			}
		})
	}
}
