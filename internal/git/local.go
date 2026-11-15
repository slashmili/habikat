package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Local struct {
	Dir    string
	config []string
}

func HostEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "LC_ALL" || key == "LANGUAGE" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, commonEnvironment()...)
	return append(env, "LC_ALL=C", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0")
}

func (g Local) Run(ctx context.Context, args ...string) ([]byte, error) {
	return g.Input(ctx, nil, args...)
}

func (g Local) Input(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("git command is required")
	}
	config := append(hostConfig(), g.config...)
	cmd := exec.CommandContext(ctx, "git", arguments(config, args)...)
	cmd.Env, cmd.Dir = HostEnvironment(), g.Dir
	cmd.Stdin = bytes.NewReader(input)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s",
			args[0], errors.Join(err, ctx.Err()), strings.TrimSpace(stderr.String()),
		)
	}
	return out, nil
}

func (g *Local) DisableFilter(key string) {
	for _, suffix := range []string{".clean", ".smudge", ".process", ".required"} {
		if strings.HasSuffix(strings.ToLower(key), suffix) {
			override := ""
			if suffix == ".required" {
				override = "false"
			}
			g.config = append(g.config, key+"="+override)
		}
	}
}
