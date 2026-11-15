package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/slashmili/habikato/internal/vm"
)

var nameArgCases = []struct {
	name     string
	args     []string
	wantName string
	wantErr  string
}{
	{name: "valid name", args: []string{"demo-2"}, wantName: "demo-2"},
	{name: "maximum length", args: []string{strings.Repeat("a", 32)}, wantName: strings.Repeat("a", 32)},
	{name: "missing name", wantErr: usage},
	{name: "extra argument", args: []string{"demo", "extra"}, wantErr: usage},
	{name: "empty name", args: []string{""}, wantErr: invalidNameMessage},
	{name: "path traversal", args: []string{"../escape"}, wantErr: invalidNameMessage},
	{name: "absolute path", args: []string{"/tmp/demo"}, wantErr: invalidNameMessage},
	{name: "flag instead of name", args: []string{"--name=demo"}, wantErr: invalidNameMessage},
	{name: "uppercase name", args: []string{"Demo"}, wantErr: invalidNameMessage},
	{name: "nul byte", args: []string{"demo\x00"}, wantErr: invalidNameMessage},
	{name: "too long", args: []string{strings.Repeat("a", 33)}, wantErr: invalidNameMessage},
}

func TestParseNameArg(t *testing.T) {
	t.Parallel()
	for _, tc := range nameArgCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNameArg(tc.args)
			assertErrorMessage(t, err, tc.wantErr)
			if got != tc.wantName {
				t.Fatalf("name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

func FuzzNameArg(f *testing.F) {
	for _, args := range []string{"demo", "a-2", "", "demo\x00extra", "../escape", "--help", "a\n", strings.Repeat("a", 32)} {
		f.Add(args)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		args := strings.Split(encoded, "\x00")
		name, parseErr := parseNameArg(args)
		wantValid := len(args) == 1 && vm.ValidateName(args[0]) == nil
		if (parseErr == nil) != wantValid {
			t.Fatalf("acceptance differs from the name/arity contract: %q, %v", args, parseErr)
		}
		if parseErr != nil && name != "" {
			t.Fatalf("invalid arguments returned a usable name: %q", name)
		}
		if wantValid && name != args[0] {
			t.Fatalf("parser changed name %q to %q", args[0], name)
		}

		stops, attaches, deletes := 0, 0, 0
		attachErr := runAttach(args, func() { stops++ }, func(got string) error {
			attaches++
			if got != name || stops != 1 {
				t.Fatalf("attach name/signal handoff: name=%q, stops=%d", got, stops)
			}
			return nil
		})
		var out bytes.Buffer
		deleteErr := runDelete(
			t.Context(),
			args,
			strings.NewReader(""),
			&out,
			func(_ context.Context, got string, _ func() error) error {
				deletes++
				if got != name {
					t.Fatalf("delete name = %q, want %q", got, name)
				}
				return nil
			},
		)
		if wantValid {
			if attachErr != nil || deleteErr != nil || stops != 1 || attaches != 1 || deletes != 1 {
				t.Fatalf("valid handlers: attach=%v, delete=%v, calls=%d/%d/%d", attachErr, deleteErr, stops, attaches, deletes)
			}
			if want := fmt.Sprintf("Deleted %s.\n", name); out.String() != want {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
		} else {
			assertErrorMessage(t, attachErr, parseErr.Error())
			assertErrorMessage(t, deleteErr, parseErr.Error())
			if stops != 0 || attaches != 0 || deletes != 0 || out.Len() != 0 {
				t.Fatal("invalid arguments caused a side effect")
			}
		}
	})
}
