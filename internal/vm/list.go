package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/slashmili/habikat/internal/repository"
	"golang.org/x/sys/unix"
)

type Listing struct {
	Name      string
	Status    string
	Directory string
}

func List(ctx context.Context, all bool) ([]Listing, error) {
	root, err := stateHome()
	if err != nil {
		return nil, err
	}
	dir, err := openPrivateDirectory(unix.AT_FDCWD, root)
	if errors.Is(err, os.ErrNotExist) {
		return []Listing{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state directory: %w", err)
	}
	defer dir.Close()
	source := ""
	if !all {
		source, err = repository.SourceDirectory(ctx, ".")
		if err != nil {
			return nil, err
		}
	}
	return listDirectory(ctx, dir, source)
}

func listDirectory(ctx context.Context, root *os.File, source string) ([]Listing, error) {
	entries, err := root.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	rows := []Listing{}
	var problems error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			problems = errors.Join(problems, err)
			break
		}
		if entry.Name() == ".lock" {
			continue
		}
		row, err := listEntry(root, entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = errors.Join(problems, fmt.Errorf("VM %q: %w", entry.Name(), err))
			continue
		}
		if source == "" || matchesSource(row, source) {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b Listing) int { return strings.Compare(a.Name, b.Name) })
	return rows, problems
}

func matchesSource(row Listing, source string) bool {
	active := row.Status == "running" || row.Status == "starting" || row.Status == "stopping"
	return active && row.Directory != "" && row.Directory == source
}

func listEntry(root *os.File, name string) (Listing, error) {
	if err := ValidateName(name); err != nil {
		return Listing{}, err
	}
	dir, err := openPrivateDirectory(int(root.Fd()), name)
	if err != nil {
		return Listing{}, err
	}
	defer dir.Close()
	state, err := readListedRecord(dir)
	if err != nil {
		return Listing{}, err
	}
	status := observeStatus(state, filepath.Join(root.Name(), name))
	return Listing{Name: name, Status: status, Directory: state.SourceDirectory}, nil
}

func openPrivateDirectory(parent int, path string) (*os.File, error) {
	fd, err := unix.Openat(
		parent,
		path,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	info, err := dir.Stat()
	if err == nil && (info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
		err = errors.New("directory must be owned by you with mode 0700")
	}
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	return dir, nil
}

func readListedRecord(dir *os.File) (record, error) {
	fd, err := unix.Openat(
		int(dir.Fd()),
		"state.json",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if errors.Is(err, os.ErrNotExist) {
		info, statErr := dir.Stat()
		if statErr != nil {
			return record{}, statErr
		}
		if info.Sys().(*syscall.Stat_t).Nlink == 0 {
			return record{}, err
		}
		return record{}, errors.New("missing state.json")
	}
	if err != nil {
		return record{}, err
	}
	file := os.NewFile(uintptr(fd), "state.json")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return record{}, err
	}
	if !info.Mode().IsRegular() {
		return record{}, errors.New("state must be a regular file")
	}
	const maxRecordSize = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxRecordSize+1))
	if err != nil {
		return record{}, err
	}
	if len(data) > maxRecordSize {
		return record{}, errors.New("state exceeds size limit")
	}
	return decodeRecord(data)
}

func hostBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !validBootID.MatchString(id) {
		return "", errors.New("invalid host boot identity")
	}
	return id, nil
}

func observeStatus(state record, dir string) string {
	if state.PID == 0 {
		return "stopped"
	}
	start, err := processStart(state.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errProcessExited) {
		return "stopped"
	}
	if err != nil {
		return "unknown"
	}
	if start != state.StartTime {
		return "stopped"
	}
	owned, err := processOwnsConfig(state.PID, dir)
	if errors.Is(err, os.ErrNotExist) {
		return "stopped"
	}
	if err != nil {
		return "unknown"
	}
	if !owned {
		return "stopped"
	}
	boot, err := hostBootID()
	if err != nil || state.BootID == "" {
		return "unknown"
	}
	if state.BootID != boot {
		return "stopped"
	}
	if !state.Ready {
		return "starting"
	}
	return "running"
}
