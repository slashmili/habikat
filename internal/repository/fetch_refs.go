package repository

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/slashmili/habikat/internal/git"
)

func gitExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func readDirectFetchRef(ctx context.Context, g git.Local, ref, format string) (string, error) {
	if err := rejectSymbolicFetchRef(ctx, g, ref); err != nil {
		return "", err
	}
	output, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", ref)
	if gitExitCode(err) == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read destination ref %s: %w", ref, err)
	}
	oid := strings.TrimSuffix(string(output), "\n")
	if !validObjectID(oid, format) {
		return "", errors.New("invalid object ID in destination ref")
	}
	return oid, nil
}

func rejectSymbolicFetchRef(ctx context.Context, g git.Local, ref string) error {
	_, err := g.Run(ctx, "symbolic-ref", "--quiet", "--no-recurse", ref)
	if err == nil {
		return fmt.Errorf("refuse symbolic destination ref %s", ref)
	}
	if gitExitCode(err) != 1 {
		return fmt.Errorf("inspect destination ref %s: %w", ref, err)
	}
	return nil
}

func (i *BranchImport) publish(ctx context.Context, beforePrepare func()) error {
	if i.closed || i.published {
		return errors.New("fetch import is closed or already published")
	}
	callerCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := i.destination.git.Command(ctx, "update-ref", "--no-deref", "--create-reflog", "-m", "hk fetch "+i.instanceID, "--stdin", "-z")
	if err != nil {
		return err
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Join(err, input.Close())
	}
	stderr := &fetchDiagnostics{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return errors.Join(err, input.Close(), output.Close())
	}
	err = i.runRefTransaction(ctx, input, bufio.NewReaderSize(output, 256), beforePrepare)
	closeErr := input.Close()
	if err != nil || closeErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if failure := errors.Join(err, closeErr); failure != nil {
		return fmt.Errorf("publish fetched refs: %w: %s", errors.Join(failure, callerCtx.Err()), strings.TrimSpace(string(stderr.data)))
	}
	if waitErr != nil {
		return fmt.Errorf("publish fetched refs: %w: %s", errors.Join(waitErr, callerCtx.Err()), strings.TrimSpace(string(stderr.data)))
	}
	i.published = true
	return nil
}

func (i *BranchImport) runRefTransaction(ctx context.Context, input io.Writer, output *bufio.Reader, beforePrepare func()) error {
	if err := sendRefTransactionCommand(input, output, "start"); err != nil {
		return err
	}
	if _, err := io.WriteString(input, i.refUpdates()); err != nil {
		return err
	}
	if beforePrepare != nil {
		beforePrepare()
	}
	if err := sendRefTransactionCommand(input, output, "prepare"); err != nil {
		return err
	}
	for _, branch := range i.branches {
		if err := rejectSymbolicFetchRef(ctx, i.destination.git, branch.Ref); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return sendRefTransactionCommand(input, output, "commit")
}

func (i *BranchImport) refUpdates() string {
	var input strings.Builder
	zero := strings.Repeat("0", 40)
	if i.destination.format == "sha256" {
		zero = strings.Repeat("0", 64)
	}
	for _, branch := range i.branches {
		if branch.Commit == branch.Previous {
			fmt.Fprintf(&input, "verify %s\x00%s\x00", branch.Ref, branch.Previous)
			continue
		}
		previous := branch.Previous
		if previous == "" {
			previous = zero
		}
		fmt.Fprintf(&input, "update %s\x00%s\x00%s\x00", branch.Ref, branch.Commit, previous)
	}
	return input.String()
}

func sendRefTransactionCommand(input io.Writer, output *bufio.Reader, command string) error {
	if _, err := io.WriteString(input, command+"\x00"); err != nil {
		return err
	}
	reply, err := output.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("Git ref transaction %s: %w", command, err)
	}
	if string(reply) != command+": ok\n" {
		return fmt.Errorf("unexpected Git ref transaction reply for %s", command)
	}
	return nil
}

type fetchDiagnostics struct{ data []byte }

func (d *fetchDiagnostics) Write(p []byte) (int, error) {
	size := len(p)
	remaining := (64 << 10) - len(d.data)
	if len(p) > remaining {
		p = p[:remaining]
	}
	d.data = append(d.data, p...)
	return size, nil
}
