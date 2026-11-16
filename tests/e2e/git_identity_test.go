//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectGitIdentity(t *testing.T) {
	h := newHarness(t)
	host, _, commit := projectFixture(t, h)
	name, email := "Zoë 李 O'Neil $HOME `id`; Fixture", "fixture+identity@example.invalid"
	fixtureGit(t, host, "config", "user.name", name)
	fixtureGit(t, host, "config", "user.email", email)
	for key, value := range map[string]string{
		"credential.helper": "!touch /home/hk/identity-sentinel",
		"user.signingKey":   "fixture-signing-key",
		"commit.gpgSign":    "true",
		"gpg.program":       "/home/hk/signing-sentinel",
		"alias.fixture":     "!touch /home/hk/alias-sentinel",
	} {
		fixtureGit(t, host, "config", key, value)
	}
	home := filepath.Join(h.dir, "identity-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Wrong Global\nemail = wrong@example.invalid\n"))
	h.env = append(h.env, "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Wrong Author", "GIT_AUTHOR_EMAIL=wrong-author@example.invalid", "GIT_COMMITTER_NAME=Wrong Committer", "GIT_COMMITTER_EMAIL=wrong-committer@example.invalid")
	config := read(t, filepath.Join(host, ".git/config"))
	index := read(t, filepath.Join(host, ".git/index"))
	h.must("", "create", "--name", "identity", "--with-git-config")
	checks := `set -eu
test "$(git config --local --get user.name)" = ` + quote(name) + `
test "$(git config --local --get user.email)" = ` + quote(email) + `
test "$(/bin/busybox stat -c '%U' .git/config)" = hk
test -z "$(git config --global --get user.name || true)"
test -z "$(git config --get-regexp '^(credential|gpg|alias|commit|filter)\.' || true)"
test -z "$(git config --get user.signingKey || true)"
test ! -e /home/hk/identity-sentinel
test ! -e /home/hk/alias-sentinel
printf 'normal guest commit\n' > 'file with spaces'
git add 'file with spaces'
git commit -m 'normal identity commit'
test "$(git log -1 --format='%an')" = ` + quote(name) + `
test "$(git log -1 --format='%ae')" = ` + quote(email) + `
test "$(git log -1 --format='%cn')" = ` + quote(name) + `
test "$(git log -1 --format='%ce')" = ` + quote(email) + `
printf 'IDENTITY_OK\n'
`
	if out := h.must(checks, "attach", "identity"); !strings.Contains(out, "IDENTITY_OK") {
		t.Fatalf("identity checks missing: %s", out)
	}
	h.must("", "create", "--name", "default")
	h.must(`set -eu
test -z "$(git config --get user.name || true)"
test -z "$(git config --get user.email || true)"
test -z "$(git config --get-regexp '^(credential|gpg|alias|commit|filter)\.' || true)"
`, "attach", "default")
	if !bytes.Equal(config, read(t, filepath.Join(host, ".git/config"))) || !bytes.Equal(index, read(t, filepath.Join(host, ".git/index"))) || fixtureGit(t, host, "rev-parse", "HEAD") != commit || fixtureGit(t, host, "status", "--porcelain") != "" {
		t.Fatal("identity import changed host repository")
	}
	h.env = append(h.env, "HK_E2E_IDENTITY_FAILURE=1")
	if out, err := h.run("", "create", "--name", "failed-identity", "--with-git-config"); err == nil || !strings.Contains(out, "configure guest Git identity") {
		t.Fatalf("guest identity failure did not fail creation: %v %s", err, out)
	}
	if _, err := os.Stat(h.state("failed-identity", "")); !os.IsNotExist(err) {
		t.Fatalf("failed identity left VM state: %v", err)
	}
	h.must("test \"$(cat 'file with spaces')\" = 'normal guest commit' || exit 1\n", "attach", "identity")
}
