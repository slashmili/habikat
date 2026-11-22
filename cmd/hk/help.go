package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

type helpPrinter struct {
	err error
}

func (h *helpPrinter) print(cmd *cobra.Command, _ []string) {
	_, h.err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n%s", commandDescription(cmd), cmd.UsageString())
}

func commandDescription(cmd *cobra.Command) string {
	if cmd.Long != "" {
		return cmd.Long
	}
	return cmd.Short
}
