//go:build e2e

package vm

import (
	"context"
	"io"
	"os"

	"github.com/slashmili/habikat/internal/repository"
)

func init() {
	applyGitIdentity = func(ctx context.Context, dir string, identity repository.GitIdentity) error {
		if os.Getenv("HK_E2E_IDENTITY_FAILURE") == "1" {
			if err := sshCommand(ctx, dir, "chmod a-w /home/hk/workspace/.git", nil); err != nil {
				return err
			}
		}
		return configureGitIdentity(ctx, dir, identity)
	}
	original := transferBundle
	transferBundle = func(ctx context.Context, dir string, in io.Reader) error {
		marker := os.Getenv("HK_E2E_TRANSFER_BOUNDARY")
		if marker == "" {
			return original(ctx, dir, in)
		}
		return transferIncompleteBundle(ctx, dir, in, marker)
	}
}

func transferIncompleteBundle(ctx context.Context, dir string, in io.Reader, marker string) error {
	if err := sshCommand(ctx, dir, receiveBundle, io.LimitReader(in, 16)); err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte("SSH transfer reached\n"), 0600); err != nil {
		return err
	}
	if os.Getenv("HK_E2E_TRANSFER_CANCEL") == "1" {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
