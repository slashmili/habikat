package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/slashmili/habikat/internal/vm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var errFetchCancelled = errors.New("fetch cancelled")

type fetchFunc func(context.Context, string, func(vm.GuestChanges) error) (*vm.FetchResult, error)

func newFetchCommand(fetchVM fetchFunc) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:                   "fetch [--force] NAME",
		Short:                 "Import committed branches from a running VM",
		Long:                  "Import committed branches from a running VM without changing your checkout.\n\nhk fetch --force skips only confirmation for uncommitted guest files, which remain in the VM.\nIgnored files and files outside the repository are excluded.",
		DisableFlagsInUseLine: true,
		Args:                  requireVMName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFetch(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), args[0], force, fetchVM)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "fetch committed branches without confirming uncommitted guest files")
	return cmd
}

func confirmFetch(ctx context.Context, name string, changes vm.GuestChanges, in io.Reader, out io.Writer) error {
	if _, err := fmt.Fprintf(out, "Excluded guest changes: %s. Ignored files and files outside the repository are always excluded.\nGuest %s has uncommitted changes. Fetch committed branches only and leave these files in the VM? [y/N] ", changeCategories(changes), name); err != nil {
		return err
	}
	line, err := readCompleteApprovalLine(ctx, in)
	if err != nil {
		return errors.Join(errFetchCancelled, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(errFetchCancelled, err)
	}
	answer := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	if answer != "y" && answer != "yes" {
		return errFetchCancelled
	}
	return nil
}

func runFetch(ctx context.Context, in io.Reader, out io.Writer, name string, force bool, fetchVM fetchFunc) error {
	var confirm func(vm.GuestChanges) error
	if !force {
		confirm = func(changes vm.GuestChanges) error {
			file, ok := in.(*os.File)
			if !ok || !term.IsTerminal(int(file.Fd())) {
				return fmt.Errorf("guest %s has uncommitted changes (%s); commit them in the VM or rerun hk fetch %s --force to fetch committed branches only", name, changeCategories(changes), name)
			}
			return confirmFetch(ctx, name, changes, terminalReader{ctx, file}, out)
		}
	}
	result, err := fetchVM(ctx, name, confirm)
	if err != nil {
		if errors.Is(err, errFetchCancelled) || errors.Is(err, context.Canceled) {
			_, writeErr := fmt.Fprintf(out, "Fetch from %s cancelled.\n", name)
			return writeErr
		}
		return err
	}
	return printFetchResult(out, name, result)
}

func changeCategories(changes vm.GuestChanges) string {
	var categories []string
	if changes.Staged {
		categories = append(categories, "staged additions/edits/deletions")
	}
	if changes.Unstaged {
		categories = append(categories, "unstaged tracked changes")
	}
	if changes.Untracked {
		categories = append(categories, "non-ignored untracked files")
	}
	return strings.Join(categories, ", ")
}

func printFetchResult(out io.Writer, name string, result *vm.FetchResult) error {
	for _, branch := range result.Branches {
		change := ""
		if branch.Previous == branch.Commit {
			change = " (unchanged)"
		}
		if branch.Rewritten {
			change = " (non-fast-forward; previous tip retained in reflog)"
		}
		if _, err := fmt.Fprintf(out, "Fetched %s: %s at %s -> %s%s\n", name, branch.Name, branch.Commit, branch.Ref, change); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "Only committed branches were fetched; any uncommitted files remain in VM %s. Ignored files and files outside the repository are excluded.\n", name)
	return err
}
