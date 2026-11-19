package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestListMissingRootDoesNotWrite(t *testing.T) {
	parent := privateTempDir(t)
	root := filepath.Join(parent, "absent")
	t.Setenv("HK_STATE_DIR", root)
	t.Setenv("PATH", "")
	for _, all := range []bool{false, true} {
		rows, err := List(t.Context(), all)
		if err != nil || len(rows) != 0 {
			t.Fatalf("missing root: %+v, %v", rows, err)
		}
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("list created root: %v", err)
		}
	}
}

func TestListAllIsSortedReadOnlyAndIndependentOfGit(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	t.Setenv("PATH", "")
	for _, tc := range []struct{ name, source string }{
		{name: "zeta", source: "/deleted/project"},
		{name: "alpha", source: "/work/alpha"},
	} {
		dir := makeVMDir(t, root, tc.name)
		if err := saveRecord(dir, currentRecordWith(t, func(s *record) { s.SourceDirectory = tc.source })); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "identity"), []byte("must not inspect key"))
		if err := os.Chmod(filepath.Join(dir, "identity"), 0000); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, ".lock"), []byte("existing lock"))
	lock, err := os.Open(filepath.Join(root, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before := listStateSnapshot(t, root)
	rows, err := List(t.Context(), true)
	want := []Listing{
		{Name: "alpha", Status: "stopped", Directory: "/work/alpha"},
		{Name: "zeta", Status: "stopped", Directory: "/deleted/project"},
	}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v error=%v", rows, err)
	}
	if !reflect.DeepEqual(before, listStateSnapshot(t, root)) {
		t.Fatal("listing changed state")
	}
}

func TestListPartialErrors(t *testing.T) {
	for _, kind := range []string{"corrupt", "missing", "symlink directory", "symlink state", "FIFO state", "unreadable", "invalid name"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			good := makeVMDir(t, root, "good")
			if err := saveRecord(good, currentRecord(t)); err != nil {
				t.Fatal(err)
			}
			bad := filepath.Join(root, "bad")
			if kind == "symlink directory" {
				if err := os.Symlink(good, bad); err != nil {
					t.Fatal(err)
				}
			} else {
				bad = makeVMDir(t, root, "bad")
				switch kind {
				case "corrupt":
					writeTestFile(t, filepath.Join(bad, "state.json"), []byte("{"))
				case "symlink state":
					if err := os.Symlink(filepath.Join(good, "state.json"), filepath.Join(bad, "state.json")); err != nil {
						t.Fatal(err)
					}
				case "FIFO state":
					if err := unix.Mkfifo(filepath.Join(bad, "state.json"), 0600); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					writeTestFile(t, filepath.Join(bad, "state.json"), []byte("{}"))
					if err := os.Chmod(filepath.Join(bad, "state.json"), 0000); err != nil {
						t.Fatal(err)
					}
				case "invalid name":
					if err := os.Rename(bad, filepath.Join(root, "bad\n\x1b[2J")); err != nil {
						t.Fatal(err)
					}
				}
			}
			rows, err := List(t.Context(), true)
			if err == nil || len(rows) != 1 || rows[0].Name != "good" {
				t.Fatalf("rows=%+v error=%v", rows, err)
			}
		})
	}
}

func TestListRejectsSymlinkRoot(t *testing.T) {
	root := privateTempDir(t)
	alias := filepath.Join(privateTempDir(t), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HK_STATE_DIR", alias)
	if _, err := List(t.Context(), true); err == nil {
		t.Fatal("followed symlinked state root")
	}
}

func TestListEntryDisappearing(t *testing.T) {
	root := privateTempDir(t)
	dir := makeVMDir(t, root, "gone")
	if err := saveRecord(dir, currentRecord(t)); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	entries, err := opened.ReadDir(-1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := listEntry(opened, entries[0].Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disappearing directory error=%v", err)
	}
}

func TestObserveStatus(t *testing.T) {
	dir := privateTempDir(t)
	child := startTestProcess(t, firecrackerArgs(dir))
	boot, err := hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	state := child.state
	state.BootID = boot
	for _, tc := range []struct {
		name   string
		change func(*record)
		want   string
	}{
		{name: "running", change: func(*record) {}, want: "running"},
		{name: "starting", change: func(s *record) { s.Ready = false }, want: "starting"},
		{name: "stopped", change: func(s *record) { s.PID = 0; s.Ready = false }, want: "stopped"},
		{name: "stale ready", change: func(s *record) { s.StartTime = "1" }, want: "stopped"},
		{name: "previous boot", change: func(s *record) { s.BootID = "00000000-0000-0000-0000-000000000000" }, want: "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := state
			tc.change(&next)
			if got := observeStatus(next, dir); got != tc.want {
				t.Fatalf("status=%q want=%q", got, tc.want)
			}
			child.assertAlive(t)
		})
	}
	if got := observeStatus(state, privateTempDir(t)); got != "stopped" {
		t.Fatalf("different VM config status=%q", got)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.await(t)
	if got := observeStatus(state, dir); got != "stopped" {
		t.Fatalf("exited status=%q", got)
	}
}

func TestListRecordSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		source  string
		invalid bool
	}{
		{name: "missing", data: recordWithoutField(t, currentRecord(t), "source_directory"), invalid: true},
		{name: "empty", data: currentRecordJSON(t, `{"source_directory":""}`), invalid: true},
		{name: "known", data: currentRecordJSON(t, `{"source_directory":"/work/api"}`), source: "/work/api"},
		{name: "missing path", data: currentRecordJSON(t, `{"source_directory":"/deleted/api"}`), source: "/deleted/api"},
		{name: "relative", data: currentRecordJSON(t, `{"source_directory":"../api"}`), invalid: true},
		{name: "unclean", data: currentRecordJSON(t, `{"source_directory":"/work/../api"}`), invalid: true},
		{name: "nul", data: currentRecordJSON(t, `{"source_directory":"/work/\u0000api"}`), invalid: true},
		{name: "bad boot", data: currentRecordJSON(t, `{"boot_id":"invalid"}`), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := decodeRecord(tc.data)
			if (err != nil) != tc.invalid || state.SourceDirectory != tc.source {
				t.Fatalf("state=%+v error=%v", state, err)
			}
			if err == nil {
				dir := privateTempDir(t)
				if err := saveRecord(dir, state); err != nil {
					t.Fatal(err)
				}
				next, err := readRecord(dir)
				if err != nil || !reflect.DeepEqual(next, state) {
					t.Fatalf("round-trip=%+v, %v", next, err)
				}
			}
		})
	}
}

