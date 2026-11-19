package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/slashmili/habikat/internal/repository"
	"golang.org/x/sys/unix"
)

func imageDir() (string, error) {
	if dir := os.Getenv("HK_IMAGE_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(cache, "habikat", "image"))
}

func verifyImages(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("build the VM image with images/build.py first: %w", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("read image manifest: %w", err)
	}
	for _, name := range []string{"firecracker", "vmlinux", "rootfs.ext4"} {
		if len(manifest[name]) != 64 {
			return fmt.Errorf("missing SHA-256 for %s", name)
		}
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		digest, err := readerSHA256(file)
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		if digest != manifest[name] {
			return fmt.Errorf("SHA-256 mismatch for %s", name)
		}
	}
	return nil
}

type CreateOptions struct {
	WithGitConfig bool
}

func Create(ctx context.Context, name string, options CreateOptions) (project *Project, result error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	sourceDirectory, err := repository.SourceDirectory(ctx, ".")
	if err != nil {
		return nil, err
	}
	snapshot, err := captureProject(ctx, ".", options)
	if err != nil {
		return nil, err
	}
	if snapshot != nil {
		defer func() { result = errors.Join(result, snapshot.Close()) }()
	}
	err = create(ctx, name, snapshot, sourceDirectory)
	if err != nil {
		return nil, err
	}
	if snapshot != nil {
		return &Project{Branch: snapshot.Branch, Commit: snapshot.Commit, Workspace: Workspace}, nil
	}
	return nil, nil
}

func captureProject(ctx context.Context, dir string, options CreateOptions) (*repository.Snapshot, error) {
	var identity *repository.GitIdentity
	var err error
	if options.WithGitConfig {
		identity, err = repository.ReadGitIdentity(ctx, dir)
		if err != nil {
			return nil, err
		}
	}
	snapshot, err := repository.Capture(ctx, dir)
	if err != nil {
		return nil, err
	}
	if snapshot != nil {
		snapshot.GitIdentity = identity
	}
	return snapshot, nil
}

func create(ctx context.Context, name string, snapshot *repository.Snapshot, sourceDirectory string) (result error) {
	if err := ValidateName(name); err != nil {
		return err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errors.New("this slice supports Linux x86_64 only")
	}
	for _, tool := range []string{"ssh", "mkfs.ext4", "cp"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required host tool: %w", err)
		}
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("Firecracker needs read/write access to /dev/kvm: %w", err)
	}
	if err := kvm.Close(); err != nil {
		return err
	}
	images, err := imageDir()
	if err != nil {
		return err
	}
	if err := verifyImages(images); err != nil {
		return err
	}
	dir, lock, err := lockState(ctx, name)
	if err != nil {
		return err
	}
	defer func() {
		if lock != nil {
			result = errors.Join(result, lock.Close())
		}
	}()
	if err := validateSocketPaths(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create VM directory (names must be unused): %w", err)
	}
	state := record{SourceDirectory: sourceDirectory}
	operation, err := lockVMOperation(dir)
	if err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	defer operation.Close()
	defer func() {
		if result != nil {
			result = errors.Join(result, cleanupFailedCreate(state, dir, name))
		}
	}()
	state, err = initialCreateRecord(images, sourceDirectory)
	if err != nil {
		return err
	}
	if err := saveRecord(dir, state); err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	lock = nil
	if err := provisionKeys(dir, name); err != nil {
		return err
	}
	copy := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", "--", filepath.Join(images, "rootfs.ext4"), filepath.Join(dir, "rootfs.ext4"))
	if output, err := copy.CombinedOutput(); err != nil {
		return fmt.Errorf("copy root disk: %w: %s", err, output)
	}
	seed := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-d", filepath.Join(dir, "seed"), filepath.Join(dir, "seed.ext4"), "2048")
	if output, err := seed.CombinedOutput(); err != nil {
		return fmt.Errorf("build seed disk: %w: %s", err, output)
	}
	if err := os.RemoveAll(filepath.Join(dir, "seed")); err != nil {
		return err
	}
	if err := writeLaunchConfig(dir, state.Launch.Kernel.Path); err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.json")
	log, err := os.OpenFile(filepath.Join(dir, "console.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	state.PID, err = startDetachedFirecracker(ctx, state.Launch.Firecracker.Path, configPath, log)
	if err != nil {
		return err
	}
	state.StartTime, err = processStart(state.PID)
	if err != nil {
		return fmt.Errorf("read Firecracker identity: %w", err)
	}
	if err := saveRecord(dir, state); err != nil {
		return err
	}
	var last error
	for {
		if !sameProcess(state, dir) {
			logData, logErr := os.ReadFile(filepath.Join(dir, "console.log"))
			if len(logData) > 6000 {
				logData = logData[len(logData)-6000:]
			}
			return errors.Join(fmt.Errorf("Firecracker exited before SSH was ready:\n%s", logData), logErr)
		}
		if last = checkSSH(ctx, dir); last == nil {
			if snapshot != nil {
				if err := importProject(ctx, dir, snapshot); err != nil {
					return fmt.Errorf("import project (check available guest disk space): %w", err)
				}
				state.Project = &Project{Branch: snapshot.Branch, Commit: snapshot.Commit, Workspace: Workspace}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			state.Creating, state.Ready = false, true
			return saveRecord(dir, state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for guest access: %w (%v)", ctx.Err(), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func initialCreateRecord(images, sourceDirectory string) (record, error) {
	state := record{Creating: true, SourceDirectory: sourceDirectory}
	var err error
	if state.Shutdown, err = imageShutdownCapability(images); err != nil {
		return record{}, err
	}
	launch, err := imageLaunchArtifacts(images)
	if err != nil {
		return record{}, err
	}
	state.Launch = &launch
	if state.BootID, err = hostBootID(); err != nil {
		return record{}, err
	}
	if state.InstanceID, err = newInstanceID(); err != nil {
		return record{}, err
	}
	return state, nil
}

func cleanupFailedCreate(state record, dir, name string) error {
	if err := stopProcess(state, dir); err != nil {
		return fmt.Errorf("cleanup failed; run hk delete %s: %w", name, err)
	}
	return os.RemoveAll(dir)
}

func startDetachedFirecracker(ctx context.Context, firecracker, configPath string, log *os.File) (int, error) {
	cmd := exec.Command(firecracker, "--api-sock", filepath.Join(filepath.Dir(configPath), "api.sock"), "--config-file", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

var errProcessExited = errors.New("process has exited")

func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if len(fields) < 20 {
		return "", errors.New("invalid process identity")
	}
	if fields[0] == "Z" {
		return "", errProcessExited
	}
	return fields[19], nil
}

func ownsConfig(pid int, dir string) bool {
	owned, err := processOwnsConfig(pid, dir)
	return err == nil && owned
}

func processOwnsConfig(pid int, dir string) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, err
	}
	args := strings.Split(string(data), "\x00")
	if filepath.Base(args[0]) != "firecracker" {
		return false, nil
	}
	if len(args) == 5 {
		return args[1] == "--no-api" && args[2] == "--config-file" && args[3] == filepath.Join(dir, "config.json"), nil
	}
	return len(args) == 6 && args[1] == "--api-sock" && args[2] == filepath.Join(dir, "api.sock") && args[3] == "--config-file" && args[4] == filepath.Join(dir, "config.json"), nil
}

func sameProcess(state record, dir string) bool {
	if state.PID <= 0 || state.StartTime == "" {
		return false
	}
	if state.BootID != "" {
		boot, err := hostBootID()
		if err != nil || boot != state.BootID {
			return false
		}
	}
	start, err := processStart(state.PID)
	return err == nil && start == state.StartTime && ownsConfig(state.PID, dir)
}

func vmNotRunning(name, dir string, state record) error {
	switch {
	case !sameProcess(state, dir):
		return fmt.Errorf("VM %s is stopped; run hk start %s; its disk, files, and keys are preserved", name, name)
	case state.Stopping:
		return fmt.Errorf("VM %s is shutting down", name)
	case !state.Ready:
		return fmt.Errorf("VM %s is still starting", name)
	}
	return nil
}

func stopProcess(state record, dir string) error {
	if state.BootID != "" {
		boot, err := hostBootID()
		if err != nil {
			return err
		}
		if boot != state.BootID {
			processes, err := processesForVM(dir)
			if err != nil {
				return err
			}
			if len(processes) != 0 {
				return errors.New("host boot identity conflicts with a live VM; state retained")
			}
			return nil
		}
	}
	if state.PID == 0 || state.StartTime == "" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		return stopUnrecordedProcesses(dir, entries)
	}
	fd, err := unix.PidfdOpen(state.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open process handle: %w", err)
	}
	defer unix.Close(fd)
	if !sameProcess(state, dir) {
		return nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	for {
		n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 5000)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("VM did not exit; state retained")
		}
		return nil
	}
}

func stopUnrecordedProcesses(dir string, entries []os.DirEntry) error {
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !ownsConfig(pid, dir) {
			continue
		}
		start, err := processStart(pid)
		if err != nil {
			continue
		}
		if err := stopProcess(record{PID: pid, StartTime: start}, dir); err != nil {
			return err
		}
	}
	return nil
}

func Delete(ctx context.Context, name string, confirm func() error) error {
	return deleteConfirmed(ctx, name, confirm, guestHasChanges, stopProcess)
}

func deleteState(ctx context.Context, name string, stop func(record, string) error) error {
	return deleteConfirmed(ctx, name, nil, guestHasChanges, stop)
}

func deleteConfirmed(ctx context.Context, name string, confirm func() error, inspect func(context.Context, string, record) (bool, error), stop func(record, string) error) (result error) {
	dir, lock, err := lockState(ctx, name)
	if err != nil {
		return err
	}
	defer func() {
		if lock != nil {
			result = errors.Join(result, lock.Close())
		}
	}()
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	operation, err := lockVMOperation(dir)
	if err != nil {
		return err
	}
	defer operation.Close()
	state, err := readRecord(dir)
	if err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	lock = nil
	if confirm != nil {
		changed, err := inspect(ctx, dir, state)
		if err != nil {
			return fmt.Errorf("cannot check VM %s for changes; use hk delete --force %s to discard its files and commits: %w", name, name, err)
		}
		if !changed {
			confirm = nil
		}
	}
	if confirm != nil {
		pinnedDirectory, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer pinnedDirectory.Close()
		original, err := pinnedDirectory.Stat()
		if err != nil {
			return err
		}
		if err := confirm(); err != nil {
			return err
		}
		_, lock, err = lockState(ctx, name)
		if err != nil {
			return err
		}
		state, err = readUnchangedVM(dir, original, state)
		if err != nil {
			return err
		}
		if err := lock.Close(); err != nil {
			return err
		}
		lock = nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := stop(state, dir); err != nil {
		return err
	}
	processes, err := processesForVM(dir)
	if err != nil {
		return err
	}
	if len(processes) != 0 {
		return errors.New("VM process is still alive; storage retained")
	}
	return discardVMStorage(ctx, name, dir)
}

const deletingPrefix = ".deleting-"

var removeVMStorage = os.RemoveAll

func discardVMStorage(ctx context.Context, name, dir string) error {
	trash, err := moveVMStorageAside(ctx, name, dir)
	if err != nil {
		return err
	}
	if err := removeVMStorage(trash); err != nil {
		return fmt.Errorf("remove storage of deleted VM %s at %s: %w", name, trash, err)
	}
	return nil
}

func moveVMStorageAside(ctx context.Context, name, dir string) (_ string, result error) {
	_, lock, err := lockState(ctx, name)
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	suffix, err := newInstanceID()
	if err != nil {
		return "", err
	}
	trash := filepath.Join(filepath.Dir(dir), deletingPrefix+name+"-"+suffix)
	return trash, os.Rename(dir, trash)
}

func readUnchangedVM(dir string, original os.FileInfo, state record) (record, error) {
	current, err := os.Lstat(dir)
	if err != nil || !os.SameFile(original, current) {
		return record{}, errors.New("VM was replaced while awaiting confirmation; deletion aborted")
	}
	next, err := readRecord(dir)
	if err != nil {
		return record{}, err
	}
	if next.PID != state.PID || next.StartTime != state.StartTime {
		return record{}, errors.New("VM instance changed while awaiting confirmation; deletion aborted")
	}
	return next, nil
}
