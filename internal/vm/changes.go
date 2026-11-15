package vm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"habikato.local/habikato/internal/git"
)

func guestHasChanges(ctx context.Context, dir string, state record) (bool, error) {
	if !state.Ready || !sameProcess(state, dir) {
		return false, errors.New("VM is not running and ready")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := sshOutput(ctx, dir, changesCommand(state.Project), nil)
	if err != nil {
		return false, err
	}
	switch string(out) {
	case "clean\n":
		return false, nil
	case "changed\n":
		return true, nil
	default:
		return false, fmt.Errorf("unexpected guest change status: %q", out)
	}
}

func changesCommand(project *Project) string {
	command := homeChangesCommand()
	if project == nil {
		return command + `if test -e workspace || test -L workspace; then changed; fi
printf 'clean\n'`
	}
	return command + projectChangesCommand(project)
}

func homeChangesCommand() string {
	return `set -eu
cd "$HOME"
changed() { printf 'changed\n'; exit 0; }
extras=$(find . -mindepth 1 -maxdepth 1 ! -name workspace ! -name .ssh ! -name .ash_history -print)
test -z "$extras" || changed
test ! -L .ssh || changed
if test -d .ssh; then
    extras=$(find .ssh -mindepth 1 ! -path .ssh/authorized_keys -print)
    test -z "$extras" || changed
fi
`
}

func projectChangesCommand(project *Project) string {
	g := git.InspectionRemote()
	return `test ! -L workspace && test -d workspace/.git || changed
cd workspace
` + g.Environment() + `
commit=` + shellQuote(project.Commit) + `
status=$(` + g.Command(git.Status("--ignored")...) + `)
test -z "$status" || changed
check_flags_hiding_worktree_changes() {
    files=$(` + g.WithConfig("core.quotePath=true").Command(git.IndexFlags()...) + `)
    if printf '%s\n' "$files" | grep -q '^[a-zS] '; then changed; fi
}
check_flags_hiding_worktree_changes
head=$(` + g.Command("rev-parse", "--verify", "HEAD") + `)
test "$head" = "$commit" || changed
commits=$(` + g.Command("rev-list", "--all", "--reflog", "--not", project.Commit) + `)
test -z "$commits" || changed
printf 'clean\n'`
}
