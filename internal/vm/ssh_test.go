package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestDisposableIdentities(t *testing.T) {
	seen := map[string]bool{}
	for range 128 {
		private, public, err := newKey()
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.ParsePrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey(public)
		if err != nil || !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
			t.Fatal("public and private identity do not match", err)
		}
		fingerprint := ssh.FingerprintSHA256(key)
		if seen[fingerprint] {
			t.Fatal("reused a disposable identity")
		}
		seen[fingerprint] = true
	}
	dir := t.TempDir()
	if err := provisionKeys(dir, "name"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity", "known_hosts", "seed/host_key", "seed/authorized_keys"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("key permissions for %s: %v", name, err)
		}
	}
	client, err := os.ReadFile(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.ReadFile(filepath.Join(dir, "seed/host_key"))
	if err != nil || bytes.Equal(client, host) {
		t.Fatal("client and server must have separate keys", err)
	}
}

func TestShellQuote(t *testing.T) {
	for _, value := range []string{"", "a b", "'single'", "$(id)", "`id`", "$HOME", "a\nb", "x; printf unexpected", "100%"} {
		t.Run(value, func(t *testing.T) {
			output, err := exec.Command("sh", "-c", "printf %s "+shellQuote(value)).Output()
			if err != nil || string(output) != value {
				t.Fatalf("shell changed literal %q: %q, %v", value, output, err)
			}
		})
	}
}

func TestProvisionKeysWiring(t *testing.T) {
	dir := privateTempDir(t)
	if err := provisionKeys(dir, "my-vm"); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(dir, "seed", "hostname"), []byte("my-vm\n"))
	client := readSigner(t, filepath.Join(dir, "identity"))
	host := readSigner(t, filepath.Join(dir, "seed", "host_key"))
	authorized, err := os.ReadFile(filepath.Join(dir, "seed", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(dir, "known_hosts"), append([]byte("habikato "), ssh.MarshalAuthorizedKey(host.PublicKey())...))
	if !bytes.Equal(authorized, ssh.MarshalAuthorizedKey(client.PublicKey())) {
		t.Fatal("guest authorized_keys does not trust the client identity")
	}
	if bytes.Equal(client.PublicKey().Marshal(), host.PublicKey().Marshal()) {
		t.Fatal("client and host share an identity")
	}
	info, err := os.Stat(filepath.Join(dir, "seed"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("seed permissions: %v, %v", info, err)
	}
}

func readSigner(t *testing.T, path string) ssh.Signer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestSSHArgs(t *testing.T) {
	args := sshArgsForExecutable("/private/hk")
	options := sshOptions(t, args)
	for key, want := range map[string]string{
		"ProxyCommand":           "'/private/hk' _proxy vsock.sock",
		"UserKnownHostsFile":     "known_hosts",
		"GlobalKnownHostsFile":   "/dev/null",
		"StrictHostKeyChecking":  "yes",
		"UpdateHostKeys":         "no",
		"IdentitiesOnly":         "yes",
		"IdentityAgent":          "none",
		"BatchMode":              "yes",
		"PasswordAuthentication": "no",
		"ConnectTimeout":         "3",
		"ServerAliveInterval":    "15",
		"ServerAliveCountMax":    "2",
		"EscapeChar":             "none",
	} {
		if got := options[key]; got != want {
			t.Errorf("SSH option %s = %q, want %q", key, got, want)
		}
	}
	if len(args) < 4 || !slices.Equal(args[:4], []string{"-F", "/dev/null", "-i", "identity"}) {
		t.Fatalf("SSH config and identity flags = %q", args)
	}
}

func TestSSHProxyCommandQuotesExecutable(t *testing.T) {
	dir := privateTempDir(t)
	exe := filepath.Join(dir, "hk %h 'quoted' $HOME")
	writeTestFile(t, exe, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"))
	if err := os.Chmod(exe, 0700); err != nil {
		t.Fatal(err)
	}
	proxy := sshOptions(t, sshArgsForExecutable(exe))["ProxyCommand"]
	if !strings.Contains(proxy, "%%h") {
		t.Fatalf("SSH percent expansion was not escaped: %q", proxy)
	}
	output, err := exec.Command("sh", "-c", strings.ReplaceAll(proxy, "%%", "%")).CombinedOutput()
	if err != nil || string(output) != "_proxy\nvsock.sock\n" {
		t.Fatalf("proxy argument boundaries = %q, %v", output, err)
	}
}

func sshOptions(t *testing.T, args []string) map[string]string {
	t.Helper()
	options := map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] != "-o" {
			continue
		}
		i++
		if i == len(args) {
			t.Fatal("SSH -o has no value")
		}
		key, value, ok := strings.Cut(args[i], "=")
		if !ok {
			t.Fatalf("malformed SSH option: %q", args[i])
		}
		if _, duplicate := options[key]; duplicate {
			t.Fatalf("duplicate SSH option: %q", key)
		}
		options[key] = value
	}
	return options
}

