package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var errDeleteCancelled = errors.New("deletion cancelled")

type deleteFunc func(context.Context, string, func() error) error

func newDeleteCommand(deleteVM deleteFunc) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:                   "delete [--force] NAME",
		Short:                 "Delete a VM and all its files",
		Long:                  "Delete a VM and all its files.\n\nA VM with file changes or new commits requires confirmation from an interactive terminal; --force discards them without asking.",
		DisableFlagsInUseLine: true,
		Args:                  requireVMName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), args[0], force, deleteVM)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "discard file changes and new commits without confirmation")
	return cmd
}

func runDelete(ctx context.Context, in io.Reader, out io.Writer, name string, force bool, deleteVM deleteFunc) error {
	var confirm func() error
	if !force {
		confirm = func() error {
			file, ok := in.(*os.File)
			if !ok || !term.IsTerminal(int(file.Fd())) {
				return errors.New("VM " + name + " has file changes or new commits; deletion requires an interactive input terminal; use hk delete --force " + name + " to discard all its files and commits")
			}
			return confirmDeletion(ctx, name, terminalReader{ctx, file}, out)
		}
	}
	if err := deleteVM(ctx, name, confirm); err != nil {
		if errors.Is(err, errDeleteCancelled) || errors.Is(err, context.Canceled) {
			_, writeErr := fmt.Fprintf(out, "Deletion of %s cancelled.\n", name)
			return writeErr
		}
		return err
	}
	_, err := fmt.Fprintf(out, "Deleted %s.\n", name)
	return err
}

func confirmDeletion(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	if _, err := fmt.Fprintf(out, "VM %s has file changes or new commits. All its files and commits will be discarded.\nDelete VM %s and all its files? [y/N] ", name, name); err != nil {
		return err
	}
	line, err := readCompleteApprovalLine(ctx, in)
	if err != nil {
		return errors.Join(errDeleteCancelled, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(errDeleteCancelled, err)
	}
	answer := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	if answer != "y" && answer != "yes" {
		return errDeleteCancelled
	}
	return nil
}

type terminalReader struct {
	ctx  context.Context
	file *os.File
}

func (r terminalReader) Read(p []byte) (int, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := unix.Poll([]unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return r.file.Read(p)
		}
	}
}

func readCompleteApprovalLine(ctx context.Context, in io.Reader) (string, error) {
	var line []byte
	var one [1]byte
	for len(line) < 64 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := in.Read(one[:])
		if err != nil {
			return "", err
		}
		if n != 1 {
			return "", io.ErrNoProgress
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			return string(line), nil
		}
	}
	return "", errors.New("confirmation line is too long")
}
