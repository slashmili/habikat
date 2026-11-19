package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slashmili/habikat/internal/vm"
)

func TestStartCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result vm.StartResult
		want   []string
		absent []string
	}{
		{"started", vm.StartResult{}, []string{"Started vm."}, []string{"already running", "--force", "graceful"}},
		{"already running", vm.StartResult{AlreadyRunning: true}, []string{"vm is already running."}, []string{"Started", "--force", "graceful"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := runStart(t.Context(), []string{"vm"}, &out, func(_ context.Context, name string) (vm.StartResult, error) {
				calls++
				if name != "vm" {
					t.Fatalf("start(%q)", name)
				}
				return tc.result, nil
			})
			if err != nil || calls != 1 {
				t.Fatalf("%v, %d", err, calls)
			}
			for _, text := range tc.want {
				if !strings.Contains(out.String(), text) {
					t.Fatalf("output omits %q: %q", text, out.String())
				}
			}
			for _, text := range tc.absent {
				if strings.Contains(out.String(), text) {
					t.Fatalf("output contains %q: %q", text, out.String())
				}
			}
		})
	}
}

func TestStartRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"--force", "vm"}, {"vm", "other"}, {"../vm"}, {"-h"}} {
		err := runStart(t.Context(), args, &bytes.Buffer{}, func(context.Context, string) (vm.StartResult, error) {
			t.Fatal("start called")
			return vm.StartResult{}, nil
		})
		if err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestStartReportsFailure(t *testing.T) {
	var out bytes.Buffer
	failure := errors.New("boot failed")
	err := runStart(t.Context(), []string{"vm"}, &out, func(context.Context, string) (vm.StartResult, error) {
		return vm.StartResult{}, failure
	})
	if !errors.Is(err, failure) || strings.Contains(out.String(), "Started") {
		t.Fatalf("%v, %q", err, out.String())
	}
}

func TestStartDispatch(t *testing.T) {
	calls := 0
	ops := vmOperations{start: func(context.Context, string) (vm.StartResult, error) { calls++; return vm.StartResult{}, nil }}
	if err := run([]string{"start", "vm"}, strings.NewReader(""), &bytes.Buffer{}, ops); err != nil || calls != 1 {
		t.Fatalf("start dispatch: %v, %d", err, calls)
	}
}

func TestHelpDescribesStart(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--help"}, strings.NewReader(""), &out, vmOperations{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hk start NAME") {
		t.Fatalf("help omits start: %s", out.String())
	}
}

func FuzzStartArgs(f *testing.F) {
	for _, s := range []string{"vm", "", "../vm", "vm other", "--force vm", "-h", "\x00", strings.Repeat("a", 33)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			t.Skip()
		}
		args := strings.Split(input, " ")
		valid := len(args) == 1 && vm.ValidateName(args[0]) == nil
		called := false
		err := runStart(t.Context(), args, &bytes.Buffer{}, func(_ context.Context, name string) (vm.StartResult, error) {
			called = true
			if !valid || name != args[0] {
				t.Fatalf("start(%q) for %q", name, args)
			}
			return vm.StartResult{}, nil
		})
		if valid != (err == nil) || valid != called {
			t.Fatalf("args %q: valid=%t called=%t err=%v", args, valid, called, err)
		}
	})
}
