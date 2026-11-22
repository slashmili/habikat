package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/slashmili/habikat/internal/vm"
)

const invalidNameMessage = "name must be 1–32 lowercase letters, digits or hyphens, starting with a letter"

type vmCall struct {
	operation     string
	name          string
	force         bool
	all           bool
	withGitConfig bool
}

type recordingOperations struct {
	calls []vmCall
}

func (r *recordingOperations) operations() vmOperations {
	return vmOperations{
		create: func(_ context.Context, name string, options vm.CreateOptions) (*vm.Project, error) {
			r.calls = append(r.calls, vmCall{operation: "create", name: name, withGitConfig: options.WithGitConfig})
			return nil, nil
		},
		attach: func(name string) error {
			r.calls = append(r.calls, vmCall{operation: "attach", name: name})
			return nil
		},
		delete: func(_ context.Context, name string, confirm func() error) error {
			r.calls = append(r.calls, vmCall{operation: "delete", name: name, force: confirm == nil})
			return nil
		},
		fetch: func(_ context.Context, name string, confirm func(vm.GuestChanges) error) (*vm.FetchResult, error) {
			r.calls = append(r.calls, vmCall{operation: "fetch", name: name, force: confirm == nil})
			return &vm.FetchResult{}, nil
		},
		stop: func(_ context.Context, name string, force bool) error {
			r.calls = append(r.calls, vmCall{operation: "stop", name: name, force: force})
			return nil
		},
		start: func(_ context.Context, name string) (vm.StartResult, error) {
			r.calls = append(r.calls, vmCall{operation: "start", name: name})
			return vm.StartResult{}, nil
		},
		list: func(_ context.Context, all bool) ([]vm.Listing, error) {
			r.calls = append(r.calls, vmCall{operation: "list", all: all})
			return nil, nil
		},
		proxy: func(path string, _ io.Reader, _ io.Writer) error {
			r.calls = append(r.calls, vmCall{operation: "proxy", name: path})
			return nil
		},
	}
}

func execute(ops vmOperations, args ...string) (string, error) {
	var out bytes.Buffer
	err := run(args, strings.NewReader(""), &out, ops)
	return out.String(), err
}

func TestRootHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := execute(unexpectedVMOperations(t), args...)
			if err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"create", "attach", "fetch", "delete", "stop", "start", "list"} {
				if !strings.Contains(out, "\n  "+command+" ") {
					t.Fatalf("help omits %q: %s", command, out)
				}
			}
			if strings.Contains(out, "_proxy") || strings.Contains(out, "completion") {
				t.Fatalf("help lists an internal command: %s", out)
			}
		})
	}
}

func TestCommandHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{args: []string{"create", "--help"}, want: []string{"hk create --name NAME [--with-git-config]", "--with-git-config", "imports only user.name and user.email"}},
		{args: []string{"attach", "-h"}, want: []string{"hk attach NAME"}},
		{args: []string{"fetch", "--help"}, want: []string{"hk fetch [--force] NAME", "without changing your checkout", "--force skips only confirmation for uncommitted guest files", "Ignored files and files outside the repository are excluded"}},
		{args: []string{"delete", "--help"}, want: []string{"hk delete [--force] NAME", "--force"}},
		{args: []string{"stop", "--help"}, want: []string{"hk stop [--force] NAME", "buffered guest data may be lost"}},
		{args: []string{"start", "--help"}, want: []string{"hk start NAME", "existing disk and keys"}},
		{args: []string{"list", "--help"}, want: []string{"hk list [-a|--all]", "-a, --all"}},
		{args: []string{"help", "stop"}, want: []string{"hk stop [--force] NAME"}},
		{args: []string{"delete", "demo", "--help"}, want: []string{"hk delete [--force] NAME"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := execute(unexpectedVMOperations(t), tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range tc.want {
				if !strings.Contains(out, text) {
					t.Fatalf("help omits %q: %s", text, out)
				}
			}
		})
	}
}

func TestHelpOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	for _, args := range [][]string{{"--help"}, {"stop", "--help"}} {
		if err := run(args, strings.NewReader(""), errorWriter{err: want}, unexpectedVMOperations(t)); !errors.Is(err, want) {
			t.Fatalf("%q error = %v, want %v", args, err, want)
		}
	}
}

