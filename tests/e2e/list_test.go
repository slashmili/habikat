//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListVMDirectoryAssociation(t *testing.T) {
	h := newHarness(t)
	first, _, _ := projectFixture(t, h)
	firstSub := h.cwd
	h.must("", "create", "--name", "list-first")
	second := filepath.Join(h.dir, "second-project")
	if err := os.Mkdir(second, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, second, "init", "--initial-branch=main")
	write(t, filepath.Join(second, "second-file"), []byte("second project\n"))
	fixtureGit(t, second, "add", ".")
	fixtureGit(t, second, "commit", "-m", "fixture")
	h.cwd = second
	h.must("", "create", "--name", "list-second")
	empty := filepath.Join(h.dir, "empty")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	h.cwd = empty
	h.must("", "create", "--name", "list-empty")
	legacyDir := filepath.Dir(h.state("list-legacy", "state.json"))
	if err := os.Mkdir(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, h.state("list-legacy", "state.json"), []byte(`{"pid":0,"start_time":"","ready":false}`))
	for name, source := range map[string]string{"list-first": first, "list-second": second, "list-empty": empty} {
		var state struct {
			Source string `json:"source_directory"`
			BootID string `json:"boot_id"`
		}
		if err := json.Unmarshal(read(t, h.state(name, "state.json")), &state); err != nil {
			t.Fatal(err)
		}
		if state.Source != source || state.BootID == "" {
			t.Fatalf("%s state=%+v; expected source=%q", name, state, source)
		}
	}
	alias := filepath.Join(h.dir, "first-alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(first, "file with spaces"), []byte("dirty host remains listable\n"))
	firstIndex := read(t, filepath.Join(first, ".git/index"))
	firstConfig := read(t, filepath.Join(first, ".git/config"))
	metadata := map[string][]byte{}
	for _, name := range []string{"list-empty", "list-first", "list-second", "list-legacy"} {
		metadata[name] = read(t, h.state(name, "state.json"))
	}
	for _, tc := range []struct{ name, directory, want string }{
		{name: "root", directory: first, want: "list-first"},
		{name: "subdirectory", directory: firstSub, want: "list-first"},
		{name: "symlink", directory: alias, want: "list-first"},
		{name: "other repository", directory: second, want: "list-second"},
		{name: "non Git", directory: empty, want: "list-empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.cwd = tc.directory
			rows := listRows(t, h.must("", "list"))
			if len(rows) != 1 || rows[0][0] != tc.want || rows[0][1] != "running" {
				t.Fatalf("default rows=%v", rows)
			}
		})
	}
	h.cwd = h.dir
	short := h.must("", "list", "-a")
	long := h.must("", "list", "--all")
	if short != long {
		t.Fatalf("flag aliases differ: %q vs %q", short, long)
	}
	rows := listRows(t, short)
	names := []string{}
	for _, row := range rows {
		names = append(names, row[0])
	}
	if !reflect.DeepEqual(names, []string{"list-empty", "list-first", "list-legacy", "list-second"}) {
		t.Fatalf("all names=%q", names)
	}
	if !strings.Contains(short, "(unknown)") {
		t.Fatalf("legacy origin missing: %s", short)
	}
	for name, before := range metadata {
		if !bytes.Equal(before, read(t, h.state(name, "state.json"))) {
			t.Fatalf("listing changed metadata for %s", name)
		}
	}
	if !bytes.Equal(firstIndex, read(t, filepath.Join(first, ".git/index"))) || !bytes.Equal(firstConfig, read(t, filepath.Join(first, ".git/config"))) {
		t.Fatal("listing changed host Git metadata")
	}
	if got := string(read(t, filepath.Join(first, "file with spaces"))); got != "dirty host remains listable\n" {
		t.Fatalf("listing changed dirty host file: %q", got)
	}
	listCrashFixture(t, h, "list-first")
	h.cwd = first
	if out := h.must("", "list"); out != "No VMs found.\n" {
		t.Fatalf("exited VM stayed active: %s", out)
	}
	if rows := listRows(t, h.must("", "list", "--all")); rows[1][0] != "list-first" || rows[1][1] != "stopped" {
		t.Fatalf("exited VM not stopped: %+v", rows)
	}
	if !bytes.Equal(metadata["list-first"], read(t, h.state("list-first", "state.json"))) {
		t.Fatal("listing repaired stale runtime metadata")
	}
	h.must("", "delete", "--force", "list-second")
	if out := h.must("", "list", "--all"); strings.Contains(out, "list-second") {
		t.Fatalf("deleted VM remained listed: %s", out)
	}
}

func listRows(t *testing.T, output string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) < 2 || strings.Join(strings.Fields(lines[0]), " ") != "NAME STATUS DIRECTORY" {
		t.Fatalf("invalid list table: %q", output)
	}
	rows := [][]string{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("invalid list row: %q", line)
		}
		rows = append(rows, fields)
	}
	return rows
}

func listCrashFixture(t *testing.T, h *harness, name string) {
	t.Helper()
	pid := vmPID(t, h.state(name, "state.json"))
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Release() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
		if len(fields) > 0 && fields[0] == "Z" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fixture process did not exit")
}
