package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/slashmili/habikat/internal/repository"
)

type FetchResult struct {
	InstanceID string
	Branches   []repository.FetchedBranch
	Changes    GuestChanges
}

var errFetchGuestBecameDirty = errors.New("guest became dirty before publication")

var fetchExportWriter = func(_ context.Context, file *os.File) io.Writer { return file }
var afterFetchDownload = func(context.Context, string) error { return nil }

func Fetch(ctx context.Context, name string, confirm func(GuestChanges) error) (_ *FetchResult, result error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	session, err := openFetchSession(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, session.Close()) }()
	destination, err := repository.OpenFetchDestination(ctx, ".", session.state.Project.Commit)
	if err != nil {
		return nil, err
	}
	changes, err := inspectFetchGuest(ctx, session)
	if err != nil {
		return nil, err
	}
	approved := confirm == nil
	if err := approveFetchChanges(changes, &approved, confirm); err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("", "hk-fetch-")
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(temporary)) }()
	bundle := filepath.Join(temporary, "project.bundle")
	if err := receiveFetchBundle(ctx, session, bundle); err != nil {
		return nil, err
	}
	if err := session.recheck(); err != nil {
		return nil, err
	}
	prepared, err := destination.Prepare(ctx, bundle, session.state.InstanceID)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, prepared.Close()) }()
	latest, err := publishFetch(ctx, session, prepared, approved)
	if errors.Is(err, errFetchGuestBecameDirty) {
		if err := approveFetchChanges(latest, &approved, confirm); err != nil {
			return nil, err
		}
		latest, err = publishFetch(ctx, session, prepared, approved)
	}
	if err != nil {
		return nil, err
	}
	return &FetchResult{InstanceID: session.state.InstanceID, Branches: prepared.Branches(), Changes: mergeGuestChanges(changes, latest)}, nil
}

func approveFetchChanges(changes GuestChanges, approved *bool, confirm func(GuestChanges) error) error {
	if !changes.Dirty() || *approved {
		return nil
	}
	if err := confirm(changes); err != nil {
		return err
	}
	*approved = true
	return nil
}

func receiveFetchBundle(ctx context.Context, session *fetchSession, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = downloadExport(ctx, session.dir, exportCommand(session.state.Project.Commit), fetchExportWriter(ctx, file))
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return afterFetchDownload(ctx, path)
}

func inspectFetchGuest(ctx context.Context, session *fetchSession) (GuestChanges, error) {
	if err := session.recheck(); err != nil {
		return GuestChanges{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := sshOutput(ctx, session.dir, fetchStatusCommand(session.state.Project.Commit), nil)
	if err != nil {
		return GuestChanges{}, fmt.Errorf("cannot inspect guest %s; fetch needs its existing VM running and reachable, and its disk has been preserved: %w", session.name, err)
	}
	if !bytes.HasPrefix(out, []byte("HKSTATUS1\n")) {
		return GuestChanges{}, errors.New("invalid guest Git status response")
	}
	return parseFetchStatus(bytes.TrimPrefix(out, []byte("HKSTATUS1\n")))
}

func publishFetch(ctx context.Context, session *fetchSession, prepared *repository.BranchImport, approved bool) (changes GuestChanges, result error) {
	_, lock, err := lockState(ctx, session.name)
	if err != nil {
		return GuestChanges{}, err
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	changes, err = inspectFetchGuest(ctx, session)
	if err != nil {
		return GuestChanges{}, err
	}
	if changes.Dirty() && !approved {
		return changes, errFetchGuestBecameDirty
	}
	if err := ctx.Err(); err != nil {
		return changes, err
	}
	return changes, prepared.Publish(ctx)
}

func mergeGuestChanges(first, next GuestChanges) GuestChanges {
	return GuestChanges{Staged: first.Staged || next.Staged, Unstaged: first.Unstaged || next.Unstaged, Untracked: first.Untracked || next.Untracked}
}
