package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"habikato.local/habikato/internal/vm"
)

type createOptions struct {
	name string
}

func runCreate(ctx context.Context, args []string, out io.Writer, createVM func(context.Context, string) error) error {
	opts, err := parseCreateArgs(args)
	if err != nil {
		return err
	}
	ctx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	if err := createVM(ctx, opts.name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Created %s. Enter with: hk attach %s\n", opts.name, opts.name)
	return err
}

func parseCreateArgs(args []string) (createOptions, error) {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "VM name")
	if err := flags.Parse(args); err != nil {
		return createOptions{}, err
	}
	if flags.NArg() != 0 || *name == "" {
		return createOptions{}, errors.New("usage: hk create --name NAME")
	}
	if err := vm.ValidateName(*name); err != nil {
		return createOptions{}, err
	}
	return createOptions{name: *name}, nil
}
