package main

import (
	"context"
	"fmt"
	"io"
)

func runDelete(ctx context.Context, args []string, out io.Writer, deleteVM func(context.Context, string) error) error {
	name, err := parseNameArg(args)
	if err != nil {
		return err
	}
	if err := deleteVM(ctx, name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Deleted %s.\n", name)
	return err
}
