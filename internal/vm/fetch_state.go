package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type fetchSession struct {
	name, dir string
	state     record
	pinned    *os.File
	original  os.FileInfo
}

func openFetchSession(ctx context.Context, name string) (_ *fetchSession, result error) {
	dir, lock, err := lockState(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	state, err := readRecord(dir)
	if err != nil {
		return nil, fmt.Errorf("open VM %s for fetch: %w", name, err)
	}
	if err := requireFetchSource(name, dir, state); err != nil {
		return nil, err
	}
	state, err = ensureInstanceID(dir, state)
	if err != nil {
		return nil, err
	}
	pinned, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	original, err := pinned.Stat()
	if err != nil {
		return nil, errors.Join(err, pinned.Close())
	}
	return &fetchSession{name: name, dir: dir, state: state, pinned: pinned, original: original}, nil
}

func requireFetchSource(name, dir string, state record) error {
	if !state.Ready || !sameProcess(state, dir) {
		return fmt.Errorf("fetch needs the existing VM %s running and reachable; its disk and keys have been preserved; previously fetched refs remain usable", name)
	}
	if state.Project == nil {
		return fmt.Errorf("VM %s has no imported project to fetch", name)
	}
	return nil
}

func (session *fetchSession) Close() error { return session.pinned.Close() }

func (session *fetchSession) recheck() error {
	current, err := os.Lstat(session.dir)
	if err != nil || !os.SameFile(session.original, current) {
		return errors.New("VM was deleted or replaced during fetch; no refs were published")
	}
	next, err := readRecord(session.dir)
	if err != nil {
		return err
	}
	if !sameFetchIdentity(session.state, next) {
		return errors.New("VM instance or project changed during fetch; no refs were published")
	}
	return requireFetchSource(session.name, session.dir, next)
}

func sameFetchIdentity(first, next record) bool {
	return first.InstanceID == next.InstanceID && first.PID == next.PID && first.StartTime == next.StartTime && first.Project != nil && next.Project != nil && *first.Project == *next.Project
}
