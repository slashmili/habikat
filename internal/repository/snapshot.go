package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/slashmili/habikato/internal/git"
)

type Snapshot struct {
	Branch string
	Commit string
	Bundle string
}

func (s *Snapshot) Close() error { return os.RemoveAll(filepath.Dir(s.Bundle)) }

func Capture(ctx context.Context, dir string) (*Snapshot, error) {
	return capture(ctx, dir, nil)
}

func capture(ctx context.Context, dir string, afterBundleCreated func()) (_ *Snapshot, result error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	g := sourceRepository{Local: git.Local{Dir: dir}}
	root, err := g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		absent, markerErr := repositoryAbsent(dir, err)
		if markerErr != nil {
			return nil, markerErr
		}
		if absent {
			return nil, nil
		}
		return nil, fmt.Errorf("discover repository (a non-bare working tree is required): %w", err)
	}
	g.Dir = strings.TrimSuffix(string(root), "\n")
	if err := g.prepare(ctx); err != nil {
		return nil, err
	}
	branch, commit, err := g.inspect(ctx)
	if err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp("", "hk-bundle-")
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Branch: branch, Commit: commit, Bundle: filepath.Join(temp, "project.bundle")}
	defer func() {
		if result != nil {
			result = errors.Join(result, s.Close())
		}
	}()
	if _, err := g.Run(ctx, "bundle", "create", s.Bundle, "refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("capture repository: %w", err)
	}
	if afterBundleCreated != nil {
		afterBundleCreated()
	}
	nextBranch, nextCommit, err := g.inspect(ctx)
	if err != nil {
		return nil, fmt.Errorf("repository changed during capture: %w", err)
	}
	if branch != nextBranch || commit != nextCommit {
		return nil, errors.New("repository branch changed during capture; retry from a clean checkout")
	}
	if err := s.verifySelfContained(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) verifySelfContained(ctx context.Context) error {
	temp := filepath.Dir(s.Bundle)
	verify := git.Local{Dir: temp}
	if _, err := verify.Run(ctx, "init", "--bare", "--template=", "verify.git"); err != nil {
		return err
	}
	verify.Dir = filepath.Join(temp, "verify.git")
	if _, err := verify.Run(ctx, "bundle", "verify", s.Bundle); err != nil {
		return fmt.Errorf("verify self-contained snapshot: %w", err)
	}
	heads, err := verify.Run(ctx, "bundle", "list-heads", s.Bundle)
	if err != nil {
		return err
	}
	if string(heads) != s.Commit+" refs/heads/"+s.Branch+"\n" {
		return errors.New("snapshot does not advertise exactly the selected branch and commit")
	}
	return nil
}

type sourceRepository struct {
	git.Local
}

func (g *sourceRepository) prepare(ctx context.Context) error {
	config, err := g.Run(ctx, "config", "--null", "--list")
	if err != nil {
		return err
	}
	for _, item := range strings.Split(string(config), "\x00") {
		key, _, _ := strings.Cut(item, "\n")
		lower := strings.ToLower(key)
		if lower == "extensions.partialclone" || (strings.HasPrefix(lower, "remote.") && (strings.HasSuffix(lower, ".promisor") || strings.HasSuffix(lower, ".partialclonefilter"))) {
			return errors.New("partial clones are not supported")
		}
		if strings.HasPrefix(lower, "filter.lfs.") {
			return errors.New("Git LFS is not supported")
		}
		if strings.HasPrefix(lower, "filter.") {
			g.DisableFilter(key)
		}
	}
	shallow, err := g.Run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(shallow)) != "false" {
		return errors.New("shallow repositories are not supported")
	}
	objects, err := g.Run(ctx, "rev-parse", "--git-path", "objects/pack")
	if err != nil {
		return err
	}
	pack := strings.TrimSuffix(string(objects), "\n")
	if !filepath.IsAbs(pack) {
		pack = filepath.Join(g.Dir, pack)
	}
	markers, err := filepath.Glob(filepath.Join(pack, "*.promisor"))
	if err != nil {
		return err
	}
	if len(markers) > 0 {
		return errors.New("partial clones are not supported (promisor objects)")
	}
	return nil
}

