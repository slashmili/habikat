package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/slashmili/habikat/internal/vm"
)

func runList(
	ctx context.Context,
	args []string,
	out io.Writer,
	listVMs func(context.Context, bool) ([]vm.Listing, error),
) error {
	all, err := parseListArgs(args)
	if err != nil {
		return err
	}
	rows, listErr := listVMs(ctx, all)
	if listErr != nil {
		listErr = fmt.Errorf("incomplete VM inventory: %s", terminalText(listErr.Error()))
		if len(rows) == 0 {
			return listErr
		}
	}
	return errors.Join(listErr, renderList(out, rows))
}

func parseListArgs(args []string) (bool, error) {
	all := false
	for _, arg := range args {
		switch arg {
		case "-a", "--all":
			all = true
		default:
			return false, fmt.Errorf("unexpected argument %q; usage: hk list [-a|--all]", arg)
		}
	}
	return all, nil
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
		directory := row.Directory
		if directory == "" {
			directory = "(unknown)"
		}
		if _, err := fmt.Fprintf(
			table,
			"%s\t%s\t%s\n",
			terminalText(row.Name),
			terminalText(row.Status),
			terminalText(directory),
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