func TestCommandDispatch(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		want       vmCall
		wantOutput string
	}{
		{args: []string{"create", "--name", "alpha-7"}, want: vmCall{operation: "create", name: "alpha-7"}, wantOutput: "Created empty VM alpha-7. Enter with: hk attach alpha-7\n"},
		{args: []string{"create", "--name=beta-8", "--with-git-config"}, want: vmCall{operation: "create", name: "beta-8", withGitConfig: true}},
		{args: []string{"create", "--with-git-config", "--name", "beta-8"}, want: vmCall{operation: "create", name: "beta-8", withGitConfig: true}},
		{args: []string{"create", "--name=demo", "--with-git-config=false"}, want: vmCall{operation: "create", name: "demo"}},
		{args: []string{"create", "--name=first", "--name=second"}, want: vmCall{operation: "create", name: "second"}},
		{args: []string{"create", "--name=demo", "--"}, want: vmCall{operation: "create", name: "demo"}},
		{args: []string{"attach", "shell-9"}, want: vmCall{operation: "attach", name: "shell-9"}},
		{args: []string{"attach", "--", "shell-9"}, want: vmCall{operation: "attach", name: "shell-9"}},
		{args: []string{"delete", "old-3", "--force"}, want: vmCall{operation: "delete", name: "old-3", force: true}, wantOutput: "Deleted old-3.\n"},
		{args: []string{"delete", "--force", "old-3"}, want: vmCall{operation: "delete", name: "old-3", force: true}},
		{args: []string{"delete", "old-3"}, want: vmCall{operation: "delete", name: "old-3"}},
		{args: []string{"delete", "--force=false", "old-3"}, want: vmCall{operation: "delete", name: "old-3"}},
		{args: []string{"fetch", "demo"}, want: vmCall{operation: "fetch", name: "demo"}},
		{args: []string{"fetch", "--force", "demo"}, want: vmCall{operation: "fetch", name: "demo", force: true}},
		{args: []string{"fetch", "demo", "--force"}, want: vmCall{operation: "fetch", name: "demo", force: true}},
		{args: []string{"stop", "vm"}, want: vmCall{operation: "stop", name: "vm"}, wantOutput: "Stopped vm.\n"},
		{args: []string{"stop", "--force", "vm"}, want: vmCall{operation: "stop", name: "vm", force: true}},
		{args: []string{"stop", "vm", "--force"}, want: vmCall{operation: "stop", name: "vm", force: true}},
		{args: []string{"start", "vm"}, want: vmCall{operation: "start", name: "vm"}, wantOutput: "Started vm.\n"},
		{args: []string{"list"}, want: vmCall{operation: "list"}, wantOutput: "No VMs found.\n"},
		{args: []string{"list", "-a"}, want: vmCall{operation: "list", all: true}},
		{args: []string{"list", "--all"}, want: vmCall{operation: "list", all: true}},
		{args: []string{"list", "-a", "--all"}, want: vmCall{operation: "list", all: true}},
		{args: []string{"_proxy", "private directory/vsock.sock"}, want: vmCall{operation: "proxy", name: "private directory/vsock.sock"}},
		{args: []string{"_proxy", "--help"}, want: vmCall{operation: "proxy", name: "--help"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var recorder recordingOperations
			out, err := execute(recorder.operations(), tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(recorder.calls) != 1 || recorder.calls[0] != tc.want {
				t.Fatalf("calls = %+v, want %+v", recorder.calls, tc.want)
			}
			if tc.wantOutput != "" && out != tc.wantOutput {
				t.Fatalf("output = %q, want %q", out, tc.wantOutput)
			}
		})
	}
}

