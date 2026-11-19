package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const shutdownTimeout = 30 * time.Second

type shutdownRequest func(context.Context, record, string) error

func Stop(ctx context.Context, name string, force bool) error {
	return stopVM(ctx, name, force, shutdownTimeout, requestShutdown)
}

func stopVM(ctx context.Context, name string, force bool, timeout time.Duration, request shutdownRequest) (result error) {
	dir, operation, err := lockLifecycle(ctx, name)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, operation.Close()) }()
	state, err := readStopRecord(dir)
	if err != nil {
		return err
	}
	current, err := reconcileStopProcess(dir, state)
	if err != nil {
		return err
	}
	if current.PID == 0 {
		return recordStopped(dir, state)
	}
	fd, err := unix.PidfdOpen(current.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return recordStopped(dir, current)
	}
	if err != nil {
		return fmt.Errorf("open VM process handle: %w", err)
	}
	defer unix.Close(fd)
	if !sameProcess(current, dir) {
		return errors.New("VM process changed before shutdown; state retained")
	}
	if err := requireOpenRootDisk(current.PID, dir); err != nil {
		return err
	}
	pendingShutdown := current.Stopping
	current.Stopping = true
	if err := saveRecord(dir, current); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := sendShutdown(waitCtx, fd, force, current, dir, request); err != nil {
		return shutdownRequestFailed(dir, current, pendingShutdown, err)
	}
	if err := waitProcessExit(waitCtx, fd); err != nil {
		return fmt.Errorf("waiting for VM shutdown; shutdown may still complete; state retained; use hk stop --force %s if the guest does not shut down: %w", name, err)
	}
	return recordStopped(dir, current)
}

func sendShutdown(ctx context.Context, fd int, force bool, state record, dir string, request shutdownRequest) error {
	if !force {
		return request(ctx, state, dir)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func shutdownRequestFailed(dir string, state record, pendingShutdown bool, err error) error {
	if !errors.Is(err, errShutdownNotDelivered) {
		return fmt.Errorf("shutdown request failed; shutdown may still complete; state retained: %w", err)
	}
	if pendingShutdown {
		return fmt.Errorf("shutdown request was not accepted; an earlier shutdown request may still complete; state retained: %w", err)
	}
	state.Stopping = false
	if saveErr := saveRecord(dir, state); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return fmt.Errorf("shutdown request was not accepted; VM is still running: %w", err)
}

func lockLifecycle(ctx context.Context, name string) (string, *os.File, error) {
	if err := ValidateName(name); err != nil {
		return "", nil, err
	}
	root, err := stateHome()
	if err != nil {
		return "", nil, err
	}
	if err := privateDir(root); err != nil {
		return "", nil, fmt.Errorf("open VM %s: %w", name, err)
	}
	dir, global, err := lockState(ctx, name)
	if err != nil {
		return "", nil, err
	}
	operation, err := lockExistingVM(name, dir)
	err = errors.Join(err, global.Close())
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		if operation != nil {
			err = errors.Join(err, operation.Close())
		}
		return "", nil, err
	}
	return dir, operation, nil
}

func lockExistingVM(name, dir string) (*os.File, error) {
	operation, err := lockVMOperation(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("VM %s not found: %w", name, err)
	}
	return operation, err
}

func lockVMOperation(dir string) (*os.File, error) {
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(dir, ".lifecycle"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "VM lifecycle lock")
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Nlink != 1) {
		err = errors.New("invalid VM lifecycle lock")
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		err = errors.New("another lifecycle operation is in progress for this VM")
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func readStopRecord(dir string) (record, error) {
	file, err := openPrivateDirectory(unix.AT_FDCWD, dir)
	if err != nil {
		return record{}, err
	}
	defer file.Close()
	return readListedRecord(file)
}

func reconcileStopProcess(dir string, state record) (record, error) {
	boot, err := hostBootID()
	if err != nil {
		return record{}, err
	}
	processes, err := processesForVM(dir)
	if err != nil {
		return record{}, err
	}
	if len(processes) == 0 {
		state.PID, state.StartTime, state.Ready, state.Stopping = 0, "", false, false
		return state, nil
	}
	if len(processes) != 1 {
		return record{}, errors.New("multiple VM processes use this configuration; state retained")
	}
	process := processes[0]
	if state.BootID != boot {
		return record{}, errors.New("host boot identity disagrees with a live VM process; refusing to signal it")
	}
	if state.PID != 0 && (state.PID != process.PID || state.StartTime != process.StartTime) {
		return record{}, errors.New("VM process identity changed; refusing to signal it")
	}
	state.PID, state.StartTime, state.BootID = process.PID, process.StartTime, boot
	return state, nil
}

func processesForVM(dir string) ([]record, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []record
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		owned, err := processOwnsConfig(pid, dir)
		if processDisappeared(err) || errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !owned {
			continue
		}
		start, err := processStart(pid)
		if processDisappeared(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		processes = append(processes, record{PID: pid, StartTime: start})
	}
	return processes, nil
}

func processDisappeared(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) || errors.Is(err, errProcessExited)
}

func requireOpenRootDisk(pid int, dir string) error {
	disk, err := os.Lstat(filepath.Join(dir, "rootfs.ext4"))
	if err != nil {
		return err
	}
	if !disk.Mode().IsRegular() {
		return errors.New("root disk must be a regular file")
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := os.Stat(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()))
		if err == nil && os.SameFile(disk, info) {
			return nil
		}
	}
	return errors.New("VM process does not hold its recorded root disk; refusing to signal process")
}

func recordStopped(dir string, state record) error {
	if state.PID == 0 && state.StartTime == "" && !state.Ready && !state.Stopping {
		return nil
	}
	state.PID, state.StartTime, state.Ready, state.Stopping = 0, "", false, false
	return saveRecord(dir, state)
}

const exitPollMillis = 20

func waitProcessExit(ctx context.Context, fd int) error {
	for {
		n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, exitPollMillis)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if n > 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
