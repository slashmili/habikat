package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newRootCommand(ops vmOperations, releaseSignals func()) *cobra.Command {
	root := &cobra.Command{
		Use:                   "hk",
		DisableFlagsInUseLine: true,
		Short:                 "Run coding agents in disposable local virtual machines",
		Args:                  rejectUnknownCommand,
		RunE:                  requireCommand,
		SilenceErrors:         true,
		SilenceUsage:          true,
		CompletionOptions:     cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetFlagErrorFunc(flagUsageError)
	root.AddCommand(
		newCreateCommand(ops.create),
		newAttachCommand(ops.attach, releaseSignals),
		newFetchCommand(ops.fetch),
		newDeleteCommand(ops.delete),
		newStopCommand(ops.stop),
		newStartCommand(ops.start),
		newListCommand(ops.list),
		newProxyCommand(ops.proxy, releaseSignals),
	)
	return root
}

func rejectUnknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return withUsage(cmd, fmt.Errorf("unknown command %q", args[0]))
	}
	return nil
}

func requireCommand(cmd *cobra.Command, _ []string) error {
	return withUsage(cmd, errors.New("missing command"))
}