func TestInvalidArgumentsHaveNoEffect(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{want: "missing command"},
		{args: []string{"unknown"}, want: `unknown command "unknown"`},
		{args: []string{""}, want: `unknown command ""`},
		{args: []string{"--bogus"}, want: "unknown flag: --bogus"},
		{args: []string{"create"}, want: "missing required --name; usage: hk create --name NAME [--with-git-config]"},
		{args: []string{"create", "--name="}, want: "missing required --name"},
		{args: []string{"create", "--name"}, want: "flag needs an argument: --name; usage: hk create"},
		{args: []string{"create", "--unknown"}, want: "unknown flag: --unknown"},
		{args: []string{"create", "-name", "demo"}, want: "unknown shorthand flag: 'n' in -name"},
		{args: []string{"create", "demo"}, want: `unexpected argument "demo"; usage: hk create`},
		{args: []string{"create", "--name=demo", "extra"}, want: `unexpected argument "extra"`},
		{args: []string{"create", "--name=demo", "--", "extra"}, want: `unexpected argument "extra"`},
		{args: []string{"create", "--name", "../escape"}, want: invalidNameMessage},
		{args: []string{"create", "--name=demo", "--with-git-config=maybe"}, want: `invalid argument "maybe" for "--with-git-config"`},
		{args: []string{"attach"}, want: "expected exactly one VM name, got 0 arguments; usage: hk attach NAME"},
		{args: []string{"attach", "demo", "extra"}, want: "expected exactly one VM name, got 2 arguments"},
		{args: []string{"attach", "../escape"}, want: invalidNameMessage},
		{args: []string{"attach", "--name=demo"}, want: "unknown flag: --name"},
		{args: []string{"delete"}, want: "expected exactly one VM name, got 0 arguments; usage: hk delete [--force] NAME"},
		{args: []string{"delete", "--force"}, want: "expected exactly one VM name, got 0 arguments"},
		{args: []string{"delete", "demo", "other"}, want: "expected exactly one VM name, got 2 arguments"},
		{args: []string{"delete", "--force=yes", "demo"}, want: `invalid argument "yes" for "--force"`},
		{args: []string{"delete", "", "--force"}, want: invalidNameMessage},
		{args: []string{"fetch"}, want: "usage: hk fetch [--force] NAME"},
		{args: []string{"fetch", "--bogus", "demo"}, want: "unknown flag: --bogus"},
		{args: []string{"fetch", "Demo"}, want: invalidNameMessage},
		{args: []string{"stop", "--unknown", "vm"}, want: "unknown flag: --unknown; usage: hk stop [--force] NAME"},
		{args: []string{"stop", "", "vm"}, want: "expected exactly one VM name, got 2 arguments"},
		{args: []string{"stop", "../vm"}, want: invalidNameMessage},
		{args: []string{"start", "--force", "vm"}, want: "unknown flag: --force; usage: hk start NAME"},
		{args: []string{"start", "vm", "other"}, want: "expected exactly one VM name"},
		{args: []string{"list", "alpha"}, want: `unexpected argument "alpha"; usage: hk list [-a|--all]`},
		{args: []string{"list", "--json"}, want: "unknown flag: --json"},
		{args: []string{"list", "--all=maybe"}, want: `invalid argument "maybe" for "-a, --all"`},
		{args: []string{"_proxy"}, want: "expected exactly one socket path, got 0 arguments"},
		{args: []string{"_proxy", "socket", "extra"}, want: "expected exactly one socket path, got 2 arguments"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := execute(unexpectedVMOperations(t), tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if out != "" {
				t.Fatalf("invalid command wrote to stdout: %q", out)
			}
		})
	}
}

func TestInvalidInputErrorsAreTerminalSafe(t *testing.T) {
	for _, args := range [][]string{
		{"list", "bad\n\x1b[2J"},
		{"list", "--bad\n\x1b[2J"},
		{"stop", "--force=\x1b[2J", "vm"},
		{"bad\n\x1b[2J"},
		{"--bad‮"},
	} {
		_, err := execute(unexpectedVMOperations(t), args...)
		if err == nil {
			t.Fatalf("accepted %q", args)
		}
		assertNoControl(t, err.Error(), false)
	}
}

