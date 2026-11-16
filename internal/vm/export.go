package vm

import (
	"strings"

	"github.com/slashmili/habikato/internal/git"
)

func fetchStatusCommand(base string) string {
	g := git.ExportRemote()
	return cleanGuestCommand(fetchRepositoryScript(base) + "\nprintf 'HKSTATUS1\\n'\n" + g.Command(git.Status("-z", "--ignore-submodules=none")...))
}

func exportCommand(base string) string {
	return cleanGuestCommand(exportScript(base))
}

func exportScript(base string) string {
	g := git.ExportRemote()
	return fetchRepositoryScript(base) + "\n" + strings.Join([]string{
		"umask 077",
		`temporary=$(/bin/busybox mktemp -d /tmp/hk-export.XXXXXXXXXX)`,
		`trap 'rm -rf "$temporary"' EXIT`,
		`trap 'exit 1' HUP INT TERM`,
		g.Command("for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/") + ` > "$temporary/before"`,
		`test -s "$temporary/before" || fail 'guest repository has no local branches to fetch'`,
		g.Command("bundle", "create") + ` "$temporary/project.bundle" --branches`,
		g.Command("bundle", "list-heads") + ` "$temporary/project.bundle" | /bin/busybox sort -k2 > "$temporary/bundle-heads"`,
		g.Command("for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/") + ` > "$temporary/after"`,
		`/bin/busybox cmp -s "$temporary/before" "$temporary/bundle-heads" && /bin/busybox cmp -s "$temporary/before" "$temporary/after" || fail 'guest branches changed during export; retry fetch'`,
		`exec 3<"$temporary/project.bundle"`,
		`rm -rf "$temporary"`,
		`trap - EXIT HUP INT TERM`,
		`cat <&3`,
	}, "\n")
}

func cleanGuestCommand(script string) string {
	return `exec env -i PATH=/usr/bin:/bin HOME="$HOME" /bin/sh -c ` + shellQuote(script)
}

func fetchRepositoryScript(base string) string {
	g := git.ExportRemote()
	return strings.Join([]string{
		"set -eu",
		`fail() { printf '%s\n' "$1" >&2; exit 1; }`,
		g.Environment(),
		`test ! -L "$HOME/workspace" && test -d "$HOME/workspace/.git" || fail 'guest project workspace is missing or invalid'`,
		`cd "$HOME/workspace"`,
		`root=$(` + g.Command("rev-parse", "--show-toplevel") + `)`,
		`test "$root" = "$PWD" || fail 'guest workspace is not the expected Git repository'`,
		`test "$(` + g.Command("cat-file", "-t", base) + `)" = commit || fail 'guest project base commit is missing'`,
		`test "$(` + g.Command("rev-parse", "--is-shallow-repository") + `)" = false || fail 'shallow guest repositories are not supported'`,
		fetchInspectionSafetyScript(g),
		`unreferenced=$(` + g.Command("rev-list", "--max-count=1", "HEAD", "--not", "--branches") + `)`,
		`test -z "$unreferenced" || fail 'guest HEAD contains commits not reachable from a local branch; create a branch in the VM before fetching'`,
	}, "\n")
}

func fetchInspectionSafetyScript(g git.Remote) string {
	return strings.Join([]string{
		`filters=$(` + g.Command("config", "--name-only", "--get-regexp", "^filter[.]") + ` || test "$?" = 1)`,
		`test -z "$filters" || fail 'cannot safely inspect guest status with configured Git filters'`,
		`files=$(` + g.WithConfig("core.quotePath=true").Command(git.IndexFlags()...) + `)`,
		`if printf '%s\n' "$files" | grep -q '^[a-zS] '; then fail 'cannot check guest changes with assume-unchanged or skip-worktree flags; unset them before fetching'; fi`,
	}, "\n")
}
