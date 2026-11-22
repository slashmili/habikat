package main

import (
	"errors"
	"slices"
	"testing"
)

func TestAttachReleasesSignalsBeforeAttaching(t *testing.T) {
	backendErr := errors.New("SSH connection failed")
	for _, want := range []error{nil, backendErr} {
		events := []string{}
		cmd := newAttachCommand(func(name string) error {
			events = append(events, "attach "+name)
			return want
		}, func() {
			events = append(events, "release signals")
		})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs([]string{"demo"})
		if err := cmd.Execute(); !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
		if wantEvents := []string{"release signals", "attach demo"}; !slices.Equal(events, wantEvents) {
			t.Fatalf("handoff = %q, want %q", events, wantEvents)
		}
	}
}

func TestInvalidAttachKeepsSignalHandling(t *testing.T) {
	cmd := newAttachCommand(func(string) error {
		t.Fatal("invalid arguments reached VM attachment")
		return nil
	}, func() {
		t.Fatal("invalid arguments changed signal handling")
	})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"../escape"})
	assertErrorMessage(t, cmd.Execute(), invalidNameMessage)
}
