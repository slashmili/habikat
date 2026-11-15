package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/slashmili/habikato/internal/vm"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var errDeleteCancelled = errors.New("deletion cancelled")

type deleteOptions struct {
	name  string
	force bool
}

func parseDeleteArgs(args []string) (deleteOptions, error) {
	var opts deleteOptions
	var names []string
	for _, arg := range args {
		if arg == "--force" && !opts.force {
			opts.force = true
			continue
		}
		names = append(names, arg)
	}
	if len(names) != 1 {
		return deleteOptions{}, errors.New(usage)
	}
	if err := vm.ValidateName(names[0]); err != nil {
		return deleteOptions{}, err
	}
	opts.name = names[0]
	return opts, nil
}

func runDelete(ctx context.Context, args []string, in io.Reader, out io.Writer, deleteVM func(context.Context, string, func() error) error) error {
	opts, err := parseDeleteArgs(args)
	if err != nil {
		return err
	}
	var confirm func() error
	if !opts.force {
		confirm = func() error {
			file, ok := in.(*os.File)
			if !ok || !term.IsTerminal(int(file.Fd())) {
				return errors.New("VM " + opts.name + " has file changes or new commits; deletion requires an interactive input terminal; use hk delete --force " + opts.name + " to discard all its files and commits")
			}
			return confirmDeletion(ctx, opts.name, terminalReader{ctx, file}, out)
		}
	}
	if err := deleteVM(ctx, opts.name, confirm); err != nil {
		if errors.Is(err, errDeleteCancelled) || errors.Is(err, context.Canceled) {
			_, writeErr := fmt.Fprintf(out, "Deletion of %s cancelled.\n", opts.name)
			return writeErr
		}
		return err
	}
	_, err = fmt.Fprintf(out, "Deleted %s.\n", opts.name)
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
