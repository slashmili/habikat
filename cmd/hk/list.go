package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/slashmili/habikat/internal/vm"
	"github.com/spf13/cobra"
)

type listFunc func(context.Context, bool) ([]vm.Listing, error)

func newListCommand(listVMs listFunc) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:                   "list [-a|--all]",
		Short:                 "List active VMs for the current repository",
		DisableFlagsInUseLine: true,
		Args:                  rejectArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd.Context(), cmd.OutOrStdout(), all, listVMs)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "list all VMs, including stopped ones and other repositories")
	return cmd
}

func runList(ctx context.Context, out io.Writer, all bool, listVMs listFunc) error {
	rows, listErr := listVMs(ctx, all)
	if listErr != nil {
		listErr = fmt.Errorf("incomplete VM inventory: %s", terminalText(listErr.Error()))
		if len(rows) == 0 {
			return listErr
		}
	}
	return errors.Join(listErr, renderList(out, rows))
}

func renderList(out io.Writer, rows []vm.Listing) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "No VMs found.")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAME\tSTATUS\tDIRECTORY"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(
			table,
			"%s\t%s\t%s\n",
			terminalText(row.Name),
			terminalText(row.Status),
			terminalText(row.Directory),
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

func terminalText(text string) string {
	quoted := strconv.QuoteToGraphic(text)
	return quoted[1 : len(quoted)-1]
}