func TestBackendErrorsAreReturnedWithoutUsage(t *testing.T) {
	backendErr := errors.New("backend failed")
	ops := vmOperations{
		create: func(context.Context, string, vm.CreateOptions) (*vm.Project, error) { return nil, backendErr },
		attach: func(string) error { return backendErr },
		delete: func(context.Context, string, func() error) error { return backendErr },
		fetch: func(context.Context, string, func(vm.GuestChanges) error) (*vm.FetchResult, error) {
			return nil, backendErr
		},
		stop:  func(context.Context, string, bool) error { return backendErr },
		start: func(context.Context, string) (vm.StartResult, error) { return vm.StartResult{}, backendErr },
		proxy: func(string, io.Reader, io.Writer) error { return backendErr },
	}
	for _, args := range [][]string{
		{"create", "--name=demo"},
		{"attach", "demo"},
		{"delete", "--force", "demo"},
		{"fetch", "--force", "demo"},
		{"stop", "demo"},
		{"start", "demo"},
		{"_proxy", "vsock.sock"},
	} {
		t.Run(args[0], func(t *testing.T) {
			out, err := execute(ops, args...)
			if err != backendErr {
				t.Fatalf("error = %v, want the backend error unchanged", err)
			}
			if out != "" {
				t.Fatalf("failed command wrote to stdout: %q", out)
			}
		})
	}
}

func TestRepeatedExecutionDoesNotLeakFlags(t *testing.T) {
	var recorder recordingOperations
	ops := recorder.operations()
	for _, args := range [][]string{
		{"create", "--name=first", "--with-git-config"},
		{"create", "--name=second"},
		{"delete", "--force", "first"},
		{"delete", "second"},
		{"stop", "--force", "first"},
		{"stop", "second"},
		{"list", "--all"},
		{"list"},
	} {
		if _, err := execute(ops, args...); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
	}
	want := []vmCall{
		{operation: "create", name: "first", withGitConfig: true},
		{operation: "create", name: "second"},
		{operation: "delete", name: "first", force: true},
		{operation: "delete", name: "second"},
		{operation: "stop", name: "first", force: true},
		{operation: "stop", name: "second"},
		{operation: "list", all: true},
		{operation: "list"},
	}
	if len(recorder.calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", recorder.calls, want)
	}
	for i := range want {
		if recorder.calls[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, recorder.calls[i], want[i])
		}
	}
}

