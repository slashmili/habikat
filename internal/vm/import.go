package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/slashmili/habikato/internal/git"
	"github.com/slashmili/habikato/internal/repository"
)

const receiveBundle = `set -eu
umask 077
mkdir /home/hk/.hk-import
cat > /home/hk/.hk-import/project.bundle`

func importCommand(branch, commit string) string {
	g := git.ImportRemote()
	return strings.Join([]string{
		"set -eu",
		"umask 077",
		g.Environment(),
		`trap 'rm -rf /home/hk/.hk-import /home/hk/.hk-workspace' EXIT`,
		importCloneCommand(branch, commit),
		"cd /home/hk/.hk-workspace",
		g.Command("remote", "remove", "origin"),
		g.Command("symbolic-ref", "HEAD", "refs/heads/"+branch),
		g.Command("reset", "--hard", commit),
		`test "$(` + g.Command("rev-parse", "HEAD") + `)" = "$commit"`,
		`test "$(` + g.Command("symbolic-ref", "HEAD") + `)" = "refs/heads/$branch"`,
		`test -z "$(` + g.Command(git.Status()...) + `)"`,
		"test ! -e .git/objects/info/alternates",
		"test ! -e /home/hk/workspace",
		"mv /home/hk/.hk-workspace /home/hk/workspace",
		"rm -rf /home/hk/.hk-import",
	}, "\n")
}

func importCloneCommand(branch, commit string) string {
	return strings.Join([]string{
		"branch=" + shellQuote(branch),
		"commit=" + shellQuote(commit),
		git.ImportRemote().Command(
			"clone", "--template=", "--no-checkout", "--no-local", "--no-hardlinks", "--no-tags",
			"--single-branch", "--branch="+branch, "--",
			"/home/hk/.hk-import/project.bundle", "/home/hk/.hk-workspace",
		),
	}, "\n")
}

func importProject(ctx context.Context, dir string, snapshot *repository.Snapshot) error {
	bundle, err := os.Open(snapshot.Bundle)
	if err != nil {
		return err
	}
	defer bundle.Close()
	if err := transferBundle(ctx, dir, bundle); err != nil {
		return fmt.Errorf("transfer bundle: %w", err)
	}
	if err := sshCommand(ctx, dir, importCommand(snapshot.Branch, snapshot.Commit), nil); err != nil {
		return fmt.Errorf("clone bundle: %w", err)
	}
	if snapshot.GitIdentity != nil {
		if err := applyGitIdentity(ctx, dir, *snapshot.GitIdentity); err != nil {
			return fmt.Errorf("configure guest Git identity: %w", err)
		}
	}
	return nil
}

func sshCommand(ctx context.Context, dir, command string, in io.Reader) error {
	_, err := sshOutput(ctx, dir, command, in)
	return err
}

const maxGuestOutput = 64 << 10

var errGuestOutputLimit = errors.New("guest command output exceeds 64 KiB limit")

type limitedGuestOutput struct {
	data     []byte
	overflow bool
}

func (out *limitedGuestOutput) Write(p []byte) (int, error) {
	out.captureUpToLimit(p)
	return len(p), nil
}

func (out *limitedGuestOutput) captureUpToLimit(p []byte) {
	remaining := maxGuestOutput - len(out.data)
	if len(p) > remaining {
		out.overflow = true
		p = p[:remaining]
	}
	out.data = append(out.data, p...)
}

func sshOutput(ctx context.Context, dir, command string, in io.Reader) ([]byte, error) {
	args, err := sshArgs()
	if err != nil {
		return nil, err
	}
	args = append(args, "-T", "hk@habikato", command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Dir, cmd.Stdin = dir, in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out := &limitedGuestOutput{}
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	if out.overflow {
		err = errors.Join(err, errGuestOutputLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("guest command: %w: %s", errors.Join(err, ctx.Err()), strings.TrimSpace(string(out.data)))
	}
	return out.data, ctx.Err()
}

var transferBundle = func(ctx context.Context, dir string, in io.Reader) error {
	return sshCommand(ctx, dir, receiveBundle, in)
}

var applyGitIdentity = configureGitIdentity
