package vm

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		{name: "xdg", data: "data", home: "ignored", want: filepath.Join(root, "data", "habikat")},
		{name: "home", home: root, want: filepath.Join(root, ".local", "share", "habikat")},
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

func currentRecord(t *testing.T) record {
	t.Helper()
	boot, err := hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	return record{
		Launch: &launchArtifacts{
			Firecracker: artifactRef{Path: "/images/firecracker", SHA256: strings.Repeat("a", 64)},
			Kernel:      artifactRef{Path: "/images/vmlinux", SHA256: strings.Repeat("b", 64)},
		},
		SourceDirectory: "/source",
		BootID:          boot,
		InstanceID:      strings.Repeat("1", 32),
	}
}

func currentRecordWith(t *testing.T, change func(*record)) record {
	t.Helper()
	state := currentRecord(t)
	change(&state)
	return state
}

func TestRecordRoundTrip(t *testing.T) {
	dir := privateTempDir(t)
	for _, want := range []record{
		currentRecord(t),
		currentRecordWith(t, func(s *record) { s.PID, s.StartTime = 42, "12345" }),
		currentRecordWith(t, func(s *record) { s.PID, s.StartTime, s.Ready = 42, "67890", true }),
	} {
		if err := saveRecord(dir, want); err != nil {
			t.Fatal(err)
		}
		if got, err := readRecord(dir); err != nil || !reflect.DeepEqual(got, want) {
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
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "syntax", data: []byte("{"), want: "read VM state:"},
		{name: "null", data: []byte("null"), want: "read VM state: expected an object"},
		{name: "array", data: []byte("[]"), want: "read VM state:"},
		{name: "pid type", data: currentRecordJSON(t, `{"pid":"42"}`), want: "read VM state:"},
		{name: "negative pid", data: currentRecordJSON(t, `{"pid":-1}`), want: "invalid VM process identity"},
		{name: "missing start", data: currentRecordJSON(t, `{"pid":42}`), want: "invalid VM process identity"},
		{name: "start without pid", data: currentRecordJSON(t, `{"start_time":"123"}`), want: "invalid VM process identity"},
		{name: "nondecimal start", data: currentRecordJSON(t, `{"pid":42,"start_time":"1a"}`), want: "invalid VM process identity"},
		{name: "overflowing start", data: currentRecordJSON(t, `{"pid":42,"start_time":"18446744073709551616"}`), want: "invalid VM process identity"},
		{name: "ready without pid", data: currentRecordJSON(t, `{"ready":true}`), want: "invalid VM process identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateTempDir(t)
			path := filepath.Join(dir, "state.json")
			writeTestFile(t, path, tc.data)
			got, err := readRecord(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != (record{}) {
				t.Fatalf("readRecord() = %+v, %v; want %q", got, err, tc.want)
			}
			assertFileContents(t, path, tc.data)
		})
	}
}

func TestRecordRejectsIncompleteOrOutdatedState(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"unversioned record", func(m map[string]any) { delete(m, "version") }},
		{"unknown version", func(m map[string]any) { m["version"] = currentRecordVersion + 1 }},
		{"null version", func(m map[string]any) { m["version"] = nil }},
		{"missing instance", func(m map[string]any) { delete(m, "instance_id") }},
		{"missing boot", func(m map[string]any) { delete(m, "boot_id") }},
		{"missing launch", func(m map[string]any) { delete(m, "launch") }},
		{"null launch", func(m map[string]any) { m["launch"] = nil }},
		{"missing source directory", func(m map[string]any) { delete(m, "source_directory") }},
		{"missing pid", func(m map[string]any) { delete(m, "pid") }},
		{"null pid", func(m map[string]any) { m["pid"] = nil }},
		{"missing start time", func(m map[string]any) { delete(m, "start_time") }},
		{"null start time", func(m map[string]any) { m["start_time"] = nil }},
		{"missing ready", func(m map[string]any) { delete(m, "ready") }},
		{"null ready", func(m map[string]any) { m["ready"] = nil }},
		{"retired shutdown field", func(m map[string]any) { m["shutdown"] = "ctrl-alt-del-v1" }},
		{"unknown field", func(m map[string]any) { m["recovered"] = true }},
		{"minimal record", func(m map[string]any) { clear(m); m["pid"], m["start_time"], m["ready"] = 0, "", false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := encodeRecord(currentRecord(t))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			tc.edit(fields)
			data, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			dir := privateTempDir(t)
			writeTestFile(t, filepath.Join(dir, "state.json"), data)
			got, err := readRecord(dir)
			if err == nil || !strings.Contains(err.Error(), "unsupported or corrupt VM state") || !reflect.DeepEqual(got, record{}) {
				t.Fatalf("readRecord() = %+v, %v; want unsupported state error", got, err)
			}
			assertFileContents(t, filepath.Join(dir, "state.json"), data)
		})
	}
}