func TestProxyTransport(t *testing.T) {
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
			err := run([]string{"_proxy", path}, strings.NewReader(request), &out, ops)
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

func FuzzCommandLine(f *testing.F) {
	for _, seed := range []struct{ args, name string }{
		{args: "create\x00--name\x00demo", name: "demo"},
		{args: "create\x00--name=demo\x00--with-git-config", name: "a-2"},
		{args: "create\x00--name=demo\x00--with-git-config\x00--with-git-config=false", name: "demo"},
		{args: "create\x00-name\x00demo", name: "-h"},
		{args: "delete\x00--force\x00demo", name: "--"},
		{args: "delete\x00--force=false\x00demo", name: "../escape"},
		{args: "delete\x00--force\x00--force=false\x00demo", name: "demo"},
		{args: "fetch\x00--force=false\x00demo\x00--force=1", name: "demo"},
		{args: "stop\x00--force=false\x00--\x00demo", name: "demo"},
		{args: "stop\x00--\x00demo\x00--force", name: "demo"},
		{args: "fetch\x00demo\x00--force\x00--force", name: ""},
		{args: "stop\x00--force\x00", name: "demo\x00extra"},
		{args: "start\x00--\x00-x", name: "Demo"},
		{args: "attach\x00--help", name: "a\n"},
		{args: "list\x00-a\x00--all=true", name: "--force"},
		{args: "list\x00-aa=false\x00--all", name: "demo"},
		{args: "list\x00--all\x00-a=false", name: "demo"},
		{args: "list\x00--\x00\x1b[2J", name: "-a"},
		{args: "_proxy\x00vsock.sock", name: "--name=demo"},
		{args: "_proxy", name: "a"},
		{args: "help\x00create", name: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{args: "", name: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	} {
		f.Add(seed.args, seed.name)
	}
	f.Fuzz(func(t *testing.T, encoded, name string) {
		if len(encoded) > 4096 || len(name) > 4096 {
			t.Skip()
		}
		args := strings.Split(encoded, "\x00")
		call, out, err := executeOnce(args)
		assertCommandLineContract(t, args, call, out, err)
		repeatCall, repeatOut, repeatErr := executeOnce(args)
		if !sameCall(repeatCall, call) || repeatOut != out || errorText(repeatErr) != errorText(err) {
			t.Fatalf("repeated execution of %q differs", args)
		}
		assertNameSpellings(t, name)
	})
}

func sameCall(a, b *vmCall) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func executeOnce(args []string) (*vmCall, string, error) {
	var recorder recordingOperations
	out, err := execute(recorder.operations(), args...)
	if len(recorder.calls) > 1 {
		panic("one command line performed several VM operations")
	}
	if len(recorder.calls) == 0 {
		return nil, out, err
	}
	return &recorder.calls[0], out, err
}

func assertCommandLineContract(t *testing.T, args []string, call *vmCall, out string, err error) {
	t.Helper()
	if err != nil {
		assertNoControl(t, err.Error(), false)
		if call != nil || out != "" {
			t.Fatalf("rejected %q after a side effect: call=%+v output=%q", args, call, out)
		}
		return
	}
	if call == nil {
		return
	}
	if call.operation == "proxy" {
		if len(args) != 2 || args[0] != "_proxy" || call.name != args[1] {
			t.Fatalf("proxy call %+v from %q", call, args)
		}
		return
	}
	if call.operation != "list" && (vm.ValidateName(call.name) != nil || !containsArgument(args, call.name)) {
		t.Fatalf("operation received name %q not validated from %q", call.name, args)
	}
	if !booleanOptionsMatch(args, *call) {
		t.Fatalf("incorrect boolean options in %+v for %q", call, args)
	}
}

func booleanOptionsMatch(args []string, call vmCall) bool {
	return call.force == booleanFlagValue(args, "--force") &&
		call.all == booleanFlagValue(args, "-a", "--all") &&
		call.withGitConfig == booleanFlagValue(args, "--with-git-config")
}

func TestBooleanOptionsRejectInventedAndDroppedFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want vmCall
	}{
		{name: "force enabled", args: []string{"delete", "demo", "--force"}, want: vmCall{force: true}},
		{name: "force disabled", args: []string{"delete", "--force", "--force=false", "demo"}},
		{name: "all enabled", args: []string{"list", "-aa"}, want: vmCall{all: true}},
		{name: "all disabled", args: []string{"list", "--all", "-aa=false"}},
		{name: "identity enabled", args: []string{"create", "--name=demo", "--with-git-config"}, want: vmCall{withGitConfig: true}},
		{name: "identity disabled", args: []string{"create", "--name=demo", "--with-git-config=false"}},
		{name: "flags after separator", args: []string{"stop", "--", "demo", "--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !booleanOptionsMatch(tc.args, tc.want) {
				t.Fatalf("rejected correct options %+v for %q", tc.want, tc.args)
			}
			for _, wrong := range []vmCall{
				{force: !tc.want.force, all: tc.want.all, withGitConfig: tc.want.withGitConfig},
				{force: tc.want.force, all: !tc.want.all, withGitConfig: tc.want.withGitConfig},
				{force: tc.want.force, all: tc.want.all, withGitConfig: !tc.want.withGitConfig},
			} {
				if booleanOptionsMatch(tc.args, wrong) {
					t.Fatalf("accepted incorrect options %+v for %q", wrong, tc.args)
				}
			}
		})
	}
}

func assertNameSpellings(t *testing.T, name string) {
	t.Helper()
	wantValid := vm.ValidateName(name) == nil
	for _, args := range [][]string{
		{"create", "--name", name},
		{"create", "--name=" + name},
		{"attach", name},
		{"delete", name},
		{"fetch", "--force", name},
		{"stop", name, "--force"},
		{"start", name},
	} {
		call, _, _ := executeOnce(args)
		if (call != nil) != wantValid || (call != nil && call.name != name) {
			t.Fatalf("%q: call=%+v, want valid=%t", args, call, wantValid)
		}
	}
	valid := validFuzzName(name)
	for _, args := range [][]string{{"create", "--name", valid}, {"delete", "--force", valid}, {"stop", valid}} {
		if call, _, err := executeOnce(args); err != nil || call == nil || call.name != valid {
			t.Fatalf("%q: call=%+v err=%v", args, call, err)
		}
	}
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

func containsArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want || strings.HasSuffix(arg, "="+want) {
			return true
		}
	}
	return false
}

