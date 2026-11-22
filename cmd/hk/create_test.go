package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/slashmili/habikat/internal/vm"
)

func TestCreateReportsProject(t *testing.T) {
	project := &vm.Project{
		Branch:    "feature/new-project",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		Workspace: "/home/hk/workspace",
	}
	var out bytes.Buffer
	err := runCreate(t.Context(), &out, "demo", vm.CreateOptions{}, func(context.Context, string, vm.CreateOptions) (*vm.Project, error) {
		if out.Len() != 0 {
			t.Fatal("printed success before project creation finished")
		}
		return project, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Created demo with branch feature/new-project at 0123456789ab in /home/hk/workspace. Enter with: hk attach demo\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestCreateFailureReleasesContext(t *testing.T) {
	want := errors.New("boot failed")
	var out bytes.Buffer
	var received context.Context
	err := runCreate(t.Context(), &out, "demo", vm.CreateOptions{}, func(ctx context.Context, _ string, _ vm.CreateOptions) (*vm.Project, error) {
		received = ctx
		return nil, want
	})
	if !errors.Is(err, want) || out.Len() != 0 {
		t.Fatalf("error=%v, output=%q; want original error and no success message", err, out.String())
	}
	if received == nil || !errors.Is(received.Err(), context.Canceled) {
		t.Fatal("creation context was not released after failure")
	}
}

func TestCreateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := runCreate(ctx, &out, "demo", vm.CreateOptions{}, func(ctx context.Context, _ string, _ vm.CreateOptions) (*vm.Project, error) {
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("error=%v, output=%q; want cancellation without success", err, out.String())
	}
}

func TestCreateDeadline(t *testing.T) {
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
				err := runCreate(ctx, &out, "demo", vm.CreateOptions{}, func(ctx context.Context, _ string, _ vm.CreateOptions) (*vm.Project, error) {
					deadline, ok := ctx.Deadline()
					if !ok || deadline.Sub(start) != tc.want {
						t.Fatalf("deadline = %v, want %v after start", deadline, tc.want)
					}
					<-ctx.Done()
					return nil, ctx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != tc.want || out.Len() != 0 {
					t.Fatalf("timeout error=%v, elapsed=%v, output=%q", err, time.Since(start), out.String())
				}
			})
		})
	}
}

func TestCreateOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	for _, project := range []*vm.Project{nil, {Branch: "main", Commit: "0123456789abcdef", Workspace: "/home/hk/workspace"}} {
		err := runCreate(t.Context(), errorWriter{err: want}, "demo", vm.CreateOptions{}, func(context.Context, string, vm.CreateOptions) (*vm.Project, error) {
			return project, nil
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	}
}
