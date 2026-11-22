package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/slashmili/habikat/internal/vm"
)

type vmOperations struct {
	stop   stopFunc
	start  startFunc
	create createFunc
	attach func(string) error
	delete deleteFunc
	fetch  fetchFunc
	proxy  func(string, io.Reader, io.Writer) error
	list   listFunc
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, defaultVMOperations()); err != nil {
		fmt.Fprintln(os.Stderr, "hk:", err)
		os.Exit(1)
	}
}

func defaultVMOperations() vmOperations {
	return vmOperations{
		stop:   vm.Stop,
		start:  vm.Start,
		create: vm.Create,
		attach: vm.Attach,
		delete: vm.Delete,
		fetch:  vm.Fetch,
		proxy:  vm.Proxy,
		list:   vm.List,
	}
}

func run(args []string, in io.Reader, out io.Writer, ops vmOperations) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	releaseSignals := func() {
		cancel()
		signal.Reset(os.Interrupt, syscall.SIGTERM)
	}
	var help helpPrinter
	root := newRootCommand(ops, releaseSignals)
	root.SetHelpFunc(help.print)
	root.SetArgs(append([]string{}, args...))
	root.SetIn(in)
	root.SetOut(out)
	if err := root.ExecuteContext(ctx); err != nil {
		return err
	}
	return help.err
}
