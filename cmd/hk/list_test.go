package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/slashmili/habikat/internal/vm"
)

func TestListRendersRows(t *testing.T) {
	ops := unexpectedVMOperations(t)
	ops.list = func(ctx context.Context, all bool) ([]vm.Listing, error) {
		if ctx.Err() != nil {
			t.Fatal("list received a cancelled context")
		}
		return []vm.Listing{{Name: "alpha", Status: "running", Directory: "/work/api"}}, nil
	}
	out, err := execute(ops, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(strings.Fields(out), " ") != "NAME STATUS DIRECTORY alpha running /work/api" {
		t.Fatalf("output=%q", out)
	}
}

func TestListPreservesRowsOnError(t *testing.T) {
	ops := unexpectedVMOperations(t)
	ops.list = func(context.Context, bool) ([]vm.Listing, error) {
		return []vm.Listing{{Name: "valid", Status: "stopped", Directory: "/work/valid"}}, errors.New("broken\n\x1b[2J\tmetadata")
	}
	out, err := execute(ops, "list")
	if err == nil || strings.ContainsAny(err.Error(), "\n\t\x1b") {
		t.Fatalf("unsafe diagnostic=%v", err)
	}
	if !strings.Contains(out, "valid") || !strings.Contains(out, "/work/valid") {
		t.Fatalf("valid row discarded: %q", out)
	}
}

func TestListTotalFailureDoesNotClaimEmptySuccess(t *testing.T) {
	ops := unexpectedVMOperations(t)
	ops.list = func(context.Context, bool) ([]vm.Listing, error) {
		return nil, errors.New("unreadable inventory")
	}
	out, err := execute(ops, "list")
	if err == nil || out != "" {
		t.Fatalf("output=%q error=%v", out, err)
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

func FuzzListRendering(f *testing.F) {
	for _, seed := range []string{"/work/api", "", "/a\tb\nc\x1b[2J", "\r\b\x7f\u0085 ‮", string([]byte{255, 0, 27})} {
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
