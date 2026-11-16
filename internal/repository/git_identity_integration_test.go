//go:build integration

package repository

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadGitIdentityConfigurationPrecedence(t *testing.T) {
	dir := fixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, "system"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	put(t, filepath.Join(home, "system"), "[user]\nname = System Fixture\nemail = system@example.invalid\n")
	assertGitIdentity(t, dir, GitIdentity{"System Fixture", "system@example.invalid"})
	put(t, filepath.Join(home, ".gitconfig"), "[user]\nname = Global Fixture\nemail = global@example.invalid\n")
	t.Setenv("GIT_AUTHOR_NAME", "Wrong Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "wrong@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Wrong Committer")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Wrong Override")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	assertGitIdentity(t, dir, GitIdentity{"Global Fixture", "global@example.invalid"})
	include := filepath.Join(home, "conditional")
	put(t, include, "[user]\nname = Conditional Fixture\nemail = conditional@example.invalid\n")
	gitTest(t, dir, "config", "--file", filepath.Join(home, ".gitconfig"), "includeIf.gitdir:"+filepath.Join(dir, ".git")+".path", include)
	assertGitIdentity(t, dir, GitIdentity{"Conditional Fixture", "conditional@example.invalid"})
	gitTest(t, dir, "config", "user.name", "Zoë 李 O'Neil $`;")
	gitTest(t, dir, "config", "user.email", "local+fixture@example.invalid")
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(filepath.Join(dir, ".git/config"))
	index, _ := os.ReadFile(filepath.Join(dir, ".git/index"))
	assertGitIdentity(t, sub, GitIdentity{"Zoë 李 O'Neil $`;", "local+fixture@example.invalid"})
	nextConfig, _ := os.ReadFile(filepath.Join(dir, ".git/config"))
	nextIndex, _ := os.ReadFile(filepath.Join(dir, ".git/index"))
	if !bytes.Equal(config, nextConfig) || !bytes.Equal(index, nextIndex) || gitTest(t, dir, "status", "--porcelain") != "" {
		t.Fatal("identity lookup changed source repository")
	}
}

func TestReadGitIdentityRejectsMissingInvalidAndAbsentRepository(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	for _, tc := range []struct{ name, author, email string }{
		{"missing both", "", ""},
		{"missing email", "Fixture", ""},
		{"blank email", "Fixture", "  "},
		{"newline email", "Fixture", "bad\nemail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t)
			if tc.author != "" {
				gitTest(t, dir, "config", "user.name", tc.author)
			}
			if tc.email != "" {
				gitTest(t, dir, "config", "user.email", tc.email)
			}
			if _, err := ReadGitIdentity(t.Context(), dir); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
	if _, err := ReadGitIdentity(t.Context(), t.TempDir()); err == nil {
		t.Fatal("non-repository accepted")
	}
}

func assertGitIdentity(t *testing.T, dir string, want GitIdentity) {
	t.Helper()
	got, err := ReadGitIdentity(t.Context(), dir)
	if err != nil || got == nil || *got != want {
		t.Fatalf("identity=%+v error=%v, want %+v", got, err, want)
	}
}
