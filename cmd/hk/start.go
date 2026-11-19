package main

import (
	"context"
	"fmt"
	"io"

	"github.com/slashmili/habikat/internal/vm"
)

func runStart(ctx context.Context, args []string, out io.Writer, start func(context.Context, string) (vm.StartResult, error)) error {
	name, err := parseNameArg(args)
	if err != nil {
		return err
	}
	result, err := start(ctx, name)
	if err != nil {
		return err
	}
	return printStartResult(out, name, result)
}

func printStartResult(out io.Writer, name string, result vm.StartResult) error {
	if result.AlreadyRunning {
		_, err := fmt.Fprintf(out, "%s is already running.\n", name)
		return err
	}
	_, err := fmt.Fprintf(out, "Started %s.\n", name)
	return err
}