func (g sourceRepository) inspect(ctx context.Context) (string, string, error) {
	branch, err := g.Run(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("an attached local branch is required; detached HEAD is not supported: %w", err)
	}
	ref := strings.TrimSuffix(string(branch), "\n")
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", "", errors.New("an attached local branch is required")
	}
	if _, err := g.Run(ctx, "check-ref-format", ref); err != nil {
		return "", "", err
	}
	commit, err := g.Run(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("repository must have an existing commit: %w", err)
	}
	paths, err := g.validateTrackedFiles(ctx)
	if err != nil {
		return "", "", err
	}
	if err := g.rejectLFSAttributes(ctx, paths); err != nil {
		return "", "", err
	}
	if err := g.rejectLFSPatternsAndPointers(ctx); err != nil {
		return "", "", err
	}
	if err := g.requireCompleteWorktreeChecks(ctx); err != nil {
		return "", "", err
	}
	if err := g.requireCleanWorktree(ctx); err != nil {
		return "", "", err
	}
	return strings.TrimPrefix(ref, "refs/heads/"), strings.TrimSpace(string(commit)), nil
}

func (g sourceRepository) validateTrackedFiles(ctx context.Context) ([]byte, error) {
	files, err := g.Run(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	var paths []byte
	for _, entry := range strings.Split(string(files), "\x00") {
		if entry == "" {
			continue
		}
		metadata, path, _ := strings.Cut(entry, "\t")
		if strings.HasPrefix(metadata, "160000 ") || filepath.Base(path) == ".gitmodules" {
			return nil, errors.New("submodules are not supported")
		}
		if filepath.Base(path) == ".lfsconfig" {
			return nil, errors.New("Git LFS is not supported")
		}
		paths = append(paths, []byte(path)...)
		paths = append(paths, 0)
	}
	return paths, nil
}

func (g sourceRepository) rejectLFSAttributes(ctx context.Context, paths []byte) error {
	attrs, err := g.Input(ctx, paths, "check-attr", "--cached", "-z", "--stdin", "filter")
	if err != nil {
		return err
	}
	fields := bytes.Split(attrs, []byte{0})
	for i := 2; i < len(fields); i += 3 {
		if string(fields[i]) == "lfs" {
			return errors.New("Git LFS is not supported (tracked filter=lfs attributes)")
		}
	}
	return nil
}

func (g sourceRepository) rejectLFSPatternsAndPointers(ctx context.Context) error {
	for _, args := range [][]string{
		{"grep", "--cached", "-I", "-l", "-E", "-e", `(^|[[:space:]])filter[[:space:]]*=[[:space:]]*lfs([[:space:]]|$)`, "--", ".gitattributes", ":(glob)**/.gitattributes"},
		{"grep", "--cached", "-I", "-l", "-G", "-e", "^version https://git-lfs[.]github[.]com/spec/v1$", "--"},
	} {
		_, err := g.Run(ctx, args...)
		if err == nil {
			return errors.New("Git LFS is not supported (tracked attributes or pointer)")
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return err
		}
	}
	return nil
}

func (g sourceRepository) requireCompleteWorktreeChecks(ctx context.Context) error {
	flaggedFiles, err := g.Run(ctx, git.IndexFlags("-z")...)
	if err != nil {
		return err
	}
	for entry := range bytes.SplitSeq(flaggedFiles, []byte{0}) {
		if len(entry) > 0 && (entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z') {
			return errors.New("repository must be clean: unset assume-unchanged and skip-worktree flags before creating a VM")
		}
	}
	return nil
}

func (g sourceRepository) requireCleanWorktree(ctx context.Context) error {
	status, err := g.Run(ctx, git.Status("-z", "--ignore-submodules=none")...)
	if err != nil {
		return err
	}
	if len(status) > 0 {
		return errors.New("repository must be clean: commit or remove staged, tracked, and non-ignored untracked changes")
	}
	return nil
}

func repositoryAbsent(dir string, discoveryErr error) (bool, error) {
	if !outsideRepository(discoveryErr) {
		return false, nil
	}
	hasMarker, err := hasGitMarker(dir)
	return !hasMarker, err
}

func outsideRepository(err error) bool {
	return strings.Contains(err.Error(), "fatal: not a git repository (or any of the parent directories): .git") || strings.Contains(err.Error(), "fatal: not a git repository (or any parent up to mount point ")
}

func hasGitMarker(dir string) (bool, error) {
	current, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	for {
		_, err := os.Lstat(filepath.Join(current, ".git"))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}
