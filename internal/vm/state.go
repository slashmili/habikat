package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var validCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

var validInstanceID = regexp.MustCompile(`^[0-9a-f]{32}$`)

var validBootID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

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

type artifactRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type launchArtifacts struct {
	Firecracker artifactRef `json:"firecracker"`
	Kernel      artifactRef `json:"kernel"`
}

type record struct {
	Creating        bool             `json:"creating,omitempty"`
	Launch          *launchArtifacts `json:"launch"`
	Stopping        bool             `json:"stopping,omitempty"`
	SourceDirectory string           `json:"source_directory,omitempty"`
	BootID          string           `json:"boot_id"`
	InstanceID      string           `json:"instance_id"`
	Project         *Project         `json:"project,omitempty"`
	PID             int              `json:"pid"`
	StartTime       string           `json:"start_time"`
	Ready           bool             `json:"ready"`
}

const currentRecordVersion = 1

type storedRecord struct {
	record
	Version              int    `json:"version"`
	SourceDirectoryBytes []byte `json:"source_directory_bytes,omitempty"`
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
	return filepath.Abs(filepath.Join(base, "habikat"))
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
	data, err := encodeRecord(state)
	if err != nil {
		return err
	}
	return replaceFile(dir, "state.json", data)
}

func encodeRecord(state record) ([]byte, error) {
	stored := storedRecord{record: state, Version: currentRecordVersion}
	if !utf8.ValidString(state.SourceDirectory) {
		stored.SourceDirectoryBytes = []byte(state.SourceDirectory)
		stored.SourceDirectory = ""
	}
	return json.Marshal(stored)
}

func replaceFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+strings.TrimSuffix(name, ".json")+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func readRecord(dir string) (record, error) {
	if err := privateDir(dir); err != nil {
		return record{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return record{}, err
	}
	return decodeRecord(data)
}

var requiredRecordFields = []string{"version", "launch", "boot_id", "instance_id", "pid", "start_time", "ready"}

func decodeRecord(data []byte) (record, error) {
	if !utf8.Valid(data) {
		return record{}, errors.New("read VM state: invalid UTF-8")
	}
	var stored *storedRecord
	if err := jsonv2.Unmarshal(data, &stored, jsonv2.RejectUnknownMembers(true)); err != nil {
		if errors.Is(err, jsonv2.ErrUnknownName) {
			return record{}, fmt.Errorf("unsupported or corrupt VM state: %w", err)
		}
		return record{}, fmt.Errorf("read VM state: %w", err)
	}
	if stored == nil {
		return record{}, errors.New("read VM state: expected an object")
	}
	if err := requireCurrentRecordFields(data); err != nil {
		return record{}, err
	}
	if stored.Version != currentRecordVersion {
		return record{}, fmt.Errorf("unsupported or corrupt VM state: version %d is unsupported (expected %d)", stored.Version, currentRecordVersion)
	}
	state := &stored.record
	if stored.SourceDirectoryBytes != nil {
		if state.SourceDirectory != "" {
			return record{}, errors.New("ambiguous VM source directory")
		}
		state.SourceDirectory = string(stored.SourceDirectoryBytes)
	}
	if err := validateRecord(*state); err != nil {
		return record{}, fmt.Errorf("unsupported or corrupt VM state: %w", err)
	}
	return *state, nil
}

func requireCurrentRecordFields(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("read VM state: %w", err)
	}
	for _, name := range requiredRecordFields {
		field, ok := fields[name]
		if !ok {
			return fmt.Errorf("unsupported or corrupt VM state: missing %s", name)
		}
		if bytes.Equal(field, []byte("null")) {
			return fmt.Errorf("unsupported or corrupt VM state: %s cannot be null", name)
		}
	}
	_, text := fields["source_directory"]
	_, raw := fields["source_directory_bytes"]
	if !text && !raw {
		return errors.New("unsupported or corrupt VM state: missing source_directory")
	}
	return nil
}

func validateRecord(state record) error {
	if state.Creating && state.Ready {
		return errors.New("invalid VM creation state")
	}
	if state.Stopping && state.PID == 0 {
		return errors.New("invalid stopping VM process identity")
	}
	if !validProcessIdentity(state) {
		return errors.New("invalid VM process identity")
	}
	if !validInstanceID.MatchString(state.InstanceID) {
		return errors.New("invalid VM instance identity")
	}
	if !validBootID.MatchString(state.BootID) {
		return errors.New("invalid VM host boot identity")
	}
	if !validAbsolutePath(state.SourceDirectory) {
		return errors.New("invalid VM source directory")
	}
	if state.Launch == nil {
		return errors.New("missing launch artifacts")
	}
	if !validLaunchArtifacts(*state.Launch) {
		return errors.New("invalid VM launch artifacts")
	}
	if state.Project != nil {
		p := state.Project
		if p.Workspace != Workspace || p.Branch == "" || strings.ContainsAny(p.Branch, "\x00\r\n") || !validCommit.MatchString(p.Commit) {
			return errors.New("invalid VM project metadata")
		}
	}
	return nil
}

func validProcessIdentity(state record) bool {
	if state.PID == 0 {
		return state.StartTime == "" && !state.Ready
	}
	ticks, err := strconv.ParseUint(state.StartTime, 10, 64)
	return state.PID > 0 && err == nil && strconv.FormatUint(ticks, 10) == state.StartTime
}

var validDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validLaunchArtifacts(launch launchArtifacts) bool {
	return validArtifact(launch.Firecracker) && validArtifact(launch.Kernel) && filepath.Base(launch.Firecracker.Path) == "firecracker"
}

func validArtifact(artifact artifactRef) bool {
	return validDigest.MatchString(artifact.SHA256) && validAbsolutePath(artifact.Path)
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func newInstanceID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}