func TestMatchesSource(t *testing.T) {
	for _, tc := range []struct {
		name, directory, status string
		want                    bool
	}{
		{name: "running", directory: "/work/api", status: "running", want: true},
		{name: "starting", directory: "/work/api", status: "starting", want: true},
		{name: "stopping", directory: "/work/api", status: "stopping", want: true},
		{name: "sibling", directory: "/work/api-two", status: "running"},
		{name: "child", directory: "/work/api/child", status: "running"},
		{name: "stopped", directory: "/work/api", status: "stopped"},
		{name: "unknown", directory: "/work/api", status: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesSource(Listing{Directory: tc.directory, Status: tc.status}, "/work/api"); got != tc.want {
				t.Fatalf("matches=%v want=%v", got, tc.want)
			}
		})
	}
}

func FuzzListContext(f *testing.F) {
	for _, seed := range []string{"/work/api", "/", "", "../escape", "/work/api\n", "/a\x00b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		for _, sibling := range []string{path + "-other", path + "/child", path + ".."} {
			if matchesSource(Listing{Status: "running", Directory: sibling}, path) {
				t.Fatalf("conflated %q and %q", path, sibling)
			}
		}
		if matchesSource(Listing{Status: "stopped", Directory: path}, path) {
			t.Fatal("stopped VM matched")
		}
	})
}

func FuzzListRecordReadOnly(f *testing.F) {
	for _, seed := range []string{currentRecordSeed(f, "/work/api"), `{}`, `null`, `{`, `{"source_directory":"/work/api"}`, `{"pid":1,"start_time":"1","ready":true}`, `{"source_directory":"../../escape"}`} {
		f.Add(seed, "vm")
	}
	f.Add(`{}`, "../escape")
	f.Fuzz(func(t *testing.T, raw, name string) {
		if len(raw) > 1<<20 || len(name) > 512 {
			t.Skip()
		}
		root := privateTempDir(t)
		dir := makeVMDir(t, root, "vm")
		writeTestFile(t, filepath.Join(dir, "state.json"), []byte(raw))
		opened, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		before := listStateSnapshot(t, root)
		row, listErr := listEntry(opened, name)
		if ValidateName(name) != nil && listErr == nil {
			t.Fatalf("unconfined name accepted: %q", name)
		}
		if listErr == nil && row.Name != name {
			t.Fatalf("wrong row: %+v", row)
		}
		_, _ = listDirectory(t.Context(), opened, "")
		if !reflect.DeepEqual(before, listStateSnapshot(t, root)) {
			t.Fatal("listing mutated records or directory entries")
		}
	})
}

type listFileSnapshot struct {
	Mode     os.FileMode
	Size     int64
	Modified int64
	Data     string
}

func listStateSnapshot(t *testing.T, root string) map[string]listFileSnapshot {
	t.Helper()
	result := map[string]listFileSnapshot{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshot := listFileSnapshot{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime().UnixNano()}
		if entry.Type().IsRegular() && entry.Name() != "identity" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Data = string(data)
		}
		result[strings.TrimPrefix(path, root)] = snapshot
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestListCancellation(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	if err := saveRecord(makeVMDir(t, root, "one"), currentRecord(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := List(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestListReportsIncompleteRecordWithoutRepairingIt(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	good := makeVMDir(t, root, "good")
	if err := saveRecord(good, currentRecord(t)); err != nil {
		t.Fatal(err)
	}
	dir := makeVMDir(t, root, "incomplete")
	raw := []byte(`{"source_directory":"/work/project","pid":0,"start_time":"","ready":false}`)
	writeTestFile(t, filepath.Join(dir, "state.json"), raw)
	rows, err := List(t.Context(), true)
	assertErrorContains(t, err, `VM "incomplete": unsupported or corrupt VM state`)
	if len(rows) != 1 || rows[0].Name != "good" {
		t.Fatalf("valid row discarded: %+v", rows)
	}
	assertFileContents(t, filepath.Join(dir, "state.json"), raw)
}

func TestObserveStoppingStatus(t *testing.T) {
	root := privateTempDir(t)
	dir := makeVMDir(t, root, "vm")
	child := startTestProcess(t, firecrackerArgs(dir))
	state := child.state
	var err error
	state.BootID, err = hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	state.Stopping = true
	for _, ready := range []bool{false, true} {
		state.Ready = ready
		if got := observeStatus(state, dir); got != "stopping" {
			t.Errorf("active shutdown reported as %q with ready=%t", got, ready)
		}
	}
}

func TestListSkipsStorageBeingDeleted(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, ".deleting-vm-0123456789abcdef")
	if err := saveRecord(dir, currentRecord(t)); err != nil {
		t.Fatal(err)
	}
	rows, err := List(t.Context(), true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("listed storage being deleted: %+v, %v", rows, err)
	}
}
