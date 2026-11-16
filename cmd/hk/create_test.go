package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/slashmili/habikato/internal/vm"
)

var createArgCases = []struct {
	name     string
	args     []string
	wantName string
	wantErr  string
}{
	{name: "separate value", args: []string{"--name", "demo-2"}, wantName: "demo-2"},
	{name: "equals value", args: []string{"--name=demo"}, wantName: "demo"},
	{name: "single dash", args: []string{"-name", "demo"}, wantName: "demo"},
	{name: "last flag wins", args: []string{"--name=first", "--name=second"}, wantName: "second"},
	{name: "end of flags", args: []string{"--name=demo", "--"}, wantName: "demo"},
	{name: "maximum length", args: []string{"--name", strings.Repeat("a", 32)}, wantName: strings.Repeat("a", 32)},
	{name: "missing name", wantErr: "usage: hk create --name NAME"},
	{name: "empty value", args: []string{"--name="}, wantErr: "usage: hk create --name NAME"},
	{name: "missing value", args: []string{"--name"}, wantErr: "flag needs an argument: -name"},
	{name: "unknown flag", args: []string{"--unknown"}, wantErr: "flag provided but not defined: -unknown"},
	{name: "positional name", args: []string{"demo"}, wantErr: "usage: hk create --name NAME"},
	{name: "extra argument", args: []string{"--name=demo", "extra"}, wantErr: "usage: hk create --name NAME"},
	{name: "argument before flag", args: []string{"extra", "--name=demo"}, wantErr: "usage: hk create --name NAME"},
	{name: "argument after separator", args: []string{"--name=demo", "--", "extra"}, wantErr: "usage: hk create --name NAME"},
	{name: "path traversal", args: []string{"--name=../escape"}, wantErr: invalidNameMessage},
	{name: "uppercase", args: []string{"--name=Demo"}, wantErr: invalidNameMessage},
	{name: "shell expression", args: []string{"--name=$(id)"}, wantErr: invalidNameMessage},
	{name: "newline", args: []string{"--name=demo\n"}, wantErr: invalidNameMessage},
	{name: "nul byte", args: []string{"--name=demo\x00"}, wantErr: invalidNameMessage},
	{name: "too long", args: []string{"--name", strings.Repeat("a", 33)}, wantErr: invalidNameMessage},
	{name: "help", args: []string{"--help"}, wantErr: flag.ErrHelp.Error()},
}

func TestCreateAcceptsGitIdentityFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--name=demo", "--with-git-config"},
		{"--with-git-config", "--name=demo"},
	} {
		if opts, err := parseCreateArgs(args); err != nil || !opts.withGitConfig {
			t.Fatalf("identity opt-in rejected: %v", err)
		}
	}
}

func TestCreatePropagatesGitIdentitySelection(t *testing.T) {
	for _, selected := range []bool{false, true} {
		args := []string{"--name=demo", fmt.Sprintf("--with-git-config=%t", selected)}
		err := runCreate(t.Context(), args, io.Discard, func(_ context.Context, name string, options vm.CreateOptions) (*vm.Project, error) {
			if name != "demo" || options.WithGitConfig != selected {
				t.Fatalf("selection changed: %q %+v", name, options)
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunCreateSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range createArgCases {
		if tc.wantErr != "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			assertCreateSuccess(t, tc.args, tc.wantName)
		})
	}
}

func TestRunCreateProjectSuccess(t *testing.T) {
	t.Parallel()
	project := &vm.Project{
		Branch:    "feature/new-project",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		Workspace: "/home/hk/workspace",
	}
	var out bytes.Buffer
	calls := 0
	err := runCreate(
		t.Context(),
		[]string{"--name=demo"},
		&out,
		func(context.Context, string, vm.CreateOptions) (*vm.Project, error) {
			calls++
			if out.Len() != 0 {
				t.Fatal("printed success before project creation finished")
			}
			return project, nil
		},
	)
	if err != nil || calls != 1 {
		t.Fatalf("create error=%v, calls=%d", err, calls)
	}
	want := "Created demo with branch feature/new-project at 0123456789ab in /home/hk/workspace. Enter with: hk attach demo\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestRunCreateFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("boot failed")
	var out bytes.Buffer
	var received context.Context
	err := runCreate(
		t.Context(),
		[]string{"--name=demo"},
		&out,
		func(ctx context.Context, _ string, _ vm.CreateOptions) (*vm.Project, error) {
			received = ctx
			return nil, want
		},
	)
	if !errors.Is(err, want) || out.Len() != 0 {
		t.Fatalf("error=%v, output=%q; want original error and no success message", err, out.String())
	}
	if received == nil || !errors.Is(received.Err(), context.Canceled) {
		t.Fatal("creation context was not released after failure")
	}
}

func TestRunCreateInvalidArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range createArgCases {
		if tc.wantErr == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runCreate(
				t.Context(),
				tc.args,
				&out,
				func(context.Context, string, vm.CreateOptions) (*vm.Project, error) {
					t.Fatal("invalid arguments reached VM creation")
					return nil, nil
				},
			)
			assertErrorMessage(t, err, tc.wantErr)
			if out.Len() != 0 {
				t.Fatalf("invalid create wrote to stdout: %q", out.String())
			}
		})
	}
}

func TestRunCreateCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := runCreate(
		ctx,
		[]string{"--name=demo"},
		&out,
		func(ctx context.Context, _ string, _ vm.CreateOptions) (*vm.Project, error) { return nil, ctx.Err() },
	)
	if !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("error=%v, output=%q; want cancellation without success", err, out.String())
	}
}