func TestCheckSSH(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{name: "ready", code: "0"},
		{name: "rejected", code: "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateTempDir(t)
			bin := privateTempDir(t)
			log := filepath.Join(bin, "args")
			script := "#!/bin/sh\nprintf '%s\\0' \"$PWD\" \"$@\" > \"$HK_TEST_SSH_LOG\"\nprintf 'guest denied\\n' >&2\nexit " + tc.code + "\n"
			writeTestFile(t, filepath.Join(bin, "ssh"), []byte(script))
			if err := os.Chmod(filepath.Join(bin, "ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("HK_TEST_SSH_LOG", log)
			err := checkSSH(t.Context(), dir)
			if tc.code == "0" && err != nil {
				t.Fatal(err)
			}
			if tc.code != "0" {
				assertErrorContains(t, err, "SSH is not ready:")
				assertErrorContains(t, err, "guest denied")
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("SSH exit status lost: %v", err)
				}
			}
			assertReadinessInvocation(t, log, dir)
		})
	}
}

func assertReadinessInvocation(t *testing.T, log, dir string) {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(args) < 4 || args[0] != dir {
		t.Fatalf("SSH working directory/args: %q", args)
	}
	want := []string{"-T", "hk@habikato", `test "$(id -u)" = 1000 && test -w "$HOME"`}
	if !slices.Equal(args[len(args)-3:], want) {
		t.Fatalf("readiness command = %q, want %q", args[len(args)-3:], want)
	}
	options := sshOptions(t, args[1:])
	if options["StrictHostKeyChecking"] != "yes" || options["IdentityAgent"] != "none" {
		t.Fatalf("readiness bypasses pinned identity: %v", options)
	}
}

func TestCheckSSHCancellation(t *testing.T) {
	bin := privateTempDir(t)
	marker := filepath.Join(bin, "started")
	script := "#!/bin/sh\nprintf started > \"$HK_TEST_SSH_MARKER\"\n"
	writeTestFile(t, filepath.Join(bin, "ssh"), []byte(script))
	if err := os.Chmod(filepath.Join(bin, "ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HK_TEST_SSH_MARKER", marker)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkSSH(ctx, privateTempDir(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled readiness = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled readiness started SSH: %v", err)
	}
}

func TestReadProxyReply(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		valid       bool
	}{
		{name: "host port", reply: "OK 1073741824\n", valid: true},
		{name: "maximum uint32", reply: "OK 4294967295\n", valid: true},
		{name: "empty", reply: ""},
		{name: "truncated", reply: "OK 22"},
		{name: "error", reply: "ERR 22\n"},
		{name: "no port", reply: "OK \n"},
		{name: "text port", reply: "OK ssh\n"},
		{name: "negative port", reply: "OK -1\n"},
		{name: "overflow", reply: "OK 4294967296\n"},
		{name: "extra field", reply: "OK 22 extra\n"},
		{name: "carriage return", reply: "OK 22\r\n"},
		{name: "overlong", reply: "OK " + strings.Repeat("1", 61) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.reply)
			err := readProxyReply(reader)
			if tc.valid {
				if err != nil || reader.Len() != 0 {
					t.Fatalf("valid acknowledgement = %v, %d bytes left", err, reader.Len())
				}
				return
			}
			if !strings.Contains(tc.reply, "\n") {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("truncated acknowledgement = %v, want EOF", err)
				}
				return
			}
			assertErrorContains(t, err, "unexpected vsock reply:")
		})
	}
}

