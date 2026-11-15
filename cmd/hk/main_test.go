package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/slashmili/habikato/internal/vm"
	"io"
	"strings"
	"testing"
)

const invalidNameMessage = "name must be 1–32 lowercase letters, digits or hyphens, starting with a letter"

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(
				[]string{arg},
				strings.NewReader(""),
				&out,
				unexpectedVMOperations(t),
			); err != nil {
				t.Fatal(err)
			}
			if got, want := out.String(), usage+"\n"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestRunCommands(t *testing.T) {
	backendErr := errors.New("backend failed")
	for _, tc := range []struct {
		name       string
		args       []string
		operation  string
		vmName     string
		wantOutput string
	}{
		{
			name:       "create",
			args:       []string{"create", "--name", "alpha-7"},
			operation:  "create",
			vmName:     "alpha-7",
			wantOutput: "Created empty VM alpha-7. Enter with: hk attach alpha-7\n",
		},
		{
			name:       "create with equals",
			args:       []string{"create", "--name=beta-8"},
			operation:  "create",
			vmName:     "beta-8",
			wantOutput: "Created empty VM beta-8. Enter with: hk attach beta-8\n",
		},
		{
			name:      "attach",
			args:      []string{"attach", "shell-9"},
			operation: "attach",
			vmName:    "shell-9",
		},
		{
			name:       "delete",
			args:       []string{"delete", "old-3"},
			operation:  "delete",
			vmName:     "old-3",
			wantOutput: "Deleted old-3.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, outcome := range []struct {
				name string
				err  error
			}{
				{name: "success"},
				{name: "backend failure", err: backendErr},
			} {
				t.Run(outcome.name, func(t *testing.T) {
					var out bytes.Buffer
					calls := 0
					record := func(name string) error {
						calls++
						if name != tc.vmName {
							t.Fatalf("name = %q, want %q", name, tc.vmName)
						}
						if out.Len() != 0 {
							t.Fatalf("output before operation completed: %q", out.String())
						}
						return outcome.err
					}
					recordWithContext := func(ctx context.Context, name string) error {
						if ctx == nil || ctx.Err() != nil {
							t.Fatal("operation received no live context")
						}
						return record(name)
					}
					ops := unexpectedVMOperations(t)
					switch tc.operation {
					case "create":
						ops.create = func(ctx context.Context, name string) (*vm.Project, error) { return nil, recordWithContext(ctx, name) }
					case "attach":
						ops.attach = record
					case "delete":
						ops.delete = func(ctx context.Context, name string, _ func() error) error { return recordWithContext(ctx, name) }
					default:
						t.Fatalf("unknown test operation %q", tc.operation)
					}
					err := run(
						tc.args,
						strings.NewReader(""),
						&out,
						ops,
					)
					if !errors.Is(err, outcome.err) {
						t.Fatalf("error = %v, want %v", err, outcome.err)
					}
					if calls != 1 {
						t.Fatalf("operation called %d times, want 1", calls)
					}
					wantOutput := tc.wantOutput
					if outcome.err != nil {
						wantOutput = ""
					}
					if out.String() != wantOutput {
						t.Fatalf("output = %q, want %q", out.String(), wantOutput)
					}
				})
			}
		})
	}
}

func TestRunProxy(t *testing.T) {
	backendErr := errors.New("proxy failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "backend failure", err: backendErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "private directory/vsock.sock"
			const request = "client bytes\x00\n"
			const response = "server bytes\x00\n"
			var out bytes.Buffer
			calls := 0
			ops := unexpectedVMOperations(t)
			ops.proxy = func(gotPath string, in io.Reader, out io.Writer) error {
				calls++
				if gotPath != path {
					t.Fatalf("socket path = %q, want %q", gotPath, path)
				}
				data, err := io.ReadAll(in)
				if err != nil || string(data) != request {
					t.Fatalf("proxy input = %q, error = %v", data, err)
				}
				if tc.err != nil {
					return tc.err
				}
				_, err = io.WriteString(out, response)
				return err
			}
			err := run(
				[]string{"_proxy", path},
				strings.NewReader(request),
				&out,
				ops,
			)
			if !errors.Is(err, tc.err) || calls != 1 {
				t.Fatalf("proxy error = %v, calls = %d; want %v and 1 call", err, calls, tc.err)
			}
			wantOutput := response
			if tc.err != nil {
				wantOutput = ""
			}
			if out.String() != wantOutput {
				t.Fatalf("proxy output = %q, want %q", out.String(), wantOutput)
			}
		})
	}
}

func TestRunInvalidArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no command", want: usage},
		{name: "unknown command", args: []string{"unknown"}, want: usage},
		{name: "help with extra argument", args: []string{"--help", "extra"}, want: usage},
		{name: "proxy missing socket", args: []string{"_proxy"}, want: usage},
		{name: "proxy extra argument", args: []string{"_proxy", "socket", "extra"}, want: usage},
		{name: "create missing name", args: []string{"create"}, want: "usage: hk create --name NAME"},
		{name: "create missing flag value", args: []string{"create", "--name"}, want: "flag needs an argument: -name"},
		{name: "create unknown flag", args: []string{"create", "--unknown"}, want: "flag provided but not defined: -unknown"},
		{name: "create invalid name", args: []string{"create", "--name", "../escape"}, want: invalidNameMessage},
		{name: "create extra argument", args: []string{"create", "--name", "demo", "extra"}, want: "usage: hk create --name NAME"},
		{name: "attach missing name", args: []string{"attach"}, want: usage},
		{name: "attach extra argument", args: []string{"attach", "demo", "extra"}, want: usage},
		{name: "attach invalid name", args: []string{"attach", "../escape"}, want: invalidNameMessage},
		{name: "delete missing name", args: []string{"delete"}, want: usage},
		{name: "delete extra argument", args: []string{"delete", "demo", "extra"}, want: usage},
		{name: "delete invalid name", args: []string{"delete", "../escape"}, want: invalidNameMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(
				tc.args,
				strings.NewReader(""),
				&out,
				unexpectedVMOperations(t),
			)
			assertErrorMessage(t, err, tc.want)
			if out.Len() != 0 {
				t.Fatalf("invalid command wrote to stdout: %q", out.String())
			}
		})
	}
}

func TestRunHelpOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	if err := run(
		[]string{"--help"},
		strings.NewReader(""),
		errorWriter{err: want},
		unexpectedVMOperations(t),
	); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func unexpectedVMOperations(t *testing.T) vmOperations {
	t.Helper()
	return vmOperations{
		create: func(_ context.Context, name string) (*vm.Project, error) {
			t.Helper()
			t.Fatalf("unexpected create operation for %q", name)
			return nil, nil
		},
		attach: func(name string) error {
			t.Helper()
			t.Fatalf("unexpected attach operation for %q", name)
			return nil
		},
		delete: func(_ context.Context, name string, _ func() error) error {
			t.Helper()
			t.Fatalf("unexpected delete operation for %q", name)
			return nil
		},
		proxy: func(path string, _ io.Reader, _ io.Writer) error {
			t.Helper()
			t.Fatalf("unexpected proxy operation for %q", path)
			return nil
		},
	}
}

func assertErrorMessage(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}
