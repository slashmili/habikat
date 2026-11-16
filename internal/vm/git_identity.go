package vm

import (
	"context"
	"strings"

	"github.com/slashmili/habikato/internal/git"
	"github.com/slashmili/habikato/internal/repository"
)

func configureGitIdentity(ctx context.Context, dir string, identity repository.GitIdentity) error {
	command, err := gitIdentityCommand(identity)
	if err != nil {
		return err
	}
	return sshCommand(ctx, dir, command, nil)
}

func gitIdentityCommand(identity repository.GitIdentity) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	g := git.ImportRemote()
	return strings.Join([]string{
		"set -eu",
		"cd /home/hk/workspace",
		g.Environment(),
		g.Command("config", "--local", "--replace-all", "user.name", identity.Name),
		g.Command("config", "--local", "--replace-all", "user.email", identity.Email),
		`test "$(` + g.Command("config", "--local", "--get", "user.name") + `)" = ` + shellQuote(identity.Name),
		`test "$(` + g.Command("config", "--local", "--get", "user.email") + `)" = ` + shellQuote(identity.Email),
	}, "\n"), nil
}
