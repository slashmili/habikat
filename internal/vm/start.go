package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const startReadyTimeout = 2 * time.Minute

type StartResult struct {
	AlreadyRunning bool
}

type readinessCheck func(context.Context, string) error

func Start(ctx context.Context, name string) (StartResult, error) {
	return startVM(ctx, name, checkSSH, startReadyTimeout)
}

func startVM(ctx context.Context, name string, ready readinessCheck, timeout time.Duration) (_ StartResult, result error) {
	dir, operation, err := lockLifecycle(ctx, name)
	if err != nil {
		return StartResult{}, err
	}
	defer func() { result = errors.Join(result, operation.Close()) }()
	state, err := readStopRecord(dir)
	if err != nil {
		return StartResult{}, fmt.Errorf("open VM %s: %w", name, err)
	}
	if state.Creating {
		return StartResult{}, fmt.Errorf("VM %s was not fully created and cannot be started; run hk delete %s to remove it", name, name)
	}
	live, err := reconcileStartProcess(name, dir, state)
	if err != nil {
		return StartResult{}, err
	}
	if live != nil && live.Ready {
		return StartResult{AlreadyRunning: true}, nil
	}
	if live != nil {
		return StartResult{}, finishInterruptedStart(ctx, name, dir, *live, ready, timeout)
	}
	return StartResult{}, bootStoppedVM(ctx, name, dir, state, ready, timeout)
}

func reconcileStartProcess(name, dir string, state record) (*record, error) {
	processes, err := processesForVM(dir)
	if err != nil {
		return nil, err
	}
	if len(processes) == 0 {
		return nil, nil
	}
	if len(processes) != 1 {
		return nil, fmt.Errorf("multiple processes use VM %s; refusing to start a second writer; state retained", name)
	}
	if state.PID == 0 {
		return adoptUnpublishedProcess(name, dir, state, processes[0])
	}
	if state.PID != processes[0].PID || !sameProcess(state, dir) {
		return nil, fmt.Errorf("VM %s has a live process that does not match its recorded identity; refusing to start a second writer; state retained", name)
	}
	if state.Stopping {
		return nil, fmt.Errorf("VM %s is shutting down; wait for it to stop or run hk stop %s, then run hk start %s", name, name, name)
	}
	return &state, nil
}

func adoptUnpublishedProcess(name, dir string, state, process record) (*record, error) {
	if err := requireOpenRootDisk(process.PID, dir); err != nil {
		return nil, fmt.Errorf("VM %s has an unrecorded process; refusing to start a second writer: %w", name, err)
	}
	boot, err := hostBootID()
	if err != nil {
		return nil, err
	}
	state.PID, state.StartTime, state.BootID = process.PID, process.StartTime, boot
	state.Ready, state.Stopping = false, false
	if err := saveRecord(dir, state); err != nil {
		return nil, err
	}
	return &state, nil
}

func finishInterruptedStart(ctx context.Context, name, dir string, state record, ready readinessCheck, timeout time.Duration) error {
	if err := waitStartReady(ctx, dir, state, ready, timeout, consoleSince{dir: dir}); err != nil {
		return fmt.Errorf("VM %s was launched by an earlier interrupted start and is not ready; its process was left running and its disk, files, and keys are preserved; run hk start %s again or hk stop --force %s: %w", name, name, name, err)
	}
	return markReady(dir, state)
}

func bootStoppedVM(ctx context.Context, name, dir string, state record, ready readinessCheck, timeout time.Duration) error {
	launch, err := prepareLaunch(dir, state)
	if err != nil {
		return fmt.Errorf("cannot start VM %s; its disk, files, and keys are preserved: %w", name, err)
	}
	state, err = recordStarting(dir, state, launch)
	if err != nil {
		return err
	}
	attempt, err := launchFirecracker(ctx, dir, state, launch)
	if err != nil {
		return errors.Join(startFailed(name, err), abandonStart(name, dir, attempt.state))
	}
	if err := waitStartReady(ctx, dir, attempt.state, ready, timeout, attempt.console); err != nil {
		return errors.Join(startFailed(name, err), abandonStart(name, dir, attempt.state))
	}
	return markReady(dir, attempt.state)
}

func startFailed(name string, err error) error {
	return fmt.Errorf("start VM %s failed; its disk, files, and keys are preserved: %w", name, err)
}

func prepareLaunch(dir string, state record) (launchArtifacts, error) {
	launch := *state.Launch
	if err := verifyLaunchArtifacts(launch); err != nil {
		return launchArtifacts{}, err
	}
	if err := validateSocketPaths(dir); err != nil {
		return launchArtifacts{}, err
	}
	if err := requireUnusedDisks(dir); err != nil {
		return launchArtifacts{}, err
	}
	return launch, removeStaleSockets(dir)
}

