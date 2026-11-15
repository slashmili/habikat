package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "/must/not/read")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
	t.Setenv("GIT_CONFIG_VALUE_0", "bad-command")
	t.Setenv("LC_ALL", "invalid-locale")
	t.Setenv("LANGUAGE", "invalid-language")
	for _, entry := range HostEnvironment() {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "LANGUAGE":
			t.Fatalf("inherited unsafe environment: %s", entry)
		case "LC_ALL":
			if value != "C" {
				t.Fatalf("inherited locale: %s", entry)
			}
		}
	}
}

func TestLocalCommandPolicy(t *testing.T) {
	dir := installGitProbe(t, `printf '%s\000' "$@" "$PWD" \
    "$GIT_CONFIG_NOSYSTEM" "$GIT_CONFIG_GLOBAL" "$GIT_TERMINAL_PROMPT" \
    "$GIT_NO_LAZY_FETCH" "$GIT_NO_REPLACE_OBJECTS" "$GIT_OPTIONAL_LOCKS" "$LC_ALL"
cat`)
	g := Local{Dir: dir}
	g.DisableFilter("filter.example.clean")
	g.DisableFilter("filter.example.required")
	out, err := g.Input(t.Context(), []byte("input\x00bytes"), "check-attr", "--stdin", "name '$;()")
	want := []byte(strings.Join([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
		"-c", "credential.helper=", "-c", "core.askPass=/bin/false", "-c", "protocol.allow=never",
		"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "core.attributesFile=/dev/null",
		"-c", "filter.example.clean=", "-c", "filter.example.required=false",
		"check-attr", "--stdin", "name '$;()", dir, "1", "/dev/null", "0", "1", "1", "0", "C", "input\x00bytes",
	}, "\x00"))
	if err != nil || !bytes.Equal(out, want) {
		t.Fatalf("local command=%q %v, want %q", out, err, want)
	}
}

func TestLocalFailureAndCancellation(t *testing.T) {
	dir := installGitProbe(t, "printf 'fixture failure' >&2\nexit 28")
	g := Local{Dir: dir}
	_, err := g.Run(t.Context(), "status")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 28 {
		t.Fatalf("exit status=%v", err)
	}
	if !strings.Contains(err.Error(), "fixture failure") {
		t.Fatalf("failure=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.Run(ctx, "status"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func installGitProbe(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}
