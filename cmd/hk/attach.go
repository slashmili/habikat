package main

import "github.com/spf13/cobra"

func newAttachCommand(attachVM func(string) error, releaseSignals func()) *cobra.Command {
	return &cobra.Command{
		Use:                   "attach NAME",
		Short:                 "Open a shell in a running VM",
		DisableFlagsInUseLine: true,
		Args:                  requireVMName,
		RunE: func(_ *cobra.Command, args []string) error {
			releaseSignals()
			return attachVM(args[0])
		},
	}
}
