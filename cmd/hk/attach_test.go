package main

import (
	"errors"
	"slices"
	"testing"
)

func TestRunAttach(t *testing.T) {
	t.Parallel()
	backendErr := errors.New("SSH connection failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "failure", err: backendErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			err := runAttach([]string{"demo"}, func() {
				events = append(events, "stop signals")
			}, func(name string) error {
				events = append(events, "attach "+name)
				return tc.err
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if want := []string{"stop signals", "attach demo"}; !slices.Equal(events, want) {
				t.Fatalf("handoff = %q, want %q", events, want)
			}
		})
	}
}

func TestRunAttachInvalidArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range nameArgCases {
		if tc.wantErr == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			err := runAttach(tc.args, func() {
				t.Fatal("invalid arguments changed signal handling")
			}, func(string) error {
				t.Fatal("invalid arguments reached VM attachment")
				return nil
			})
			assertErrorMessage(t, err, tc.wantErr)
		})
	}
}
