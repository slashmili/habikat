package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/slashmili/habikat/internal/vm"
)

func TestStartReportsResult(t *testing.T) {
	for result, want := range map[vm.StartResult]string{
		{}:                     "Started vm.\n",
		{AlreadyRunning: true}: "vm is already running.\n",
	} {
		var out bytes.Buffer
		err := runStart(t.Context(), &out, "vm", func(_ context.Context, name string) (vm.StartResult, error) {
			if name != "vm" {
				t.Fatalf("start(%q)", name)
			}
			return result, nil
		})
		if err != nil || out.String() != want {
			t.Fatalf("output = %q, error = %v; want %q", out.String(), err, want)
		}
	}
}

func TestStartReportsFailure(t *testing.T) {
	var out bytes.Buffer
	failure := errors.New("boot failed")
	err := runStart(t.Context(), &out, "vm", func(context.Context, string) (vm.StartResult, error) {
		return vm.StartResult{}, failure
	})
	if !errors.Is(err, failure) || out.Len() != 0 {
		t.Fatalf("%v, %q", err, out.String())
	}
}
