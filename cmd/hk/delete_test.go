package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestRunDelete(t *testing.T) {
	t.Parallel()
	backendErr := errors.New("VM could not be stopped")
	for _, tc := range []struct {
		name       string
		backendErr error
		wantOutput string
	}{
		{name: "success", wantOutput: "Deleted demo.\n"},
		{name: "failure", backendErr: backendErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var out bytes.Buffer
			calls := 0
			err := runDelete(
				ctx,
				[]string{"demo"},
				&out,
				func(gotCtx context.Context, name string) error {
					calls++
					if name != "demo" || gotCtx != ctx {
						t.Fatalf("delete received name=%q or a different context", name)
					}
					if out.Len() != 0 {
						t.Fatal("printed success before deletion finished")
					}
					return tc.backendErr
				},
			)
			if !errors.Is(err, tc.backendErr) || calls != 1 {
				t.Fatalf("delete error=%v, calls=%d", err, calls)
			}
			if out.String() != tc.wantOutput {
				t.Fatalf("output = %q, want %q", out.String(), tc.wantOutput)
			}
		})
	}
}

func TestRunDeleteInvalidArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range nameArgCases {
		if tc.wantErr == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runDelete(
				t.Context(),
				tc.args,
				&out,
				func(context.Context, string) error {
					t.Fatal("invalid arguments reached VM deletion")
					return nil
				},
			)
			assertErrorMessage(t, err, tc.wantErr)
			if out.Len() != 0 {
				t.Fatalf("invalid delete wrote to stdout: %q", out.String())
			}
		})
	}
}

func TestRunDeleteOutputFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("output unavailable")
	err := runDelete(
		t.Context(),
		[]string{"demo"},
		errorWriter{err: want},
		func(context.Context, string) error { return nil },
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
