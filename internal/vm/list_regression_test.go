package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slashmili/habikat/internal/repository"
	"golang.org/x/sys/unix"
)

func TestReadRecordRejectsMalformedBootIdentity(t *testing.T) {
	dir := privateTempDir(t)
	writeTestFile(t, filepath.Join(dir, "state.json"), []byte(`{"boot_id":"invalid"}`))
	if _, err := readRecord(dir); err == nil || !strings.Contains(err.Error(), "invalid VM host boot identity") {
		t.Fatalf("malformed boot identity accepted: %v", err)
	}
}

type cancelAfterTwoEntries struct {
	context.Context
	cancel context.CancelFunc
	calls  int
}

func (c *cancelAfterTwoEntries) Err() error {
	c.calls++
	if c.calls == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestListCanceledPartialInventoryIsSorted(t *testing.T) {
	root := privateTempDir(t)
	for _, name := range []string{"zeta", "alpha", "omega"} {
		if err := saveRecord(makeVMDir(t, root, name), record{}); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	before := listStateSnapshot(t, root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	partial := &cancelAfterTwoEntries{Context: ctx, cancel: cancel}
	rows, err := listDirectory(partial, opened, "")
	if !errors.Is(err, context.Canceled) || len(rows) != 2 {
		t.Fatalf("rows=%+v error=%v", rows, err)
	}
	names := []string{rows[0].Name, rows[1].Name}
	if !slices.IsSorted(names) {
		t.Fatalf("canceled inventory returned unsorted partial rows: %v", names)
	}
	if !reflect.DeepEqual(before, listStateSnapshot(t, root)) {
		t.Fatal("cancellation changed state")
	}
}

func TestListPrivateDirectoryAndSizeBoundaries(t *testing.T) {
	for _, kind := range []string{"shared root", "shared directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			dir := makeVMDir(t, root, "fixture")
			if err := saveRecord(dir, record{}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "shared root":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			case "shared directory":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				writeTestFile(t, filepath.Join(dir, "state.json"), []byte(strings.Repeat(" ", 1<<20+1)))
			}
			before := listStateSnapshot(t, root)
			if rows, err := List(t.Context(), true); err == nil || len(rows) != 0 {
				t.Fatalf("unsafe state accepted: %+v %v", rows, err)
			}
			if !reflect.DeepEqual(before, listStateSnapshot(t, root)) {
				t.Fatal("listing changed state")
			}
		})
	}
}

func TestListDoesNotWaitForLifecycleLockOrReadFIFOKey(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "fixture")
	if err := saveRecord(dir, record{}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "identity"), 0600); err != nil {
		t.Fatal(err)
	}
	_, lock, err := lockState(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before := listStateSnapshot(t, root)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	rows, err := List(ctx, true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("inventory blocked on lifecycle or keys: %+v %v", rows, err)
	}
	if !reflect.DeepEqual(before, listStateSnapshot(t, root)) {
		t.Fatal("listing changed state")
	}
}

func FuzzSourceDirectoryRoundTrip(f *testing.F) {
	for _, seed := range []string{"project", "", ".", "..", "a/b", "a\x00b", "\xff", "\ufffd", "line\nbreak"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if len(name) > 100 || strings.ContainsAny(name, "/\x00") {
			t.Skip()
		}
		parent := privateTempDir(t)
		path := filepath.Join(parent, "source-"+name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		source, err := repository.SourceDirectory(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if source != path {
			t.Fatalf("discovery changed bytes: %q != %q", source, path)
		}
		dir := privateTempDir(t)
		if err := saveRecord(dir, record{SourceDirectory: source}); err != nil {
			t.Fatal(err)
		}
		state, err := readRecord(dir)
		if err != nil || state.SourceDirectory != path {
			t.Fatalf("source changed after persistence: %q != %q, %v", state.SourceDirectory, path, err)
		}
		row := Listing{Status: "running", Directory: state.SourceDirectory}
		if !matchesSource(row, path) || matchesSource(row, path+"-other") {
			t.Fatal("source association changed")
		}
	})
}

func TestSourceDirectoryByteEncoding(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		invalid         bool
	}{
		{name: "byte path", raw: `{"source_directory_bytes":"L3dvcmsv/w=="}`, want: "/work/\xff"},
		{name: "conflicting paths", raw: `{"source_directory":"/other","source_directory_bytes":"L3dvcmsv/w=="}`, invalid: true},
		{name: "bad base64", raw: `{"source_directory_bytes":"!"}`, invalid: true},
		{name: "relative bytes", raw: `{"source_directory_bytes":"Li4vZXNjYXBl"}`, invalid: true},
		{name: "nul bytes", raw: `{"source_directory_bytes":"LwA="}`, invalid: true},
		{name: "invalid JSON UTF8", raw: "{\"source_directory\":\"/work/\xff\"}", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := decodeRecord([]byte(tc.raw))
			if (err != nil) != tc.invalid || state.SourceDirectory != tc.want {
				t.Fatalf("state=%+v error=%v", state, err)
			}
		})
	}
}

