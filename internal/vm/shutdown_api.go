package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errShutdownNotDelivered = errors.New("shutdown request was not delivered")

func requestShutdown(ctx context.Context, state record, dir string) error {
	path := filepath.Join(dir, "api.sock")
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: graceful shutdown API unavailable: %w", errShutdownNotDelivered, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: graceful shutdown API path is not a socket", errShutdownNotDelivered)
	}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dialShutdownSocket(ctx, path, state.PID)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errShutdownNotDelivered, err)
			}
			return conn, nil
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/actions", strings.NewReader(`{"action_type":"SendCtrlAltDel"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		return errors.Join(fmt.Errorf("%w: Firecracker rejected shutdown: HTTP %d: %s", errShutdownNotDelivered, response.StatusCode, body), err)
	}
	return nil
}

func dialShutdownSocket(ctx context.Context, path string, pid int) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	socket, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	var credentialErr error
	err = socket.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credentialErr = err
			return
		}
		if int(credentials.Pid) != pid || credentials.Uid != uint32(os.Getuid()) {
			credentialErr = errors.New("shutdown API is not owned by the verified VM process")
		}
	})
	if err = errors.Join(err, credentialErr); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}
