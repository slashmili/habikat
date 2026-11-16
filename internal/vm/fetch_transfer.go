package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const maxFetchBundleBytes int64 = 1 << 30

var errFetchBundleLimit = errors.New("guest bundle exceeds the 1 GiB transfer limit")

func downloadExport(ctx context.Context, dir, command string, out io.Writer) error {
	deadline, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	args, err := sshArgs()
	if err != nil {
		return err
	}
	args = append(args, "-T", "hk@habikat", command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	output := &boundedBundleWriter{writer: out, remaining: maxFetchBundleBytes, cancel: cancel}
	diagnostic := &fetchDiagnostic{cancel: cancel}
	cmd.Stdout, cmd.Stderr = output, diagnostic
	runErr := cmd.Run()
	err = fetchTransferFailure(runErr, output.err, diagnostic.overflow, deadline.Err())
	if err != nil {
		return fmt.Errorf("download guest bundle (VM disk preserved): %w: %s", err, strings.TrimSpace(string(diagnostic.data)))
	}
	return nil
}

func fetchTransferFailure(runErr, outputErr error, overflow bool, callerErr error) error {
	if overflow {
		outputErr = errors.Join(outputErr, errGuestOutputLimit)
	}
	if outputErr != nil && callerErr == nil {
		runErr = nil
	}
	return errors.Join(runErr, outputErr, callerErr)
}

type boundedBundleWriter struct {
	writer    io.Writer
	remaining int64
	cancel    context.CancelFunc
	err       error
}

func (out *boundedBundleWriter) Write(data []byte) (int, error) {
	if out.err != nil {
		return 0, out.err
	}
	overflow := int64(len(data)) > out.remaining
	if overflow {
		data = data[:out.remaining]
	}
	n, err := out.writer.Write(data)
	out.remaining -= int64(n)
	if overflow {
		err = errors.Join(err, errFetchBundleLimit)
	}
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		out.err = err
		out.cancel()
	}
	return n, err
}

type fetchDiagnostic struct {
	limitedGuestOutput
	cancel context.CancelFunc
}

func (out *fetchDiagnostic) Write(data []byte) (int, error) {
	out.captureUpToLimit(data)
	if out.overflow {
		out.cancel()
	}
	return len(data), nil
}