func TestRunCreateDeadline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parentTimeout time.Duration
		want          time.Duration
	}{
		{name: "default deadline", want: 90 * time.Second},
		{name: "earlier parent deadline", parentTimeout: time.Second, want: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.parentTimeout != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.parentTimeout)
					defer cancel()
				}
				start := time.Now()
				var out bytes.Buffer
				err := runCreate(
					ctx,
					[]string{"--name=demo"},
					&out,
					func(ctx context.Context, _ string, _ vm.CreateOptions) (*vm.Project, error) {
						deadline, ok := ctx.Deadline()
						if !ok || deadline.Sub(start) != tc.want {
							t.Fatalf("deadline = %v, want %v after start", deadline, tc.want)
						}
						<-ctx.Done()
						return nil, ctx.Err()
					},
				)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != tc.want || out.Len() != 0 {
					t.Fatalf("timeout error=%v, elapsed=%v, output=%q", err, time.Since(start), out.String())
				}
			})
		})
	}
}

func TestRunCreateOutputFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("output unavailable")
	for _, tc := range []struct {
		name    string
		project *vm.Project
	}{
		{name: "empty VM"},
		{
			name: "project VM",
			project: &vm.Project{
				Branch:    "main",
				Commit:    "0123456789abcdef0123456789abcdef01234567",
				Workspace: "/home/hk/workspace",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runCreate(
				t.Context(),
				[]string{"--name=demo"},
				errorWriter{err: want},
				func(context.Context, string, vm.CreateOptions) (*vm.Project, error) { return tc.project, nil },
			)
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestParseCreateArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range createArgCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCreateArgs(tc.args)
			assertErrorMessage(t, err, tc.wantErr)
			if got.name != tc.wantName {
				t.Fatalf("name = %q, want %q", got.name, tc.wantName)
			}
			if tc.name == "help" && !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("help lost its error identity: %v", err)
			}
		})
	}
}

func FuzzCreateArgs(f *testing.F) {
	for _, seed := range []struct {
		args string
		name string
	}{
		{args: "--name\x00demo", name: "demo"},
		{args: "--name=demo", name: "a-2"},
		{args: "--name=first\x00--name=second", name: ""},
		{args: "--name", name: "../escape"},
		{args: "--help", name: "demo\x00extra"},
		{args: "--name=demo\x00--\x00extra", name: strings.Repeat("a", 32)},
	} {
		f.Add(seed.args, seed.name)
	}
	f.Fuzz(func(t *testing.T, encoded, name string) {
		args := strings.Split(encoded, "\x00")
		opts, err := parseCreateArgs(args)
		if err == nil {
			if vm.ValidateName(opts.name) != nil {
				t.Fatalf("accepted invalid name %q from %q", opts.name, args)
			}
			assertCreateSuccess(t, args, opts.name)
			roundTrip, err := parseCreateArgs([]string{"--name=" + opts.name, fmt.Sprintf("--with-git-config=%t", opts.withGitConfig)})
			if err != nil || roundTrip != opts {
				t.Fatalf("normalized arguments changed options: %+v, %v", roundTrip, err)
			}
			if _, err := parseCreateArgs(append([]string{"unexpected"}, args...)); err == nil {
				t.Fatal("accepted an unexpected positional argument")
			}
		} else {
			if opts != (createOptions{}) {
				t.Fatalf("invalid arguments returned usable options: %+v", opts)
			}
			handlerErr := runCreate(
				t.Context(),
				args,
				io.Discard,
				func(context.Context, string, vm.CreateOptions) (*vm.Project, error) {
					t.Fatal("invalid arguments reached VM creation")
					return nil, nil
				},
			)
			assertErrorMessage(t, handlerErr, err.Error())
		}

		wantValid := vm.ValidateName(name) == nil
		for _, args := range [][]string{{"--name", name}, {"--name=" + name}, {"-name", name}} {
			opts, err := parseCreateArgs(args)
			if (err == nil) != wantValid || (err == nil && opts.name != name) {
				t.Fatalf("name spelling changed acceptance/value for %q: %+v, %v", args, opts, err)
			}
		}

		validName := validFuzzName(name)
		for _, args := range [][]string{{"--name", validName}, {"--name=" + validName}, {"-name", validName}} {
			assertCreateSuccess(t, args, validName)
		}
	})
}

func validFuzzName(name string) string {
	if vm.ValidateName(name) == nil {
		return name
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-"
	generated := []byte{'v'}
	for i := 0; i < len(name) && i < 31; i++ {
		generated = append(generated, alphabet[int(name[i])%len(alphabet)])
	}
	return string(generated)
}

func assertCreateSuccess(t *testing.T, args []string, wantName string) {
	t.Helper()
	var out bytes.Buffer
	var received context.Context
	calls := 0
	err := runCreate(
		t.Context(),
		args,
		&out,
		func(ctx context.Context, name string, _ vm.CreateOptions) (*vm.Project, error) {
			calls++
			received = ctx
			if name != wantName {
				t.Fatalf("create received name=%q, want %q", name, wantName)
			}
			if ctx == nil || ctx.Err() != nil {
				t.Fatal("create received no live context")
			}
			if out.Len() != 0 {
				t.Fatal("printed success before creation finished")
			}
			return nil, nil
		},
	)
	if err != nil || calls != 1 {
		t.Fatalf("create error=%v, calls=%d", err, calls)
	}
	if want := "Created empty VM " + wantName + ". Enter with: hk attach " + wantName + "\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	if !errors.Is(received.Err(), context.Canceled) {
		t.Fatal("creation context was not released after returning")
	}
}
