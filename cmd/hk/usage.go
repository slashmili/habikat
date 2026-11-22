package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/slashmili/habikat/internal/vm"
	"github.com/spf13/cobra"
)

func requireVMName(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return withUsage(cmd, fmt.Errorf("expected exactly one VM name, got %d arguments", len(args)))
	}
	return vm.ValidateName(args[0])
}

func rejectArguments(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return withUsage(cmd, fmt.Errorf("unexpected argument %q", args[0]))
	}
	return nil
}

func flagUsageError(cmd *cobra.Command, err error) error {
	return withUsage(cmd, errors.New(printableText(err.Error())))
}

func withUsage(cmd *cobra.Command, err error) error {
	if cmd.HasParent() {
		return fmt.Errorf("%w; usage: %s", err, cmd.UseLine())
	}
	return fmt.Errorf("%w; run '%s --help' for usage", err, cmd.CommandPath())
}

func printableText(text string) string {
	var printable strings.Builder
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		printable.WriteString(printableRune(r, text[:size]))
		text = text[size:]
	}
	return printable.String()
}

func printableRune(r rune, encoded string) string {
	if r == utf8.RuneError && len(encoded) == 1 {
		return fmt.Sprintf(`\x%02x`, encoded[0])
	}
	if strconv.IsGraphic(r) {
		return encoded
	}
	quoted := strconv.QuoteRuneToGraphic(r)
	return quoted[1 : len(quoted)-1]
}
