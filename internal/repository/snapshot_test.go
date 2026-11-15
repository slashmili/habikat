package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "/must/not/read")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
	t.Setenv("GIT_CONFIG_VALUE_0", "bad-command")
	for _, entry := range gitEnv() {
		if strings.Contains(entry, "must/not/read") || strings.Contains(entry, "bad-command") || strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") {
			t.Fatalf("inherited unsafe environment: %s", entry)
		}
	}
}

func TestCaptureCancellationAndInvalidDirectory(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if snapshot, err := Capture(ctx, t.TempDir()); snapshot != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("capture=%v %v", snapshot, err)
	}
	if snapshot, err := Capture(t.Context(), filepath.Join(t.TempDir(), "absent")); snapshot != nil || err == nil {
		t.Fatalf("invalid directory silently accepted: %v %v", snapshot, err)
	}
}

func TestCloseSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.bundle")
	if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (&Snapshot{Bundle: path}).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot retained: %v", err)
	}
}
