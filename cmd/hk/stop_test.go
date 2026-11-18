package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slashmili/habikat/internal/vm"
)

func TestStopCommand(t *testing.T) {
	for _, args := range [][]string{{"vm"}, {"--force", "vm"}, {"vm", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := runStop(t.Context(), args, &out, func(ctx context.Context, name string, force bool) error {
				calls++
				if name != "vm" || force != (len(args) == 2) {
					t.Fatalf("stop(%q, %v)", name, force)
				}
				return nil
			})
			if err != nil || calls != 1 || !strings.Contains(out.String(), "Stopped vm") {
				t.Fatalf("%v, %d, %q", err, calls, out.String())
			}
			if len(args) == 2 && !strings.Contains(out.String(), "buffered guest data may be lost") {
				t.Fatal("missing force warning")
			}
		})
	}
}

func TestStopRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"--force"}, {"vm", "other"}, {"vm", "--force", "--force"}, {"../vm"}, {"--unknown", "vm"}} {
		err := runStop(t.Context(), args, &bytes.Buffer{}, func(context.Context, string, bool) error { t.Fatal("stop called"); return nil })
		if err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestStopReportsFailure(t *testing.T) {
	var out bytes.Buffer
	failure := errors.New("shutdown timed out")
	err := runStop(t.Context(), []string{"vm"}, &out, func(context.Context, string, bool) error { return failure })
	if !errors.Is(err, failure) || strings.Contains(out.String(), "Stopped") {
		t.Fatalf("%v, %q", err, out.String())
	}
}

func FuzzStopArgs(f *testing.F) {
	for _, s := range []string{"vm", "--force vm", "vm --force", "", "../vm", "vm --force --force", "--force --force", "--foo vm", "vm other", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			t.Skip()
		}
		args := strings.Split(input, " ")
		name, force, valid := expectedStopCall(args)
		called := false
		err := runStop(t.Context(), args, &bytes.Buffer{}, func(_ context.Context, gotName string, gotForce bool) error {
			called = true
			if !valid || gotName != name || gotForce != force {
				t.Fatalf("stop(%q, %t) for %q", gotName, gotForce, args)
			}
			return nil
		})
		if valid != (err == nil) || valid != called {
			t.Fatalf("args %q: valid=%t called=%t err=%v", args, valid, called, err)
		}
	})
}

func expectedStopCall(args []string) (name string, force, valid bool) {
	switch {
	case len(args) == 1:
		name = args[0]
	case len(args) == 2 && args[0] == "--force":
		name, force = args[1], true
	case len(args) == 2 && args[1] == "--force":
		name, force = args[0], true
	default:
		return "", false, false
	}
	return name, force, vm.ValidateName(name) == nil
}

func TestStopDispatch(t *testing.T) {
	calls := 0
	err := run([]string{"stop", "vm"}, strings.NewReader(""), &bytes.Buffer{}, vmOperations{stop: func(context.Context, string, bool) error { calls++; return nil }})
	if err != nil || calls != 1 {
		t.Fatalf("stop dispatch: %v, %d", err, calls)
	}
}

func TestStopRejectsUnknownOption(t *testing.T) {
	err := runStop(t.Context(), []string{"--foo", "vm"}, &bytes.Buffer{}, func(context.Context, string, bool) error { return nil })
	if err == nil || !strings.Contains(err.Error(), `unknown option "--foo"`) {
		t.Fatalf("unknown option: %v", err)
	}
}

func TestStopRejectsEmptyExtraArgument(t *testing.T) {
	err := runStop(t.Context(), []string{"", "vm"}, &bytes.Buffer{}, func(context.Context, string, bool) error { return nil })
	if err == nil {
		t.Fatal("accepted empty extra argument")
	}
}

func TestStopHelpWarnsAboutForcedShutdown(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--help"}, strings.NewReader(""), &out, vmOperations{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"hk stop --force", "buffered guest data may be lost", "hk fetch --force"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("help omits %q: %s", text, out.String())
		}
	}
}
