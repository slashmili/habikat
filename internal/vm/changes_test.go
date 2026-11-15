package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuestHasChanges(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		changed, fail  bool
	}{
		{name: "clean", response: "printf 'clean\\n'"},
		{name: "changed", response: "printf 'changed\\n'", changed: true},
		{name: "unreachable", response: "exit 255", fail: true},
		{name: "empty response", response: "exit 0", fail: true},
		{name: "unexpected response", response: "printf 'clean\\nextra\\n'", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateTempDir(t)
			child := startTestProcess(t, firecrackerArgs(dir))
			state := child.state
			state.Ready = true
			bin := privateTempDir(t)
			writeTestFile(t, filepath.Join(bin, "ssh"), []byte("#!/bin/sh\n"+tc.response+"\n"))
			if err := os.Chmod(filepath.Join(bin, "ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			changed, err := guestHasChanges(t.Context(), dir, state)
			if changed != tc.changed || (err != nil) != tc.fail {
				t.Fatalf("change check = %v, %v", changed, err)
			}
			child.assertAlive(t)
		})
	}
	t.Run("stopped", func(t *testing.T) {
		if _, err := guestHasChanges(t.Context(), privateTempDir(t), record{}); err == nil {
			t.Fatal("stopped VM reported clean")
		}
	})
}

func TestDeleteChangeCheck(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		changed, force, fail, missing bool
	}{
		{name: "clean"},
		{name: "changed", changed: true},
		{name: "inspection failure", fail: true},
		{name: "forced", force: true, fail: true},
		{name: "missing", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := privateTempDir(t)
			t.Setenv("HK_STATE_DIR", root)
			dir := filepath.Join(root, "vm")
			if !tc.missing {
				makeVMDir(t, root, "vm")
				if err := saveRecord(dir, record{}); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(dir, "disk"), []byte("keep"))
			}
			checks, prompts, stops := 0, 0, 0
			confirm := func() error { prompts++; return nil }
			if tc.force {
				confirm = nil
			}
			err := deleteConfirmed(t.Context(), "vm", confirm, func(context.Context, string, record) (bool, error) {
				checks++
				if tc.fail {
					return false, errors.New("inspection failed")
				}
				return tc.changed, nil
			}, func(record, string) error { stops++; return nil })
			if tc.fail && !tc.force {
				if err == nil || !strings.Contains(err.Error(), "--force") || checks != 1 || prompts != 0 || stops != 0 {
					t.Fatalf("failed check allowed deletion: %v checks=%d prompts=%d stops=%d", err, checks, prompts, stops)
				}
				assertFileContents(t, filepath.Join(dir, "disk"), []byte("keep"))
				return
			}
			wantChecks, wantPrompts, wantStops := 1, 0, 1
			if tc.changed {
				wantPrompts = 1
			}
			if tc.force || tc.missing {
				wantChecks = 0
			}
			if tc.missing {
				wantStops = 0
			}
			if err != nil || checks != wantChecks || prompts != wantPrompts || stops != wantStops {
				t.Fatalf("deletion = %v checks=%d prompts=%d stops=%d", err, checks, prompts, stops)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("VM directory remains: %v", err)
			}
		})
	}
}
