package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/slashmili/habikat/internal/vm"
)

const usage = `usage: hk create --name NAME [--with-git-config]
hk attach NAME
hk fetch [--force] NAME
hk delete [--force] NAME
hk stop [--force] NAME
hk list [-a|--all]

--with-git-config imports only user.name and user.email into the guest repository.
hk fetch imports committed branches from a running VM without changing your checkout.
hk fetch --force skips only confirmation for uncommitted guest files, which remain in the VM.
Ignored files and files outside the repository are excluded.
hk stop --force abruptly stops the VM; buffered guest data may be lost.`

type vmOperations struct {
	stop   func(context.Context, string, bool) error
	create func(context.Context, string, vm.CreateOptions) (*vm.Project, error)
	attach func(string) error
	delete func(context.Context, string, func() error) error
	fetch  func(context.Context, string, func(vm.GuestChanges) error) (*vm.FetchResult, error)
	proxy  func(string, io.Reader, io.Writer) error
	list   func(context.Context, bool) ([]vm.Listing, error)
}

func main() {
	ops := vmOperations{
		stop:   vm.Stop,
		create: vm.Create,
		attach: vm.Attach,
		delete: vm.Delete,
		fetch:  vm.Fetch,
		proxy:  vm.Proxy,
		list:   vm.List,
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
	case "stop":
		return runStop(ctx, args[1:], out, ops.stop)
	case "list":
		return runList(ctx, args[1:], out, ops.list)
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
	case "fetch":
		return runFetch(ctx, args[1:], in, out, ops.fetch)
	default:
		return errors.New(usage)
	}
}
