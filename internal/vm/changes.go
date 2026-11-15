package vm

import (
	"context"
	"errors"
	"fmt"
	"time"
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
	return `test ! -L workspace && test -d workspace/.git || changed
cd workspace
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_OPTIONAL_LOCKS=0 GIT_NO_LAZY_FETCH=1
git() { command git -c core.fsmonitor=false -c core.untrackedCache=false -c core.fileMode=true -c core.hooksPath=/dev/null "$@"; }
commit=` + shellQuote(project.Commit) + `
status=$(git status --porcelain=v1 --untracked-files=all --ignored)
test -z "$status" || changed
check_flags_hiding_worktree_changes() {
    files=$(git -c core.quotePath=true ls-files -v)
    if printf '%s\n' "$files" | grep -q '^[a-zS] '; then changed; fi
}
check_flags_hiding_worktree_changes
head=$(git rev-parse --verify HEAD)
test "$head" = "$commit" || changed
commits=$(git rev-list --all --reflog --not "$commit")
test -z "$commits" || changed
printf 'clean\n'`
}
