package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/slashmili/habikat/internal/vm"
	"github.com/spf13/cobra"
)

type createFunc func(context.Context, string, vm.CreateOptions) (*vm.Project, error)

func newCreateCommand(createVM createFunc) *cobra.Command {
	var name string
	var options vm.CreateOptions
	cmd := &cobra.Command{
		Use:                   "create --name NAME [--with-git-config]",
		Short:                 "Create and boot a VM from the current directory",
		Long:                  "Create and boot a VM from the current directory.\n\n--with-git-config imports only user.name and user.email into the guest repository.",
		DisableFlagsInUseLine: true,
		Args: func(cmd *cobra.Command, args []string) error {
			return validateCreateArgs(cmd, args, name)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreate(cmd.Context(), cmd.OutOrStdout(), name, options, createVM)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "VM name")
	cmd.Flags().BoolVar(&options.WithGitConfig, "with-git-config", false, "import only user.name and user.email into the guest repository")
	return cmd
}

func validateCreateArgs(cmd *cobra.Command, args []string, name string) error {
	if err := rejectArguments(cmd, args); err != nil {
		return err
	}
	if name == "" {
		return withUsage(cmd, errors.New("missing required --name"))
	}
	return vm.ValidateName(name)
}

func runCreate(ctx context.Context, out io.Writer, name string, options vm.CreateOptions, createVM createFunc) error {
	ctx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	project, err := createVM(ctx, name, options)
	if err != nil {
		return err
	}
	return printCreateResult(out, name, project)
}

func printCreateResult(out io.Writer, name string, project *vm.Project) error {
	if project == nil {
		_, err := fmt.Fprintf(out, "Created empty VM %s. Enter with: hk attach %s\n", name, name)
		return err
	}
	_, err := fmt.Fprintf(out, "Created %s with branch %s at %.12s in %s. Enter with: hk attach %s\n", name, project.Branch, project.Commit, project.Workspace, name)
	return err
}
