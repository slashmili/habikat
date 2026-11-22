package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newProxyCommand(proxy func(string, io.Reader, io.Writer) error, releaseSignals func()) *cobra.Command {
	return &cobra.Command{
		Use:                   "_proxy SOCKET",
		Hidden:                true,
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		Args:                  requireSocketPath,
		RunE: func(cmd *cobra.Command, args []string) error {
			releaseSignals()
			return proxy(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func requireSocketPath(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return withUsage(cmd, fmt.Errorf("expected exactly one socket path, got %d arguments", len(args)))
	}
	return nil
}
