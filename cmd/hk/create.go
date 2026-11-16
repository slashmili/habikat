package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/slashmili/habikat/internal/vm"
)

type createOptions struct {
	name          string
	withGitConfig bool
}

func runCreate(ctx context.Context, args []string, out io.Writer, createVM func(context.Context, string, vm.CreateOptions) (*vm.Project, error)) error {
	opts, err := parseCreateArgs(args)
	if err != nil {
		return err
	}
	ctx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	project, err := createVM(ctx, opts.name, vm.CreateOptions{WithGitConfig: opts.withGitConfig})
	if err != nil {
		return err
	}
	if project != nil {
		_, err = fmt.Fprintf(out, "Created %s with branch %s at %.12s in %s. Enter with: hk attach %s\n", opts.name, project.Branch, project.Commit, project.Workspace, opts.name)
	} else {
		_, err = fmt.Fprintf(out, "Created empty VM %s. Enter with: hk attach %s\n", opts.name, opts.name)
	}
	return err
}

func parseCreateArgs(args []string) (createOptions, error) {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "VM name")
	withGitConfig := flags.Bool("with-git-config", false, "import only user.name and user.email")
	if err := flags.Parse(args); err != nil {
		return createOptions{}, err
	}
	if flags.NArg() != 0 || *name == "" {
		return createOptions{}, errors.New("usage: hk create --name NAME")
	}
	if err := vm.ValidateName(*name); err != nil {
		return createOptions{}, err
	}
	return createOptions{name: *name, withGitConfig: *withGitConfig}, nil
}
