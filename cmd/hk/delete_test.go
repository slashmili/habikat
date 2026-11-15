package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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
				strings.NewReader(""),
				&out,
				func(gotCtx context.Context, name string, _ func() error) error {
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

func TestRunDeleteCancellation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		backendErr error
	}{
		{name: "approval declined", backendErr: errDeleteCancelled},
		{name: "wrapped approval declined", backendErr: fmt.Errorf("confirmation: %w", errDeleteCancelled)},
		{name: "context cancelled", backendErr: context.Canceled},
		{name: "wrapped context cancelled", backendErr: fmt.Errorf("deletion: %w", context.Canceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := runDelete(
				t.Context(),
				[]string{"demo"},
				strings.NewReader(""),
				&out,
				func(context.Context, string, func() error) error {
					calls++
					if out.Len() != 0 {
						t.Fatal("printed result before deletion returned")
					}
					return tc.backendErr
				},
			)
			if err != nil || calls != 1 {
				t.Fatalf("cancelled deletion error=%v, calls=%d", err, calls)
			}
			if want := "Deletion of demo cancelled.\n"; out.String() != want {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
		})
	}
}

func TestRunDeleteCancellationOutputFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("output unavailable")
	for _, tc := range []struct {
		name       string
		backendErr error
	}{
		{name: "approval declined", backendErr: errDeleteCancelled},
		{name: "context cancelled", backendErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runDelete(
				t.Context(),
				[]string{"demo"},
				strings.NewReader(""),
				errorWriter{err: want},
				func(context.Context, string, func() error) error { return tc.backendErr },
			)
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
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
				strings.NewReader(""),
				&out,
				func(context.Context, string, func() error) error {
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
		strings.NewReader(""),
		errorWriter{err: want},
		func(context.Context, string, func() error) error { return nil },
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestDeleteArgumentsAndApproval(t *testing.T) {
	for _, args := range [][]string{{"demo", "--force"}, {"--force", "demo"}, {"demo"}} {
		opts, err := parseDeleteArgs(args)
		if err != nil || opts.name != "demo" || opts.force != (len(args) == 2) {
			t.Fatalf("parse %v = %+v %v", args, opts, err)
		}
		var out bytes.Buffer
		called := false
		err = runDelete(t.Context(), args, strings.NewReader("yes\n"), &out, func(_ context.Context, name string, confirm func() error) error {
			if confirm != nil {
				if err := confirm(); err != nil {
					return err
				}
			}
			called = true
			return nil
		})
		if opts.force {
			if err != nil || !called {
				t.Fatalf("force: %v %v", called, err)
			}
		} else if err == nil || called || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("pipe authorized destruction: %v %v", called, err)
		}
	}
	for _, args := range [][]string{{"demo", "--force", "--force"}, {"--force=yes", "demo"}, {"--force=false", "demo"}, {"demo", "other"}, {"--force"}, {"", "--force"}} {
		if _, err := parseDeleteArgs(args); err == nil {
			t.Fatalf("malformed accepted: %v", args)
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
	t.Parallel()
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

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("input failed") }

func FuzzDeleteApproval(f *testing.F) {
	for _, seed := range []string{"y\n", "YES\n", "\n", "yes", "yes\x00\n", "y\rno\n"} {
		f.Add(seed, "demo\x00--force")
	}
	f.Fuzz(func(t *testing.T, answer, encoded string) {
		if len(answer) > 4096 || len(encoded) > 4096 {
			t.Skip()
		}
		err := confirmDeletion(t.Context(), "demo", strings.NewReader(answer), &bytes.Buffer{})
		first, _, complete := strings.Cut(answer, "\n")
		first = strings.TrimSuffix(first, "\r")
		valid := complete && len(first) < 64 && (strings.ToLower(first) == "y" || strings.ToLower(first) == "yes")
		if (err == nil) != valid {
			t.Fatalf("ambiguous approval %q: %v", answer, err)
		}
		args := strings.Split(encoded, "\x00")
		opts, err := parseDeleteArgs(args)
		if err == nil && opts.force {
			valid := (len(args) == 2) && ((args[0] == "--force" && args[1] == opts.name) || (args[1] == "--force" && args[0] == opts.name))
			if !valid {
				t.Fatalf("ambiguous force authorization: %q", args)
			}
		}
	})
}

func TestConfirmationRejectsReadErrorWithBytes(t *testing.T) {
	if err := confirmDeletion(t.Context(), "demo", &approvalErrorReader{}, &bytes.Buffer{}); err == nil {
		t.Fatal("input error accompanying newline authorized deletion")
	}
}

type approvalErrorReader struct{ offset int }

func (r *approvalErrorReader) Read(p []byte) (int, error) {
	p[0] = "y\n"[r.offset]
	r.offset++
	if r.offset == 2 {
		return 1, errors.New("terminal input failed")
	}
	return 1, nil
}