func TestReadListedRecordAfterDirectoryRemoval(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed", true: "replaced"}[replacement], func(t *testing.T) {
			root := privateTempDir(t)
			dir := makeVMDir(t, root, "fixture")
			opened, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if _, err := readListedRecord(opened); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("present directory with missing record must be diagnosed: %v", err)
			}
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if replacement {
				makeVMDir(t, root, "fixture")
				if err := saveRecord(dir, record{SourceDirectory: "/replacement"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readListedRecord(opened); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed opened directory: %v", err)
			}
			if replacement {
				state, err := readRecord(dir)
				if err != nil || state.SourceDirectory != "/replacement" {
					t.Fatalf("replacement changed: %+v, %v", state, err)
				}
			}
		})
	}
}

func TestListRejectsMalformedBootIdentity(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "live")
	child := startTestProcess(t, firecrackerArgs(dir))
	state := child.state
	state.BootID = "invalid"
	state.SourceDirectory = "/fixture"
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	good := makeVMDir(t, root, "good")
	if err := saveRecord(good, record{}); err != nil {
		t.Fatal(err)
	}
	before := listStateSnapshot(t, root)
	rows, err := List(t.Context(), true)
	child.assertAlive(t)
	if err == nil || !strings.Contains(err.Error(), "invalid VM host boot identity") || len(rows) != 1 || rows[0].Name != "good" {
		t.Fatalf("invalid boot metadata was not diagnosed while preserving the good row: rows=%+v err=%v", rows, err)
	}
	if !reflect.DeepEqual(before, listStateSnapshot(t, root)) {
		t.Fatal("listing changed corrupt metadata")
	}
}

func TestListDuringAtomicRecordReplacement(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	dir := makeVMDir(t, root, "fixture")
	if err := saveRecord(dir, record{SourceDirectory: "/first"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := range 100 {
			source := []string{"/first", "/second"}[i%2]
			if err := saveRecord(dir, record{SourceDirectory: source}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	defer func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 100 {
		rows, err := List(t.Context(), true)
		if err != nil || len(rows) != 1 || rows[0].Name != "fixture" || rows[0].Status != "stopped" || (rows[0].Directory != "/first" && rows[0].Directory != "/second") {
			t.Fatalf("partial atomic record: %+v, %v", rows, err)
		}
	}
}

func TestListDirectoryBytesRoundTrip(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	parent := privateTempDir(t)
	sourcePath := filepath.Join(parent, "source-"+string([]byte{0xff}))
	differentPath := filepath.Join(parent, "source-\ufffd")
	for _, path := range []string{sourcePath, differentPath} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(sourcePath)
	source, err := repository.SourceDirectory(t.Context(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if source != sourcePath {
		t.Fatalf("discovery changed source bytes: %q", source)
	}
	dir := makeVMDir(t, root, "live")
	child := startTestProcess(t, firecrackerArgs(dir))
	state := child.state
	state.SourceDirectory = source
	state.BootID, err = hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveRecord(dir, state); err != nil {
		t.Fatal(err)
	}
	originalRows, err := List(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(differentPath)
	differentRows, err := List(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("original=%q rows=%+v; different=%q rows=%+v", sourcePath, originalRows, differentPath, differentRows)
	if len(originalRows) != 1 || len(differentRows) != 0 {
		t.Fatal("distinct Linux directory names were conflated by source metadata")
	}
}