func TestRecordRejectsAmbiguousJSONKeys(t *testing.T) {
	valid := string(currentRecordJSON(t, `{"pid":42,"start_time":"123","ready":false}`))
	for _, tc := range []struct {
		name        string
		original    string
		replacement string
		want        string
	}{
		{"duplicate version", "{", `{"version":0,`, "duplicate"},
		{"duplicate null version", "{", `{"version":null,`, "duplicate"},
		{"duplicate ready", `"ready":false`, `"ready":false,"ready":true`, "duplicate"},
		{"case variant ready", `"ready":false`, `"ready":false,"READY":true`, "unknown"},
		{"duplicate nested path", `"path":"/images/firecracker"`, `"path":"/other/firecracker","path":"/images/firecracker"`, "duplicate"},
		{"case variant nested path", `"path":"/images/firecracker"`, `"PATH":"/images/firecracker"`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := strings.Replace(valid, tc.original, tc.replacement, 1)
			if data == valid {
				t.Fatal("fixture did not change")
			}
			got, err := decodeRecord([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != (record{}) {
				t.Fatalf("decodeRecord() = %+v, %v; want %q error", got, err, tc.want)
			}
		})
	}
}

func currentRecordJSON(t *testing.T, overrides string, removed ...string) []byte {
	t.Helper()
	var changes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(overrides), &changes); err != nil {
		t.Fatal(err)
	}
	data := recordWithoutField(t, currentRecord(t), removed...)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	maps.Copy(fields, changes)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func recordWithoutField(t *testing.T, state record, removed ...string) []byte {
	t.Helper()
	data, err := encodeRecord(state)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range removed {
		delete(fields, field)
	}
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRecordAcceptsNonUTF8SourceDirectory(t *testing.T) {
	dir := privateTempDir(t)
	want := currentRecordWith(t, func(s *record) { s.SourceDirectory = "/source/\xff" })
	if err := saveRecord(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, err := readRecord(dir); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("readRecord() = %+v, %v; want %+v", got, err, want)
	}
}

func TestRecordRejectsInvalidInstanceID(t *testing.T) {
	for _, id := range []string{"../outside", "AABB", strings.Repeat("g", 32), strings.Repeat("a", 31), strings.Repeat("a", 33), "a\n"} {
		t.Run(id, func(t *testing.T) {
			dir := privateTempDir(t)
			state := currentRecordWith(t, func(s *record) { s.InstanceID = id })
			data, err := encodeRecord(state)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(dir, "state.json"), data)
			if _, err := readRecord(dir); err == nil {
				t.Fatal("accepted invalid instance identity")
			}
		})
	}
}

func currentRecordSeed(f *testing.F, source string) string {
	f.Helper()
	data, err := encodeRecord(record{
		Launch: &launchArtifacts{
			Firecracker: artifactRef{Path: "/images/firecracker", SHA256: strings.Repeat("a", 64)},
			Kernel:      artifactRef{Path: "/images/vmlinux", SHA256: strings.Repeat("b", 64)},
		},
		SourceDirectory: source,
		BootID:          "01234567-89ab-cdef-0123-456789abcdef",
		InstanceID:      strings.Repeat("1", 32),
	})
	if err != nil {
		f.Fatal(err)
	}
	return string(data)
}

func FuzzReadRecord(f *testing.F) {
	stopped := currentRecordSeed(f, "/source")
	for _, start := range []string{"0", "18446744073709551615", "18446744073709551616", "1a", ""} {
		live := strings.Replace(stopped, `"pid":0,"start_time":""`, `"pid":5,"start_time":`+strconv.Quote(start), 1)
		f.Add([]byte(live))
	}
	for _, data := range []string{stopped, `{}`, `null`, `{`, `{"pid":-1}`, `{"pid":0,"start_time":"","ready":false}`, `{"pid":5,"start_time":"123","ready":true}`, `{"instance_id":"0123456789abcdef0123456789abcdef"}`, `{"instance_id":"../bad"}`} {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		dir := privateTempDir(t)
		valid := currentRecordWith(t, func(s *record) {
			s.PID, s.StartTime, s.Ready = len(data)+1, strconv.Itoa(len(data)), len(data)%2 == 0
		})
		if err := saveRecord(dir, valid); err != nil {
			t.Fatal(err)
		}
		if got, err := readRecord(dir); err != nil || !reflect.DeepEqual(got, valid) {
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
		if !validProcessIdentity(got) {
			t.Fatalf("accepted invalid process identity: %+v", got)
		}
		if !validInstanceID.MatchString(got.InstanceID) || !validBootID.MatchString(got.BootID) {
			t.Fatal("accepted record without valid instance and boot identities")
		}
		if got.Launch == nil || !validLaunchArtifacts(*got.Launch) || !validAbsolutePath(got.SourceDirectory) {
			t.Fatal("accepted record without valid launch artifacts and source directory")
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
