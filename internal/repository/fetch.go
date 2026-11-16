package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/slashmili/habikato/internal/git"
)

type FetchedBranch struct {
	Name      string
	Commit    string
	Ref       string
	Previous  string
	Rewritten bool
}

type FetchDestination struct {
	git    git.Local
	format string
}

type BranchImport struct {
	destination *FetchDestination
	branches    []FetchedBranch
	directory   string
	instanceID  string
	closed      bool
	published   bool
}

func OpenFetchDestination(ctx context.Context, dir, base string) (*FetchDestination, error) {
	g := fetchGit(dir)
	root, err := g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a suitable fetch destination: a non-bare Git repository is required: %w", err)
	}
	g.Dir = strings.TrimSuffix(string(root), "\n")
	format, err := g.Run(ctx, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("read destination object format: %w", err)
	}
	d := &FetchDestination{git: g, format: strings.TrimSpace(string(format))}
	if !validObjectID(base, d.format) {
		return nil, errors.New("not a suitable fetch destination: incompatible base commit or object format")
	}
	if err := requireCommit(ctx, g, base); err != nil {
		return nil, fmt.Errorf("not a suitable fetch destination: the initial project commit must exist locally: %w", err)
	}
	return d, nil
}

func fetchGit(dir string) git.Local {
	return (git.Local{Dir: dir}).WithConfig(
		"gc.auto=0", "maintenance.auto=false", "fetch.writeCommitGraph=false",
		"transfer.fsckObjects=true", "fetch.fsckObjects=true",
		"user.name=Habikato", "user.email=habikato@localhost.invalid",
	)
}

func requireCommit(ctx context.Context, g git.Local, oid string) error {
	kind, err := g.Run(ctx, "cat-file", "-t", oid)
	if err != nil {
		return err
	}
	if string(kind) != "commit\n" {
		return errors.New("branch tip is not a commit object")
	}
	return nil
}

func (d *FetchDestination) Prepare(ctx context.Context, bundle, instanceID string) (_ *BranchImport, result error) {
	if !validHex(instanceID, 32) {
		return nil, errors.New("invalid VM instance identifier")
	}
	temp, err := os.MkdirTemp("", "hk-fetch-verify-")
	if err != nil {
		return nil, err
	}
	prepared := &BranchImport{destination: d, directory: temp, instanceID: instanceID}
	defer func() {
		if result != nil {
			result = errors.Join(result, prepared.Close())
		}
	}()
	immutable := filepath.Join(temp, "snapshot.bundle")
	if err := copyFetchBundle(ctx, bundle, immutable); err != nil {
		return nil, err
	}
	manifest, err := d.validateBundle(ctx, temp, immutable)
	if err != nil {
		return nil, err
	}
	if _, err := d.git.Run(ctx, "bundle", "unbundle", immutable); err != nil {
		return nil, fmt.Errorf("import validated bundle objects: %w", err)
	}
	prepared.branches, err = d.prepareBranches(ctx, manifest, instanceID)
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

func (d *FetchDestination) prepareBranches(ctx context.Context, manifest bundleManifest, instanceID string) ([]FetchedBranch, error) {
	branches := make([]FetchedBranch, 0, len(manifest.Branches))
	for _, source := range manifest.Branches {
		ref, err := fetchDestinationRef(instanceID, source.Ref)
		if err != nil {
			return nil, err
		}
		if _, err := d.git.Run(ctx, "check-ref-format", ref); err != nil {
			return nil, fmt.Errorf("invalid destination ref: %w", err)
		}
		previous, err := readDirectFetchRef(ctx, d.git, ref, d.format)
		if err != nil {
			return nil, err
		}
		rewritten, err := d.isRewritten(ctx, previous, source.Commit)
		if err != nil {
			return nil, err
		}
		branches = append(branches, FetchedBranch{
			Name: strings.TrimPrefix(source.Ref, "refs/heads/"), Commit: source.Commit,
			Ref: ref, Previous: previous, Rewritten: rewritten,
		})
	}
	slices.SortFunc(branches, func(a, b FetchedBranch) int { return strings.Compare(a.Ref, b.Ref) })
	return branches, nil
}

func (d *FetchDestination) isRewritten(ctx context.Context, previous, next string) (bool, error) {
	if previous == "" || previous == next {
		return false, nil
	}
	if err := requireCommit(ctx, d.git, previous); err != nil {
		return false, fmt.Errorf("existing destination ref is not a commit: %w", err)
	}
	_, err := d.git.Run(ctx, "merge-base", "--is-ancestor", previous, next)
	if gitExitCode(err) == 1 {
		return true, nil
	}
	return false, err
}

func (i *BranchImport) Branches() []FetchedBranch { return slices.Clone(i.branches) }

func (i *BranchImport) Publish(ctx context.Context) error {
	return i.publish(ctx, nil)
}

func (i *BranchImport) Close() error {
	i.closed = true
	return os.RemoveAll(i.directory)
}
