package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/slashmili/habikat/internal/vm"
)

type stopOptions struct {
	name  string
	force bool
}

func parseStopArgs(args []string) (stopOptions, error) {
	var opts stopOptions
	for _, arg := range args {
		switch {
		case arg == "":
			return stopOptions{}, errors.New("VM name must not be empty")
		case arg == "--force" && !opts.force:
			opts.force = true
		case strings.HasPrefix(arg, "-") && arg != "--force":
			return stopOptions{}, fmt.Errorf("unknown option %q; usage: hk stop [--force] NAME", arg)
		case opts.name == "":
			opts.name = arg
		default:
			return stopOptions{}, errors.New("usage: hk stop [--force] NAME")
		}
	}
	if err := vm.ValidateName(opts.name); err != nil {
		return stopOptions{}, err
	}
	return opts, nil
}

func runStop(ctx context.Context, args []string, out io.Writer, stop func(context.Context, string, bool) error) error {
	opts, err := parseStopArgs(args)
	if err != nil {
		return err
	}
	if opts.force {
		if _, err := fmt.Fprintln(out, "Forced stop: buffered guest data may be lost."); err != nil {
			return err
		}
	}
	if err := stop(ctx, opts.name, opts.force); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Stopped %s.\n", opts.name)
	return err
}
