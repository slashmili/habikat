package vm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"a", true}, {"my-vm-2", true}, {strings.Repeat("a", 32), true},
		{"", false}, {"..", false}, {"a/b", false}, {"a\\b", false},
		{"-x", false}, {"a\n", false}, {"a\x00", false}, {"é", false},
		{"X", false}, {"a b", false}, {strings.Repeat("a", 33), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidateName(tc.name) == nil; got != tc.valid {
				t.Fatalf("valid=%v, want %v", got, tc.valid)
			}
		})
	}
}

func FuzzNameConfinement(f *testing.F) {
	for _, name := range []string{"vm", "../escape", "/tmp/x", "a\x00", "a\n", "é", strings.Repeat("a", 33)} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		want := len(name) >= 1 && len(name) <= 32
		for i := range len(name) {
			c := name[i]
			want = want && ((c >= 'a' && c <= 'z') || (i > 0 && ((c >= '0' && c <= '9') || c == '-')))
		}
		accepted := ValidateName(name) == nil
		if accepted != want {
			t.Fatalf("name acceptance differs from grammar: %q", name)
		}
		if accepted {
			base := "/private/state"
			path := filepath.Join(base, name)
			if filepath.Dir(path) != base || filepath.Base(path) != name {
				t.Fatalf("name escaped its state directory: %q", name)
			}
		}
	})
}

func TestPrivateDir(t *testing.T) {
	root := privateTempDir(t)
	if err := privateDir(root); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(privateTempDir(t), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(root); err == nil {
		t.Fatal("accepted shared state directory")
	}
}

func TestStateLockCancellation(t *testing.T) {
	t.Setenv("HK_STATE_DIR", privateTempDir(t))
	_, lock, err := lockState(t.Context(), "one")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, next, err := lockState(ctx, "two"); !errors.Is(err, context.DeadlineExceeded) || next != nil {
		t.Fatalf("lock ignored cancellation: %v", err)
	}
}

func TestStateHome(t *testing.T) {
	root := privateTempDir(t)
	t.Chdir(root)
	for _, tc := range []struct {
		name, state, data, home, want string
	}{
		{name: "explicit", state: "state/../chosen", data: "ignored", want: filepath.Join(root, "chosen")},
		{name: "xdg", data: "data", home: "ignored", want: filepath.Join(root, "data", "habikato")},
		{name: "home", home: root, want: filepath.Join(root, ".local", "share", "habikato")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HK_STATE_DIR", tc.state)
			t.Setenv("XDG_DATA_HOME", tc.data)
			t.Setenv("HOME", tc.home)
			got, err := stateHome()
			if err != nil || got != tc.want {
				t.Fatalf("stateHome() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestStateLockRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"root symlink", "shared root", "lock symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTempDir(t)
			outside := privateTempDir(t)
			sentinel := filepath.Join(outside, "keep")
			writeTestFile(t, sentinel, []byte("unchanged"))
			switch kind {
			case "root symlink":
				root = filepath.Join(root, "link")
				if err := os.Symlink(outside, root); err != nil {
					t.Fatal(err)
				}
			case "shared root":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			case "lock symlink":
				if err := os.Symlink(sentinel, filepath.Join(root, ".lock")); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HK_STATE_DIR", root)
			dir, lock, err := lockState(t.Context(), "vm")
			if lock != nil {
				lock.Close()
			}
			if err == nil || dir != "" || lock != nil {
				t.Fatalf("unsafe lock returned %q, %v, %v", dir, lock, err)
			}
			assertFileContents(t, sentinel, []byte("unchanged"))
			if _, err := os.Lstat(filepath.Join(root, "vm")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("VM directory created on rejection: %v", err)
			}
		})
	}
}

func TestStateLockInvalidNameDoesNotCreateRoot(t *testing.T) {
	root := filepath.Join(privateTempDir(t), "absent")
	t.Setenv("HK_STATE_DIR", root)
	if _, lock, err := lockState(t.Context(), "../escape"); err == nil || lock != nil {
		t.Fatalf("invalid name accepted: %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid name touched state root: %v", err)
	}
}

func TestStateLockRelease(t *testing.T) {
	root := privateTempDir(t)
	t.Setenv("HK_STATE_DIR", root)
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		dir, lock, err := lockState(ctx, "vm")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		if dir != filepath.Join(root, "vm") {
			t.Fatalf("locked %q", dir)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	dir := privateTempDir(t)
	for _, want := range []record{{}, {PID: 42, StartTime: "12345"}, {PID: 42, StartTime: "67890", Ready: true}} {
		if err := saveRecord(dir, want); err != nil {
			t.Fatal(err)
		}
		if got, err := readRecord(dir); err != nil || got != want {
			t.Fatalf("readRecord() = %+v, %v; want %+v", got, err, want)
		}
		info, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("state permissions: %v, %v", info, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "state.tmp")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary record remains: %v", err)
		}
	}
}

func TestRecordRejectsCorruption(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{name: "syntax", data: "{", want: "read VM state:"},
		{name: "null", data: "null", want: "read VM state: expected an object"},
		{name: "array", data: "[]", want: "read VM state:"},
		{name: "pid type", data: `{"pid":"42"}`, want: "read VM state:"},
		{name: "negative pid", data: `{"pid":-1}`, want: "invalid VM process identity"},
		{name: "missing start", data: `{"pid":42}`, want: "invalid VM process identity"},
		{name: "ready without pid", data: `{"ready":true}`, want: "invalid VM process identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateTempDir(t)
			path := filepath.Join(dir, "state.json")
			writeTestFile(t, path, []byte(tc.data))
			got, err := readRecord(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != (record{}) {
				t.Fatalf("readRecord() = %+v, %v; want %q", got, err, tc.want)
			}
			assertFileContents(t, path, []byte(tc.data))
		})
	}
}

func FuzzReadRecord(f *testing.F) {
	for _, data := range []string{`{}`, `null`, `{`, `{"pid":-1}`, `{"pid":5,"start_time":"123","ready":true}`} {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		dir := privateTempDir(t)
		valid := record{PID: len(data) + 1, StartTime: "1" + hex.EncodeToString(data), Ready: len(data)%2 == 0}
		if err := saveRecord(dir, valid); err != nil {
			t.Fatal(err)
		}
		if got, err := readRecord(dir); err != nil || got != valid {
			t.Fatalf("generated valid record = %+v, %v; want %+v", got, err, valid)
		}
		writeTestFile(t, filepath.Join(dir, "state.json"), data)
		got, err := readRecord(dir)
		if err != nil {
			if got != (record{}) {
				t.Fatalf("failed read exposed state: %+v", got)
			}
			return
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil || object == nil {
			t.Fatalf("accepted non-object state %q", data)
		}
		if got.PID < 0 || (got.PID > 0 && got.StartTime == "") || (got.Ready && got.PID == 0) {
			t.Fatalf("accepted invalid process identity: %+v", got)
		}
		if err := saveRecord(dir, got); err != nil {
			t.Fatal(err)
		}
		if again, err := readRecord(dir); err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("state changed after save: %+v, %v; want %+v", again, err, got)
		}
	})
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("contents of %s = %q, %v; want %q", path, got, err, want)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
