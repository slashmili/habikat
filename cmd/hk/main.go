package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"habikato.local/habikato/internal/vm"
)

const usage = "usage: hk create --name NAME | hk attach NAME | hk delete [--force] NAME"

type vmOperations struct {
	create func(context.Context, string) (*vm.Project, error)
	attach func(string) error
	delete func(context.Context, string, func() error) error
	proxy  func(string, io.Reader, io.Writer) error
}

func main() {
	ops := vmOperations{
		create: vm.Create,
		attach: vm.Attach,
		delete: vm.Delete,
		proxy:  vm.Proxy,
	}
	if err := run(
		os.Args[1:],
		os.Stdin,
		os.Stdout,
		ops,
	); err != nil {
		fmt.Fprintln(os.Stderr, "hk:", err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out io.Writer, ops vmOperations) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	if len(args) == 2 && args[0] == "_proxy" {
		return ops.proxy(args[1], in, out)
	}
	if len(args) == 0 {
		return errors.New(usage)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "create":
		return runCreate(
			ctx,
			args[1:],
			out,
			ops.create,
		)
	case "attach":
		return runAttach(args[1:], func() {
			cancel()
			signal.Reset(os.Interrupt, syscall.SIGTERM)
		}, ops.attach)
	case "delete":
		return runDelete(
			ctx,
			args[1:],
			in,
			out,
			ops.delete,
		)
	default:
		return errors.New(usage)
	}
}
