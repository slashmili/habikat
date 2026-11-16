package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/slashmili/habikat/internal/git"
)

const maxFetchBundle = 1 << 30

func copyFetchBundle(ctx context.Context, source, destination string) (result error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, in.Close()) }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxFetchBundle {
		return errors.New("bundle must be a nonempty regular file no larger than 1 GiB")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, out.Close()) }()
	written, err := io.Copy(out, fetchContextReader{ctx, io.LimitReader(in, maxFetchBundle+1)})
	if err != nil {
		return err
	}
	if written > maxFetchBundle {
		return errors.New("bundle exceeds 1 GiB limit")
	}
	return ctx.Err()
}

type fetchContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r fetchContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (d *FetchDestination) validateBundle(ctx context.Context, temp, bundle string) (bundleManifest, error) {
	manifest, err := fetchManifestFile(bundle)
	if err != nil {
		return bundleManifest{}, err
	}
	if manifest.Format != d.format {
		return bundleManifest{}, errors.New("bundle object format is incompatible with the destination repository")
	}
	stage := fetchGit(temp)
	if _, err := stage.Run(ctx, "init", "--bare", "--template=", "--object-format="+manifest.Format, "objects.git"); err != nil {
		return bundleManifest{}, err
	}
	stage.Dir = filepath.Join(temp, "objects.git")
	if err := validateBundleObjects(ctx, stage, bundle, manifest); err != nil {
		return bundleManifest{}, err
	}
	return manifest, nil
}

func fetchManifestFile(path string) (_ bundleManifest, result error) {
	file, err := os.Open(path)
	if err != nil {
		return bundleManifest{}, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	return readFetchManifest(file)
}

func validateBundleObjects(ctx context.Context, stage git.Local, bundle string, manifest bundleManifest) error {
	if _, err := stage.Run(ctx, "bundle", "verify", bundle); err != nil {
		return fmt.Errorf("verify self-contained bundle: %w", err)
	}
	heads, err := stage.Run(ctx, "bundle", "list-heads", bundle)
	if err != nil {
		return err
	}
	if !bytes.Equal(normalizeManifest(heads), manifestLines(manifest)) {
		return errors.New("bundle and branch manifest disagree")
	}
	if _, err := stage.Run(ctx, "bundle", "unbundle", bundle); err != nil {
		return fmt.Errorf("validate bundle pack: %w", err)
	}
	if err := installValidationRefs(ctx, stage, manifest); err != nil {
		return err
	}
	unreachable, err := stage.Run(ctx, "fsck", "--full", "--strict", "--no-reflogs", "--unreachable")
	if err != nil {
		return fmt.Errorf("verify bundle object integrity and completeness: %w", err)
	}
	if len(unreachable) != 0 {
		return errors.New("bundle contains objects outside its committed branch history")
	}
	return nil
}

func installValidationRefs(ctx context.Context, stage git.Local, manifest bundleManifest) error {
	var input strings.Builder
	input.WriteString("start\x00")
	for _, branch := range manifest.Branches {
		if _, err := stage.Run(ctx, "check-ref-format", branch.Ref); err != nil {
			return fmt.Errorf("invalid exported branch ref: %w", err)
		}
		if err := requireCommit(ctx, stage, branch.Commit); err != nil {
			return fmt.Errorf("bundle manifest does not name a commit in its pack: %w", err)
		}
		fmt.Fprintf(&input, "create %s\x00%s\x00", branch.Ref, branch.Commit)
	}
	input.WriteString("prepare\x00commit\x00")
	if _, err := stage.Input(ctx, []byte(input.String()), "update-ref", "--no-deref", "--stdin", "-z"); err != nil {
		return fmt.Errorf("validate branch names together: %w", err)
	}
	return nil
}

func manifestLines(manifest bundleManifest) []byte {
	var lines []string
	for _, branch := range manifest.Branches {
		lines = append(lines, branch.Commit+" "+branch.Ref)
	}
	slices.Sort(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func normalizeManifest(data []byte) []byte {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	slices.Sort(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}
