package git

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestRemoteProfiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote Remote
		config []string
		env    []string
	}{
		{
			name:   "import",
			remote: ImportRemote(),
			config: []string{"-c", "core.hooksPath=/dev/null"},
			env:    []string{"1", "/dev/null", "0", "unset", "unset", "unset"},
		},
		{
			name:   "inspection",
			remote: InspectionRemote(),
			config: []string{
				"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
				"-c", "core.untrackedCache=false", "-c", "core.fileMode=true",
			},
			env: []string{"1", "/dev/null", "0", "0", "1", "unset"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"probe", "", "topic/'quote%$;()", "a\n$(exit 77)", "\ncd /home/hk/.hk-workspace\n", "`exit 78`"}
			probe := `git() { printf '%s\000' "$@"; }
` + tc.remote.Environment() + "\n" + tc.remote.Command(args...) + `
printf '%s\000' "$GIT_CONFIG_NOSYSTEM" "$GIT_CONFIG_GLOBAL" "$GIT_TERMINAL_PROMPT" \
    "${GIT_OPTIONAL_LOCKS-unset}" "${GIT_NO_LAZY_FETCH-unset}" "${GIT_NO_REPLACE_OBJECTS-unset}"`
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", probe)
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.Output()
			want := append(append(append([]string{}, tc.config...), args...), tc.env...)
			if err != nil || !bytes.Equal(out, []byte(strings.Join(want, "\x00")+"\x00")) {
				t.Fatalf("remote command=%q %v, want %q", out, err, want)
			}
		})
	}
}

func TestRemoteWithConfigDoesNotChangeOriginal(t *testing.T) {
	base := InspectionRemote()
	before := base.Command(IndexFlags()...)
	quoted := base.WithConfig("core.quotePath=true")
	other := base.WithConfig("core.quotePath=false")
	if base.Command(IndexFlags()...) != before {
		t.Fatal("command-specific configuration changed the original remote")
	}
	if !strings.Contains(quoted.Command(IndexFlags()...), "core.quotePath=true") {
		t.Fatal("command-specific configuration changed another copy")
	}
	if !strings.Contains(other.Command(IndexFlags()...), "core.quotePath=false") {
		t.Fatal("command-specific configuration missing from copy")
	}
}