func booleanFlagValue(args []string, names ...string) bool {
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		flag, text, explicit := strings.Cut(arg, "=")
		if !matchesBooleanFlag(flag, names) {
			continue
		}
		value = true
		if explicit {
			value, _ = strconv.ParseBool(text)
		}
	}
	return value
}

func matchesBooleanFlag(flag string, names []string) bool {
	if slices.Contains(names, flag) {
		return true
	}
	return slices.Contains(names, "-a") && strings.HasPrefix(flag, "-a") && strings.Trim(flag[1:], "a") == ""
}

func TestBooleanFlagValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		names []string
		want  bool
	}{
		{name: "absent", args: []string{"delete", "demo"}, names: []string{"--force"}},
		{name: "bare flag", args: []string{"delete", "demo", "--force"}, names: []string{"--force"}, want: true},
		{name: "explicit false", args: []string{"delete", "--force=false", "demo"}, names: []string{"--force"}},
		{name: "numeric true", args: []string{"stop", "--force=1", "demo"}, names: []string{"--force"}, want: true},
		{name: "last false wins", args: []string{"stop", "--force", "--force=false", "demo"}, names: []string{"--force"}},
		{name: "last true wins", args: []string{"fetch", "--force=false", "demo", "--force"}, names: []string{"--force"}, want: true},
		{name: "after separator", args: []string{"stop", "--", "--force"}, names: []string{"--force"}},
		{name: "separator preserves earlier value", args: []string{"stop", "--force", "--", "--force=false"}, names: []string{"--force"}, want: true},
		{name: "exact name", args: []string{"--forcefully"}, names: []string{"--force"}},
		{name: "short false", args: []string{"list", "-a=false"}, names: []string{"-a", "--all"}},
		{name: "short cluster", args: []string{"list", "-aa"}, names: []string{"-a", "--all"}, want: true},
		{name: "short cluster false", args: []string{"list", "-aa=false"}, names: []string{"-a", "--all"}},
		{name: "mixed aliases last wins", args: []string{"list", "--all", "-a=false"}, names: []string{"-a", "--all"}},
		{name: "git identity false", args: []string{"create", "--name=demo", "--with-git-config=false"}, names: []string{"--with-git-config"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := booleanFlagValue(tc.args, tc.names...); got != tc.want {
				t.Fatalf("booleanFlagValue(%q, %q) = %t, want %t", tc.args, tc.names, got, tc.want)
			}
		})
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func unexpectedVMOperations(t *testing.T) vmOperations {
	t.Helper()
	unexpected := func(operation, target string) {
		t.Helper()
		t.Fatalf("unexpected %s operation for %q", operation, target)
	}
	return vmOperations{
		create: func(_ context.Context, name string, _ vm.CreateOptions) (*vm.Project, error) {
			unexpected("create", name)
			return nil, nil
		},
		attach: func(name string) error {
			unexpected("attach", name)
			return nil
		},
		delete: func(_ context.Context, name string, _ func() error) error {
			unexpected("delete", name)
			return nil
		},
		fetch: func(_ context.Context, name string, _ func(vm.GuestChanges) error) (*vm.FetchResult, error) {
			unexpected("fetch", name)
			return nil, nil
		},
		stop: func(_ context.Context, name string, _ bool) error {
			unexpected("stop", name)
			return nil
		},
		start: func(_ context.Context, name string) (vm.StartResult, error) {
			unexpected("start", name)
			return vm.StartResult{}, nil
		},
		list: func(context.Context, bool) ([]vm.Listing, error) {
			unexpected("list", "")
			return nil, nil
		},
		proxy: func(path string, _ io.Reader, _ io.Writer) error {
			unexpected("proxy", path)
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
