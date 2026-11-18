package vm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func newKey() ([]byte, []byte, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "habikat disposable key")
	if err != nil {
		return nil, nil, err
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(key), nil
}

func provisionKeys(dir, name string) error {
	client, authorized, err := newKey()
	if err != nil {
		return err
	}
	host, hostPublic, err := newKey()
	if err != nil {
		return err
	}
	seed := filepath.Join(dir, "seed")
	if err := os.Mkdir(seed, 0700); err != nil {
		return err
	}
	for path, data := range map[string][]byte{
		"identity":             client,
		"known_hosts":          append([]byte("habikat "), hostPublic...),
		"seed/authorized_keys": authorized,
		"seed/host_key":        host,
		"seed/hostname":        []byte(name + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, path), data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func sshArgs() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return sshArgsForExecutable(exe), nil
}

func sshArgsForExecutable(exe string) []string {
	return []string{"-F", "/dev/null", "-i", "identity",
		"-o", "ProxyCommand=" + quoteSSHExecutable(exe) + " _proxy vsock.sock",
		"-o", "UserKnownHostsFile=known_hosts",
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "BatchMode=yes", "-o", "PasswordAuthentication=no",
		"-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2",
		"-o", "LogLevel=ERROR", "-o", "EscapeChar=none"}
}

func quoteSSHExecutable(exe string) string {
	return strings.ReplaceAll(shellQuote(exe), "%", "%%")
}

func checkSSH(ctx context.Context, dir string) error {
	args, err := sshArgs()
	if err != nil {
		return err
	}
	args = append(args, "-T", "hk@habikat", "test \"$(id -u)\" = 1000 && test -w \"$HOME\"")
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("SSH is not ready: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func Attach(name string) error {
	dir, lock, err := lockState(context.Background(), name)
	if err != nil {
		return err
	}
	state, err := readRecord(dir)
	if err == nil {
		err = vmNotRunning(name, dir, state)
	}
	err = errors.Join(err, lock.Close())
	if err != nil {
		return err
	}
	args, err := sshArgs()
	if err != nil {
		return err
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		args = append(args, "-tt")
	} else {
		args = append(args, "-T")
	}
	args = append(args, "hk@habikat")
	if command := attachCommand(state.Project); command != "" {
		args = append(args, command)
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	return syscall.Exec(sshPath, append([]string{"ssh"}, args...), os.Environ())
}

func Proxy(path string, in io.Reader, out io.Writer) error {
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "CONNECT 22\n"); err != nil {
		return err
	}
	if err := readProxyReply(conn); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	go func() {
		_, copyErr := io.Copy(conn, in)
		if copyErr != nil {
			conn.Close()
			return
		}
		conn.(*net.UnixConn).CloseWrite()
	}()
	_, err = io.Copy(out, conn)
	return err
}

func readProxyReply(reader io.Reader) error {
	line, err := readUnbufferedLine(reader, 64)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(line), "OK ") || line[len(line)-1] != '\n' {
		return fmt.Errorf("unexpected vsock reply: %q", line)
	}
	if _, err := strconv.ParseUint(string(line[3:len(line)-1]), 10, 32); err != nil {
		return fmt.Errorf("unexpected vsock reply: %q: %w", line, err)
	}
	return nil
}

func readUnbufferedLine(reader io.Reader, limit int) ([]byte, error) {
	var line []byte
	var one [1]byte
	for len(line) < limit {
		if _, err := io.ReadFull(reader, one[:]); err != nil {
			return nil, err
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			break
		}
	}
	return line, nil
}

func attachCommand(project *Project) string {
	if project == nil {
		return ""
	}
	return `cd /home/hk/workspace || { echo 'hk: project workspace /home/hk/workspace is missing or inaccessible' >&2; exit 1; }; exec /bin/sh`
}
