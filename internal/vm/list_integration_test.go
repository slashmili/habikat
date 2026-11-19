//go:build integration

package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestListDirectoryContexts(t *testing.T) {
	first := newExportFixture(t)
	second := newExportFixture(t)
	outside := privateTempDir(t)
	childDir := filepath.Join(outside, "child")
	if err := os.Mkdir(childDir, 0700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(first.repo, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(privateTempDir(t), "alias")
	if err := os.Symlink(first.repo, alias); err != nil {
		t.Fatal(err)
	}
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	boot, err := hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ name, source string }{
		{name: "first", source: first.repo},
		{name: "second", source: second.repo},
		{name: "empty", source: outside},
		{name: "child", source: childDir},
		{name: "deleted", source: filepath.Join(outside, "deleted")},
	} {
		dir := makeVMDir(t, root, fixture.name)
		process := startTestProcess(t, firecrackerArgs(dir))
		state := process.state
		state.BootID = boot
		state.SourceDirectory = fixture.source
		if err := saveRecord(dir, state); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveRecord(makeVMDir(t, root, "stopped"), currentRecordWith(t, func(s *record) { s.SourceDirectory = first.repo })); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.repo, "file"), []byte("dirty host"))
	writeTestFile(t, filepath.Join(first.repo, "untracked"), []byte("untracked host"))
	hostBefore := listStateSnapshot(t, first.repo)
	stateBefore := listStateSnapshot(t, root)
	for _, tc := range []struct{ name, dir, want string }{
		{name: "first repository", dir: first.repo, want: "first"},
		{name: "repository subdirectory", dir: sub, want: "first"},
		{name: "repository symlink", dir: alias, want: "first"},
		{name: "second repository", dir: second.repo, want: "second"},
		{name: "non Git directory", dir: outside, want: "empty"},
		{name: "non Git child", dir: childDir, want: "child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.dir)
			rows, err := List(t.Context(), false)
			if err != nil || len(rows) != 1 || rows[0].Name != tc.want || rows[0].Status != "running" {
				t.Fatalf("rows=%+v error=%v", rows, err)
			}
		})
	}
	t.Chdir(privateTempDir(t))
	rows, err := List(t.Context(), true)
	if err != nil || len(rows) != 6 {
		t.Fatalf("all rows=%+v error=%v", rows, err)
	}
	if !reflect.DeepEqual(stateBefore, listStateSnapshot(t, root)) {
		t.Fatal("listing modified state")
	}
	if !reflect.DeepEqual(hostBefore, listStateSnapshot(t, first.repo)) {
		t.Fatal("listing modified dirty host repository")
	}
}
