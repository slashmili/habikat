package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/slashmili/habikato/internal/vm"
)

func TestParseListArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		all     bool
		invalid bool
	}{
		{name: "default"},
		{name: "short", args: []string{"-a"}, all: true},
		{name: "long", args: []string{"--all"}, all: true},
		{name: "repeated", args: []string{"-a", "--all"}, all: true},
		{name: "positional", args: []string{"alpha"}, invalid: true},
		{name: "unknown flag", args: []string{"--json"}, invalid: true},
		{name: "extra argument", args: []string{"-a", "alpha"}, invalid: true},
		{name: "equals", args: []string{"--all=true"}, invalid: true},
		{name: "end flags", args: []string{"--"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all, err := parseListArgs(tc.args)
			if (err != nil) != tc.invalid || all != tc.all {
				t.Fatalf("parseListArgs(%q) = %v, %v", tc.args, all, err)
			}
		})
	}
}

func TestRunList(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		all  bool
	}{
		{name: "default", args: []string{"list"}},
		{name: "short", args: []string{"list", "-a"}, all: true},
		{name: "long", args: []string{"list", "--all"}, all: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := unexpectedVMOperations(t)
			calls := 0
			ops.list = func(ctx context.Context, all bool) ([]vm.Listing, error) {
				calls++
				if ctx.Err() != nil || all != tc.all {
					t.Fatalf("invalid call: %v, %v", ctx.Err(), all)
				}
				return []vm.Listing{{Name: "alpha", Status: "running", Directory: "/work/api"}}, nil
			}
			var out bytes.Buffer
			if err := run(tc.args, strings.NewReader(""), &out, ops); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || strings.Join(strings.Fields(out.String()), " ") != "NAME STATUS DIRECTORY alpha running /work/api" {
				t.Fatalf("calls=%d output=%q", calls, out.String())
			}
		})
	}
}

func TestRunListInvalidInputHasNoEffects(t *testing.T) {
	var out bytes.Buffer
	if err := runList(t.Context(), []string{"bad\n\x1b[2J"}, &out, nil); err == nil || strings.ContainsAny(err.Error(), "\n\x1b") {
		t.Fatalf("error=%v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output=%q", out.String())
	}
}

func TestRunListPreservesRowsOnError(t *testing.T) {
	var out bytes.Buffer
	err := runList(t.Context(), nil, &out, func(context.Context, bool) ([]vm.Listing, error) {
		return []vm.Listing{{Name: "valid", Status: "stopped"}}, errors.New("broken\n\x1b[2J\tmetadata")
	})
	if err == nil || strings.ContainsAny(err.Error(), "\n\t\x1b") {
		t.Fatalf("unsafe diagnostic=%v", err)
	}
	if !strings.Contains(out.String(), "valid") || !strings.Contains(out.String(), "(unknown)") {
		t.Fatalf("valid row discarded: %q", out.String())
	}
}

func TestRunListTotalFailureDoesNotClaimEmptySuccess(t *testing.T) {
	var out bytes.Buffer
	err := runList(t.Context(), nil, &out, func(context.Context, bool) ([]vm.Listing, error) {
		return nil, errors.New("unreadable inventory")
	})
	if err == nil || out.Len() != 0 {
		t.Fatalf("output=%q error=%v", out.String(), err)
	}
}

func TestRenderList(t *testing.T) {
	var out bytes.Buffer
	if err := renderList(&out, nil); err != nil || out.String() != "No VMs found.\n" {
		t.Fatalf("empty output=%q error=%v", out.String(), err)
	}
	want := errors.New("write failed")
	for _, rows := range [][]vm.Listing{nil, {{Name: "one", Status: "running", Directory: "/work"}}} {
		if err := renderList(errorWriter{err: want}, rows); !errors.Is(err, want) {
			t.Fatalf("write failure=%v", err)
		}
	}
}

func FuzzListArgs(f *testing.F) {
	for _, seed := range []string{"", "-a", "--all", "-a\x00--all", "--all=true", "../x", "\n\x1b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		args := strings.Split(raw, "\x00")
		all, err := parseListArgs(args)
		valid := true
		for _, arg := range args {
			valid = valid && (arg == "-a" || arg == "--all")
		}
		if (err == nil) != valid || (err == nil && !all) {
			t.Fatalf("args=%q all=%v err=%v", args, all, err)
		}
		if err != nil {
			assertNoControl(t, err.Error(), false)
		}
	})
}

func FuzzListRendering(f *testing.F) {
	for _, seed := range []string{"/work/api", "", "/a\tb\nc\x1b[2J", "\r\b\x7f\u0085\u2028\u202e", string([]byte{255, 0, 27})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		var out bytes.Buffer
		if err := renderList(&out, []vm.Listing{{Name: "one", Status: "running", Directory: path}}); err != nil {
			t.Fatal(err)
		}
		if strings.Count(out.String(), "\n") != 2 {
			t.Fatalf("injected table row: %q", out.String())
		}
		assertNoControl(t, out.String(), true)
		assertNoControl(t, terminalText(path), false)
	})
}

func assertNoControl(t *testing.T, text string, allowNewline bool) {
	t.Helper()
	for _, r := range text {
		if allowNewline && r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("raw control %U in %q", r, text)
		}
	}
}
