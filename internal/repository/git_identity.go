package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/slashmili/habikat/internal/git"
)

type GitIdentity struct {
	Name  string
	Email string
}

func (identity GitIdentity) Validate() error {
	for _, item := range []struct{ key, value string }{{"user.name", identity.Name}, {"user.email", identity.Email}} {
		if !validGitIdentityValue(item.value) {
			return fmt.Errorf("--with-git-config requires a nonempty valid %s; configure it with git config %s VALUE", item.key, item.key)
		}
	}
	return nil
}

func validGitIdentityValue(value string) bool {
	if !utf8.ValidString(value) || strings.ContainsAny(value, "<>") || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	return strings.Trim(strings.TrimSpace(value), " ,:;\"\\'") != ""
}

func ReadGitIdentity(ctx context.Context, dir string) (*GitIdentity, error) {
	root, err := (git.Local{Dir: dir}).Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("--with-git-config requires a non-bare Git repository: %w", err)
	}
	dir = strings.TrimSuffix(string(root), "\n")
	name, err := gitIdentityConfigValue(ctx, dir, "user.name")
	if err != nil {
		return nil, err
	}
	email, err := gitIdentityConfigValue(ctx, dir, "user.email")
	if err != nil {
		return nil, err
	}
	identity := &GitIdentity{Name: name, Email: email}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	return identity, nil
}

func gitIdentityConfigValue(ctx context.Context, dir, key string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--includes", "--null", "--get", key)
	cmd.Dir, cmd.Env = dir, gitIdentityEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("--with-git-config requires configured %s; run git config %s VALUE: %w", key, key, errors.Join(err, ctx.Err()))
	}
	if len(out) == 0 || out[len(out)-1] != 0 {
		return "", fmt.Errorf("invalid Git configuration result for %s", key)
	}
	return string(out[:len(out)-1]), nil
}

func gitIdentityEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") && key != "GIT_CONFIG_SYSTEM" && key != "GIT_CONFIG_GLOBAL" && key != "GIT_CONFIG_NOSYSTEM" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}
