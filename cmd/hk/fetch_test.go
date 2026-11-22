package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slashmili/habikat/internal/repository"
	"github.com/slashmili/habikat/internal/vm"
)

func TestFetchConfirmation(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "YeS\r\n", "", "\n", "no\n", "y", " y\n", "yes please\n", "y\x00\n", "yes\rgarbage\n", "\x03\n", strings.Repeat("y", 70) + "\n"} {
		t.Run(answer, func(t *testing.T) {
			var out bytes.Buffer
			err := confirmFetch(t.Context(), "demo", vm.GuestChanges{Staged: true, Unstaged: true, Untracked: true}, strings.NewReader(answer), &out)
			want := answer == "y\n" || answer == "Y\n" || answer == "yes\n" || answer == "YeS\r\n"
			if (err == nil) != want {
				t.Fatalf("approval %q = %v", answer, err)
			}
			if !strings.Contains(out.String(), "Guest demo has uncommitted changes. Fetch committed branches only and leave these files in the VM? [y/N]") {
				t.Fatal("missing explicit fetch scope in prompt")
			}
		})
	}
	if err := confirmFetch(t.Context(), "demo", vm.GuestChanges{}, &approvalErrorReader{}, &bytes.Buffer{}); err == nil {
		t.Fatal("input error authorized fetch")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := confirmFetch(ctx, "demo", vm.GuestChanges{}, strings.NewReader("yes\n"), &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled input = %v", err)
	}
}

func TestFetchRequiresTerminalOrForceForDirtyGuest(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		dirty, force bool
	}{
		{name: "clean", args: []string{"fetch", "demo"}},
		{name: "dirty pipe", args: []string{"fetch", "demo"}, dirty: true},
		{name: "forced", args: []string{"fetch", "demo", "--force"}, dirty: true, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			published := false
			ops := unexpectedVMOperations(t)
			ops.fetch = func(_ context.Context, name string, confirm func(vm.GuestChanges) error) (*vm.FetchResult, error) {
				if tc.dirty && confirm != nil {
					if err := confirm(vm.GuestChanges{Untracked: true}); err != nil {
						return nil, err
					}
				}
				published = true
				return &vm.FetchResult{Branches: []repository.FetchedBranch{{Name: "feature", Commit: strings.Repeat("a", 40), Ref: "refs/remotes/hk/instance/feature"}}}, nil
			}
			var out bytes.Buffer
			err := run(tc.args, strings.NewReader("yes\n"), &out, ops)
			if tc.dirty && !tc.force {
				if err == nil || published || !strings.Contains(err.Error(), "--force") {
					t.Fatalf("pipe approved fetch: %v", err)
				}
				return
			}
			if err != nil || !published || !strings.Contains(out.String(), "refs/remotes/hk/instance/feature") || !strings.Contains(out.String(), "Only committed branches") {
				t.Fatalf("fetch=%v published=%v output=%s", err, published, &out)
			}
		})
	}
}

func TestFetchCancelledWithoutSuccess(t *testing.T) {
	for _, failure := range []error{errFetchCancelled, context.Canceled} {
		var out bytes.Buffer
		err := runFetch(t.Context(), strings.NewReader(""), &out, "demo", false, func(context.Context, string, func(vm.GuestChanges) error) (*vm.FetchResult, error) {
			return nil, failure
		})
		if err != nil || out.String() != "Fetch from demo cancelled.\n" {
			t.Fatalf("cancellation = %v, %q", err, out.String())
		}
	}
}

func FuzzFetchApproval(f *testing.F) {
	for _, seed := range []string{"y\n", "YES\r\n", "", "y", "yes please\n", "\x03\n", " y\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, answer string) {
		err := confirmFetch(t.Context(), "demo", vm.GuestChanges{}, strings.NewReader(answer), &bytes.Buffer{})
		if err == nil {
			line, _, _ := strings.Cut(answer, "\n")
			line = strings.ToLower(strings.TrimSuffix(line, "\r"))
			if line != "y" && line != "yes" {
				t.Fatalf("ambiguous approval accepted %q", answer)
			}
		}
	})
}
