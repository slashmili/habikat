package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestStopReportsResult(t *testing.T) {
	for force, want := range map[bool]string{
		false: "Stopped vm.\n",
		true:  "Forced stop: buffered guest data may be lost.\nStopped vm.\n",
	} {
		var out bytes.Buffer
		err := runStop(t.Context(), &out, "vm", force, func(_ context.Context, name string, gotForce bool) error {
			if name != "vm" || gotForce != force {
				t.Fatalf("stop(%q, %t)", name, gotForce)
			}
			return nil
		})
		if err != nil || out.String() != want {
			t.Fatalf("output = %q, error = %v; want %q", out.String(), err, want)
		}
	}
}

func TestStopReportsFailure(t *testing.T) {
	var out bytes.Buffer
	failure := errors.New("shutdown timed out")
	err := runStop(t.Context(), &out, "vm", false, func(context.Context, string, bool) error { return failure })
	if !errors.Is(err, failure) || out.Len() != 0 {
		t.Fatalf("%v, %q", err, out.String())
	}
}
