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

func TestFetchArguments(t *testing.T) {
	for _, args := range [][]string{{"demo"}, {"demo", "--force"}, {"--force", "demo"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got, err := parseFetchArgs(args)
			if err != nil || got.name != "demo" || got.force != (len(args) == 2) {
				t.Fatalf("arguments = %+v, %v", got, err)
			}
		})
	}
	for _, args := range [][]string{nil, {"demo", "other"}, {"--force"}, {"demo", "--force", "--force"}, {"--force=true", "demo"}, {"../escape"}, {"Demo"}, {"--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseFetchArgs(args); err == nil {
				t.Fatalf("accepted malformed arguments %v", args)
			}
		})
	}
}

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

func TestRunFetch(t *testing.T) {
	for _, tc := range []struct {
		name         string
		force, dirty bool
	}{
		{name: "clean"}, {name: "dirty pipe", dirty: true}, {name: "forced", force: true, dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"demo"}
			if tc.force {
				args = append(args, "--force")
			}
			var out bytes.Buffer
			published := false
			err := runFetch(t.Context(), args, strings.NewReader("yes\n"), &out, func(_ context.Context, name string, confirm func(vm.GuestChanges) error) (*vm.FetchResult, error) {
				if name != "demo" {
					t.Fatal("wrong VM selected")
				}
				if tc.dirty && confirm != nil {
					if err := confirm(vm.GuestChanges{Untracked: true}); err != nil {
						return nil, err
					}
				}
				published = true
				return &vm.FetchResult{Branches: []repository.FetchedBranch{{Name: "feature", Commit: strings.Repeat("a", 40), Ref: "refs/remotes/hk/instance/feature"}}}, nil
			})
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
		err := runFetch(t.Context(), []string{"demo"}, strings.NewReader(""), &out, func(context.Context, string, func(vm.GuestChanges) error) (*vm.FetchResult, error) {
			return nil, failure
		})
		if err != nil || out.String() != "Fetch from demo cancelled.\n" {
			t.Fatalf("cancellation = %v, %q", err, out.String())
		}
	}
}

func FuzzFetchArgs(f *testing.F) {
	for _, input := range []string{"demo", "--force demo", "demo --force", "../escape", "--force --force demo", "\x00"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		args := strings.Split(input, " ")
		opts, err := parseFetchArgs(args)
		if err == nil {
			if err := vm.ValidateName(opts.name); err != nil {
				t.Fatal(err)
			}
			if len(args) != 1 && len(args) != 2 {
				t.Fatal("accepted extra arguments")
			}
			if opts.force && args[0] != "--force" && args[1] != "--force" {
				t.Fatal("invented force")
			}
		}
	})
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
