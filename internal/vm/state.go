package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var validCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return errors.New("name must be 1–32 lowercase letters, digits or hyphens, starting with a letter")
	}
	return nil
}

const Workspace = "/home/hk/workspace"

type Project struct {
	Branch    string `json:"branch"`
	Commit    string `json:"commit"`
	Workspace string `json:"workspace"`
}

type record struct {
	Project   *Project `json:"project,omitempty"`
	PID       int      `json:"pid"`
	StartTime string   `json:"start_time"`
	Ready     bool     `json:"ready"`
}

func stateHome() (string, error) {
	if path := os.Getenv("HK_STATE_DIR"); path != "" {
		return filepath.Abs(path)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Abs(filepath.Join(base, "habikato"))
}

func privateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s must be a real directory owned by you with mode 0700", path)
	}
	return nil
}

func lockState(ctx context.Context, name string) (string, *os.File, error) {
	if err := ValidateName(name); err != nil {
		return "", nil, err
	}
	root, err := stateHome()
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	if err := privateDir(root); err != nil {
		return "", nil, err
	}
	fd, err := unix.Open(filepath.Join(root, ".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, err
	}
	f := os.NewFile(uintptr(fd), ".lock")
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return filepath.Join(root, name), f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return "", nil, errors.Join(err, f.Close())
		}
		select {
		case <-ctx.Done():
			return "", nil, errors.Join(ctx.Err(), f.Close())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func saveRecord(dir string, state record) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "state.tmp"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "state.tmp"), filepath.Join(dir, "state.json"))
}

func readRecord(dir string) (record, error) {
	if err := privateDir(dir); err != nil {
		return record{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return record{}, err
	}
	var state *record
	if err := json.Unmarshal(data, &state); err != nil {
		return record{}, fmt.Errorf("read VM state: %w", err)
	}
	if state == nil {
		return record{}, errors.New("read VM state: expected an object")
	}
	if state.PID < 0 || (state.PID != 0 && state.StartTime == "") || (state.Ready && state.PID == 0) {
		return record{}, errors.New("invalid VM process identity")
	}
	if state.Project != nil {
		p := state.Project
		if p.Workspace != Workspace || p.Branch == "" || strings.ContainsAny(p.Branch, "\x00\r\n") || !validCommit.MatchString(p.Commit) {
			return record{}, errors.New("invalid VM project metadata")
		}
	}
	return *state, nil
}
