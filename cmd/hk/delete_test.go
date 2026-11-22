package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDeleteReportsResult(t *testing.T) {
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
			err := runDelete(ctx, strings.NewReader(""), &out, "demo", true, func(gotCtx context.Context, name string, _ func() error) error {
				if name != "demo" || gotCtx != ctx {
					t.Fatalf("delete received name=%q or a different context", name)
				}
				if out.Len() != 0 {
					t.Fatal("printed success before deletion finished")
				}
				return tc.backendErr
			})
			if !errors.Is(err, tc.backendErr) || out.String() != tc.wantOutput {
				t.Fatalf("error = %v, output = %q", err, out.String())
			}
		})
	}
}

func TestDeleteCancellation(t *testing.T) {
	for _, backendErr := range []error{
		errDeleteCancelled,
		fmt.Errorf("confirmation: %w", errDeleteCancelled),
		context.Canceled,
		fmt.Errorf("deletion: %w", context.Canceled),
	} {
		var out bytes.Buffer
		err := runDelete(t.Context(), strings.NewReader(""), &out, "demo", false, func(context.Context, string, func() error) error {
			return backendErr
		})
		if err != nil || out.String() != "Deletion of demo cancelled.\n" {
			t.Fatalf("%v: error = %v, output = %q", backendErr, err, out.String())
		}
	}
}

func TestDeleteCancellationOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	for _, backendErr := range []error{errDeleteCancelled, context.Canceled} {
		err := runDelete(t.Context(), strings.NewReader(""), errorWriter{err: want}, "demo", false, func(context.Context, string, func() error) error {
			return backendErr
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	}
}

func TestDeleteOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	err := runDelete(t.Context(), strings.NewReader(""), errorWriter{err: want}, "demo", true, func(context.Context, string, func() error) error {
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestDeleteRequiresTerminalOrForce(t *testing.T) {
	for _, args := range [][]string{{"delete", "demo"}, {"delete", "--force", "demo"}, {"delete", "demo", "--force"}, {"delete", "--force=false", "demo"}} {
		deleted := false
		ops := unexpectedVMOperations(t)
		ops.delete = func(_ context.Context, _ string, confirm func() error) error {
			if confirm != nil {
				if err := confirm(); err != nil {
					return err
				}
			}
			deleted = true
			return nil
		}
		var out bytes.Buffer
		err := run(args, strings.NewReader("yes\n"), &out, ops)
		forced := strings.Contains(strings.Join(args, " "), "--force") && !strings.Contains(strings.Join(args, " "), "=false")
		if forced && (err != nil || !deleted) {
			t.Fatalf("%q: deleted=%t err=%v", args, deleted, err)
		}
		if !forced && (err == nil || deleted || !strings.Contains(err.Error(), "--force")) {
			t.Fatalf("%q: pipe authorized destruction: deleted=%t err=%v", args, deleted, err)
		}
	}
}

func TestConfirmation(t *testing.T) {
	for _, value := range []string{"y\n", "Y\n", "yes\n", "YES\r\n", "\n", "no\n", "y", "yes", " y\n", "yes please\n", "y\x00\n", "yes\rgarbage\n", strings.Repeat("y", 100) + "\n"} {
		var out bytes.Buffer
		err := confirmDeletion(t.Context(), "demo", strings.NewReader(value), &out)
		want := value == "y\n" || value == "Y\n" || value == "yes\n" || value == "YES\r\n"
		if (err == nil) != want {
			t.Fatalf("confirmation %q: %v", value, err)
		}
		if !strings.Contains(out.String(), "Delete VM demo and all its files? [y/N]") {
			t.Fatal("missing destructive confirmation")
		}
		if !strings.Contains(out.String(), "has file changes or new commits") {
			t.Fatal("missing reason for confirmation")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := confirmDeletion(ctx, "demo", strings.NewReader("yes\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("cancelled confirmation accepted")
	}
	if err := confirmDeletion(t.Context(), "demo", failingReader{}, &bytes.Buffer{}); err == nil {
		t.Fatal("failed input authorized deletion")
	}
}

func TestConfirmationOutputFailureDoesNotReadApproval(t *testing.T) {
	want := errors.New("prompt output unavailable")
	const approval = "yes\n"
	in := strings.NewReader(approval)
	err := confirmDeletion(t.Context(), "demo", in, errorWriter{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if in.Len() != len(approval) {
		t.Fatal("read approval after failing to display the prompt")
	}
}

func TestConfirmationRejectsReadErrorWithBytes(t *testing.T) {
	if err := confirmDeletion(t.Context(), "demo", &approvalErrorReader{}, &bytes.Buffer{}); err == nil {
		t.Fatal("input error accompanying newline authorized deletion")
	}
}

func FuzzDeleteApproval(f *testing.F) {
	for _, seed := range []string{"y\n", "YES\n", "\n", "yes", "yes\x00\n", "y\rno\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, answer string) {
		if len(answer) > 4096 {
			t.Skip()
		}
		err := confirmDeletion(t.Context(), "demo", strings.NewReader(answer), &bytes.Buffer{})
		first, _, complete := strings.Cut(answer, "\n")
		first = strings.TrimSuffix(first, "\r")
		valid := complete && len(first) < 64 && (strings.ToLower(first) == "y" || strings.ToLower(first) == "yes")
		if (err == nil) != valid {
			t.Fatalf("ambiguous approval %q: %v", answer, err)
		}
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("input failed") }

type approvalErrorReader struct{ offset int }

func (r *approvalErrorReader) Read(p []byte) (int, error) {
	p[0] = "y\n"[r.offset]
	r.offset++
	if r.offset == 2 {
		return 1, errors.New("terminal input failed")
	}
	return 1, nil
}
