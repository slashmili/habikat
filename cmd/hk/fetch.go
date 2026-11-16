package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/slashmili/habikat/internal/vm"
	"golang.org/x/term"
)

var errFetchCancelled = errors.New("fetch cancelled")

type fetchOptions struct {
	name  string
	force bool
}

func parseFetchArgs(args []string) (fetchOptions, error) {
	var opts fetchOptions
	var names []string
	for _, arg := range args {
		if arg == "--force" && !opts.force {
			opts.force = true
			continue
		}
		names = append(names, arg)
	}
	if len(names) != 1 {
		return fetchOptions{}, errors.New("usage: hk fetch [--force] NAME")
	}
	if err := vm.ValidateName(names[0]); err != nil {
		return fetchOptions{}, err
	}
	opts.name = names[0]
	return opts, nil
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

func runFetch(ctx context.Context, args []string, in io.Reader, out io.Writer, fetchVM func(context.Context, string, func(vm.GuestChanges) error) (*vm.FetchResult, error)) error {
	opts, err := parseFetchArgs(args)
	if err != nil {
		return err
	}
	var confirm func(vm.GuestChanges) error
	if !opts.force {
		confirm = func(changes vm.GuestChanges) error {
			file, ok := in.(*os.File)
			if !ok || !term.IsTerminal(int(file.Fd())) {
				return fmt.Errorf("guest %s has uncommitted changes (%s); commit them in the VM or rerun hk fetch %s --force to fetch committed branches only", opts.name, changeCategories(changes), opts.name)
			}
			return confirmFetch(ctx, opts.name, changes, terminalReader{ctx, file}, out)
		}
	}
	result, err := fetchVM(ctx, opts.name, confirm)
	if err != nil {
		if errors.Is(err, errFetchCancelled) || errors.Is(err, context.Canceled) {
			_, writeErr := fmt.Fprintf(out, "Fetch from %s cancelled.\n", opts.name)
			return writeErr
		}
		return err
	}
	return printFetchResult(out, opts.name, result)
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
