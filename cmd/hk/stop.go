package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

type stopFunc func(context.Context, string, bool) error

func newStopCommand(stop stopFunc) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:                   "stop [--force] NAME",
		Short:                 "Shut down a running VM",
		Long:                  "Shut down a running VM gracefully.\n\nhk stop --force abruptly stops the VM; buffered guest data may be lost.",
		DisableFlagsInUseLine: true,
		Args:                  requireVMName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(cmd.Context(), cmd.OutOrStdout(), args[0], force, stop)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop abruptly; buffered guest data may be lost")
	return cmd
}

func runStop(ctx context.Context, out io.Writer, name string, force bool, stop stopFunc) error {
	if force {
		if _, err := fmt.Fprintln(out, "Forced stop: buffered guest data may be lost."); err != nil {
			return err
		}
	}
	if err := stop(ctx, name, force); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "Stopped %s.\n", name)
	return err
}