func requireUnusedDisks(dir string) error {
	var disks []os.FileInfo
	for _, name := range []string{"rootfs.ext4", "seed.ext4"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("VM disk %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("VM disk %s must be a regular file", name)
		}
		disks = append(disks, info)
	}
	pid, err := processHoldingFile(disks)
	if err != nil {
		return err
	}
	if pid != 0 {
		return fmt.Errorf("VM disk is open in process %d; refusing to start a second writer", pid)
	}
	return nil
}

func processHoldingFile(files []os.FileInfo) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		held, err := processHoldsAny(pid, files)
		if err != nil {
			return 0, err
		}
		if held {
			return pid, nil
		}
	}
	return 0, nil
}

func processHoldsAny(pid int, files []os.FileInfo) (bool, error) {
	descriptors := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(descriptors)
	if processDisappeared(err) || errors.Is(err, os.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(descriptors, entry.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if os.SameFile(file, info) {
				return true, nil
			}
		}
	}
	return false, nil
}

func removeStaleSockets(dir string) error {
	for _, name := range []string{"api.sock", "vsock.sock"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("%s is not a socket; refusing to remove it", path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func recordStarting(dir string, state record, launch launchArtifacts) (record, error) {
	boot, err := hostBootID()
	if err != nil {
		return record{}, err
	}
	state.PID, state.StartTime, state.Ready, state.Stopping = 0, "", false, false
	state.BootID = boot
	state.Launch = &launch
	return state, saveRecord(dir, state)
}

type startAttempt struct {
	state   record
	console consoleSince
}

func launchFirecracker(ctx context.Context, dir string, state record, launch launchArtifacts) (startAttempt, error) {
	attempt := startAttempt{state: state, console: consoleSince{dir: dir}}
	if err := writeLaunchConfig(dir, launch.Kernel.Path); err != nil {
		return attempt, err
	}
	log, err := openPrivateRegular(filepath.Join(dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return attempt, fmt.Errorf("open console log: %w", err)
	}
	defer log.Close()
	if attempt.console.offset, err = log.Seek(0, io.SeekEnd); err != nil {
		return attempt, err
	}
	pid, err := startDetachedFirecracker(ctx, launch.Firecracker.Path, filepath.Join(dir, "config.json"), log)
	if err != nil {
		return attempt, err
	}
	start, err := processStart(pid)
	if err != nil {
		return attempt, fmt.Errorf("read Firecracker identity: %w", err)
	}
	attempt.state.PID, attempt.state.StartTime = pid, start
	return attempt, saveRecord(dir, attempt.state)
}

func openPrivateRegular(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s must be a regular file", path)
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

type consoleSince struct {
	dir    string
	offset int64
}

func (console consoleSince) tail() string {
	file, err := openPrivateRegular(filepath.Join(console.dir, "console.log"), os.O_RDONLY)
	if err != nil {
		return fmt.Sprintf("(console log unavailable: %v)", err)
	}
	defer file.Close()
	const limit = 6000
	info, err := file.Stat()
	if err != nil {
		return fmt.Sprintf("(console log unavailable: %v)", err)
	}
	offset := max(console.offset, info.Size()-limit)
	data, err := io.ReadAll(io.NewSectionReader(file, offset, limit))
	if err != nil {
		return fmt.Sprintf("(console log unavailable: %v)", err)
	}
	return string(data)
}

func waitStartReady(ctx context.Context, dir string, state record, ready readinessCheck, timeout time.Duration, console consoleSince) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		if !sameProcess(state, dir) {
			return fmt.Errorf("Firecracker exited before SSH was ready:\n%s", console.tail())
		}
		if last = ready(ctx, dir); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for guest access: %w (%v)", ctx.Err(), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func markReady(dir string, state record) error {
	state.Ready = true
	return saveRecord(dir, state)
}

func abandonStart(name, dir string, state record) error {
	if err := stopProcess(state, dir); err != nil {
		return fmt.Errorf("could not stop the VM process launched by this start; its identity is retained and blocks another start; run hk stop --force %s: %w", name, err)
	}
	processes, err := processesForVM(dir)
	if err != nil {
		return err
	}
	if len(processes) != 0 {
		return fmt.Errorf("VM %s process %d is still alive after a failed start; its identity is retained; run hk stop --force %s", name, processes[0].PID, name)
	}
	if err := removeStaleSockets(dir); err != nil {
		return err
	}
	return recordStopped(dir, state)
}
