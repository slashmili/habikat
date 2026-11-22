package main

import (
	"context"
	"fmt"
	"io"

	"github.com/slashmili/habikat/internal/vm"
	"github.com/spf13/cobra"
)

type startFunc func(context.Context, string) (vm.StartResult, error)

func newStartCommand(start startFunc) *cobra.Command {
	return &cobra.Command{
		Use:                   "start NAME",
		Short:                 "Boot a stopped VM",
		Long:                  "Boot a stopped VM from its existing disk and keys.",
		DisableFlagsInUseLine: true,
		Args:                  requireVMName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd.Context(), cmd.OutOrStdout(), args[0], start)
		},
	}
}

func runStart(ctx context.Context, out io.Writer, name string, start startFunc) error {
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
