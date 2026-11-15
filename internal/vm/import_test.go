package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectMetadataAndAttach(t *testing.T) {
	dir := privateTempDir(t)
	project := &Project{Branch: "topic/'$%;()", Commit: strings.Repeat("a", 40), Workspace: Workspace}
	state := record{PID: 42, StartTime: "123", Ready: true, Project: project}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	got, err := readRecord(dir)
	if err != nil || !reflect.DeepEqual(got, state) {
		t.Fatalf("metadata=%+v %v", got, err)
	}
	if attachCommand(nil) != "" {
		t.Fatal("legacy/empty VM has workspace command")
	}
	command := attachCommand(project)
	if !strings.Contains(command, "cd /home/hk/workspace") || strings.Contains(command, "/bin/sh -l") || strings.Contains(command, project.Branch) {
		t.Fatalf("unsafe attach command: %s", command)
	}
	for _, value := range []string{`{"project":{"workspace":"/tmp/escape","branch":"main","commit":"` + project.Commit + `"}}`, `{"project":{}}`} {
		writeTestFile(t, filepath.Join(dir, "state.json"), []byte(value))
		if _, err := readRecord(dir); err == nil {
			t.Fatal("invalid workspace metadata accepted")
		}
	}
}

func TestSSHCommandFailureAndCancellation(t *testing.T) {
	dir := privateTempDir(t)
	script := filepath.Join(dir, "ssh")
	writeTestFile(t, script, []byte("#!/bin/sh\necho 'fixture disk full' >&2\nexit 28\n"))
	if err := os.Chmod(script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if err := sshCommand(t.Context(), dir, "ignored", nil); err == nil || !strings.Contains(err.Error(), "fixture disk full") {
		t.Fatalf("failed transfer=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sshCommand(ctx, dir, "ignored", strings.NewReader("bytes")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestSSHOutputLimit(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			dir := privateTempDir(t)
			script := "#!/bin/sh\n"
			if stream == "stderr" {
				script += "exec 1>&2\n"
			}
			script += "i=0\nwhile [ \"$i\" -lt 65 ]; do\nprintf '%s' '" + strings.Repeat("x", 1024) + "'\ni=$((i + 1))\ndone\n"
			writeTestFile(t, filepath.Join(dir, "ssh"), []byte(script))
			if err := os.Chmod(filepath.Join(dir, "ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			out, err := sshOutput(t.Context(), dir, "ignored", nil)
			if !errors.Is(err, errGuestOutputLimit) || len(out) != 0 {
				t.Fatalf("oversized guest output accepted: bytes=%d error=%v", len(out), err)
			}
			if len(err.Error()) > maxGuestOutput+256 {
				t.Fatalf("diagnostic exceeded capture limit: %d bytes", len(err.Error()))
			}
		})
	}
}

func FuzzImportBoundary(f *testing.F) {
	for _, branch := range []string{"main", "topic/'quote%$;()", "a\n$(touch /tmp/escaped)", "'; exit 77; #", "-option", "a b"} {
		f.Add(branch, strings.Repeat("a", 40))
	}
	f.Fuzz(func(t *testing.T, branch, commit string) {
		if strings.ContainsRune(branch, 0) || strings.ContainsRune(commit, 0) || len(branch)+len(commit) > 4096 {
			t.Skip()
		}
		prefix := importAssignmentsWithFixedDestinations(t, importCommand(branch, commit))
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", prefix+"\nprintf '%s\\000%s' \"$branch\" \"$commit\"")
		out, err := cmd.Output()
		want := append(append([]byte(branch), 0), []byte(commit)...)
		if err != nil || !bytes.Equal(out, want) {
			t.Fatalf("input escaped assignment: %q %q, %q %v", branch, commit, out, err)
		}
	})
}

func importAssignmentsWithFixedDestinations(t *testing.T, command string) string {
	t.Helper()
	start := strings.Index(command, "branch=")
	end := strings.LastIndex(command, "\ngit -c core.hooksPath=/dev/null clone")
	if start < 0 || end < start {
		t.Fatal("import lost data boundary")
	}
	cloneDestination := "/home/hk/.hk-import/project.bundle /home/hk/.hk-workspace"
	finalDestination := "mv /home/hk/.hk-workspace /home/hk/workspace"
	if !strings.Contains(command[end:], cloneDestination) || !strings.Contains(command[end:], finalDestination) {
		t.Fatal("destinations changed")
	}
	return command[start:end]
}
