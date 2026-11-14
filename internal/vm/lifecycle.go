package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	return filepath.Abs(filepath.Join(cache, "habikato", "image"))
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
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != manifest[name] {
			return fmt.Errorf("SHA-256 mismatch for %s", name)
		}
	}
	return nil
}

func Create(ctx context.Context, name string) (result error) {
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
	defer func() { result = errors.Join(result, lock.Close()) }()
	if len(filepath.Join(dir, "vsock.sock")) > 107 {
		return errors.New("state path is too long for a Unix socket; choose a shorter HK_STATE_DIR")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create VM directory (names must be unused): %w", err)
	}
	state := record{}
	defer func() {
		if result != nil {
			result = errors.Join(result, cleanupFailedCreate(state, dir, name))
		}
	}()
	if err := saveRecord(dir, state); err != nil {
		return err
	}
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
	config := map[string]any{
		"boot-source":    map[string]any{"kernel_image_path": filepath.Join(images, "vmlinux"), "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init"},
		"machine-config": map[string]any{"vcpu_count": 1, "mem_size_mib": 256},
		"drives": []map[string]any{
			{"drive_id": "root", "path_on_host": filepath.Join(dir, "rootfs.ext4"), "is_root_device": true, "is_read_only": false},
			{"drive_id": "seed", "path_on_host": filepath.Join(dir, "seed.ext4"), "is_root_device": false, "is_read_only": true},
		},
		"vsock": map[string]any{"guest_cid": 3, "uds_path": filepath.Join(dir, "vsock.sock")},
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "console.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	state.PID, err = startDetachedFirecracker(ctx, images, configPath, log)
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
			state.Ready = true
			return saveRecord(dir, state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for guest access: %w (%v)", ctx.Err(), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func cleanupFailedCreate(state record, dir, name string) error {
	if err := stopProcess(state, dir); err != nil {
		return fmt.Errorf("cleanup failed; run hk delete %s: %w", name, err)
	}
	return os.RemoveAll(dir)
}

func startDetachedFirecracker(ctx context.Context, images, configPath string, log *os.File) (int, error) {
	cmd := exec.Command(filepath.Join(images, "firecracker"), "--no-api", "--config-file", configPath)
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

func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if len(fields) < 20 || fields[0] == "Z" {
		return "", errors.New("process has exited")
	}
	return fields[19], nil
}

func ownsConfig(pid int, dir string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(data), "\x00")
	return len(args) == 5 && filepath.Base(args[0]) == "firecracker" && args[1] == "--no-api" && args[2] == "--config-file" && args[3] == filepath.Join(dir, "config.json")
}

func sameProcess(state record, dir string) bool {
	if state.PID <= 0 || state.StartTime == "" {
		return false
	}
	start, err := processStart(state.PID)
	return err == nil && start == state.StartTime && ownsConfig(state.PID, dir)
}

func stopProcess(state record, dir string) error {
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

func Delete(ctx context.Context, name string) error {
	return deleteState(ctx, name, stopProcess)
}

func deleteState(ctx context.Context, name string, stop func(record, string) error) (result error) {
	dir, lock, err := lockState(ctx, name)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	state, err := readRecord(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Lstat(dir); errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
		}
		return err
	}
	if err := stop(state, dir); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