func FuzzProxyReply(f *testing.F) {
	f.Add(uint32(1073741824), []byte("SSH-2.0-guest\r\n"))
	f.Add(uint32(4294967295), []byte{0, 255, '\n'})
	f.Add(uint32(0), []byte{})
	f.Fuzz(func(t *testing.T, port uint32, payload []byte) {
		if len(payload) > 4096 {
			t.Skip()
		}
		header := "OK " + strconv.FormatUint(uint64(port), 10) + "\n"
		reader := bytes.NewReader(append([]byte(header), payload...))
		if err := readProxyReply(reader); err != nil {
			t.Fatalf("valid host port %d rejected: %v", port, err)
		}
		remaining, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(remaining, payload) {
			t.Fatalf("acknowledgement consumed SSH bytes: %x, %v; want %x", remaining, err, payload)
		}
		if err := readProxyReply(strings.NewReader(strings.TrimSuffix(header, "\n"))); !errors.Is(err, io.EOF) {
			t.Fatalf("truncated acknowledgement = %v", err)
		}
	})
}

func TestProxyStreamsAndHalfCloses(t *testing.T) {
	banner := []byte("SSH-2.0-guest\r\n\x00\xff")
	input := []byte("client\x00\xff\n")
	path, done := proxyServer(t, func(conn *net.UnixConn) error {
		if _, err := conn.Write(append([]byte("OK 1073741824\n"), banner...)); err != nil {
			return err
		}
		got, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(got, input) {
			return fmt.Errorf("client stream = %x, %v; want %x", got, err, input)
		}
		_, err = io.WriteString(conn, "after EOF")
		return err
	})
	var output bytes.Buffer
	if err := Proxy(path, bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := append(banner, []byte("after EOF")...)
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("proxy output = %x, want %x", output.Bytes(), want)
	}
}

func TestProxyRejectsReplyBeforeSendingInput(t *testing.T) {
	path, done := proxyServer(t, func(conn *net.UnixConn) error {
		if _, err := io.WriteString(conn, "OK invalid\n"); err != nil {
			return err
		}
		data, err := io.ReadAll(conn)
		if err != nil || len(data) != 0 {
			return fmt.Errorf("invalid acknowledgement received client data %q: %v", data, err)
		}
		return nil
	})
	var output bytes.Buffer
	assertErrorContains(t, Proxy(path, strings.NewReader("private SSH data"), &output), "unexpected vsock reply:")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid acknowledgement exposed output %q", output.Bytes())
	}
}

func TestProxyOutputError(t *testing.T) {
	path, done := proxyServer(t, func(conn *net.UnixConn) error {
		_, err := io.WriteString(conn, "OK 1073741824\nSSH bytes")
		return err
	})
	failure := errors.New("output closed")
	if err := Proxy(path, strings.NewReader(""), failingWriter{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("proxy output error = %v, want %v", err, failure)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func proxyServer(t *testing.T, serve func(*net.UnixConn) error) (string, <-chan error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hk-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "socket")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	done := make(chan error, 1)
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
			t.Error("proxy server did not finish")
		}
	})
	go func() {
		defer close(finished)
		conn, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- err
			return
		}
		request := make([]byte, len("CONNECT 22\n"))
		if _, err := io.ReadFull(conn, request); err != nil || string(request) != "CONNECT 22\n" {
			done <- fmt.Errorf("vsock request = %q, %v", request, err)
			return
		}
		done <- serve(conn)
	}()
	return path, done
}
