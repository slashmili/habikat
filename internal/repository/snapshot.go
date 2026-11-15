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
	"syscall"
	"time"
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
	g := git{dir: dir}
	root, err := g.run(ctx, "rev-parse", "--show-toplevel")
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
	g.dir = strings.TrimSuffix(string(root), "\n")
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
	if _, err := g.run(ctx, "bundle", "create", s.Bundle, "refs/heads/"+branch); err != nil {
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
	verify := git{dir: temp}
	if _, err := verify.run(ctx, "init", "--bare", "--template=", "verify.git"); err != nil {
		return err
	}
	verify.dir = filepath.Join(temp, "verify.git")
	if _, err := verify.run(ctx, "bundle", "verify", s.Bundle); err != nil {
		return fmt.Errorf("verify self-contained snapshot: %w", err)
	}
	heads, err := verify.run(ctx, "bundle", "list-heads", s.Bundle)
	if err != nil {
		return err
	}
	if string(heads) != s.Commit+" refs/heads/"+s.Branch+"\n" {
		return errors.New("snapshot does not advertise exactly the selected branch and commit")
	}
	return nil
}

type git struct {
	dir    string
	config []string
}

func gitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "LC_ALL" || key == "LANGUAGE" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0")
}

func (g git) run(ctx context.Context, args ...string) ([]byte, error) {
	return g.input(ctx, nil, args...)
}
func (g git) input(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "credential.helper=", "-c", "core.askPass=/bin/false", "-c", "protocol.allow=never", "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "core.attributesFile=/dev/null"}
	base = append(base, g.config...)
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env, cmd.Dir = gitEnv(), g.dir
	cmd.Stdin = bytes.NewReader(input)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], errors.Join(err, ctx.Err()), strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g *git) prepare(ctx context.Context) error {
	config, err := g.run(ctx, "config", "--null", "--list")
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
			g.disableFilterForCommand(key)
		}
	}
	shallow, err := g.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(shallow)) != "false" {
		return errors.New("shallow repositories are not supported")
	}
	objects, err := g.run(ctx, "rev-parse", "--git-path", "objects/pack")
	if err != nil {
		return err
	}
	pack := strings.TrimSuffix(string(objects), "\n")
	if !filepath.IsAbs(pack) {
		pack = filepath.Join(g.dir, pack)
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

func (g *git) disableFilterForCommand(key string) {
	for _, suffix := range []string{".clean", ".smudge", ".process", ".required"} {
		if strings.HasSuffix(strings.ToLower(key), suffix) {
			override := ""
			if suffix == ".required" {
				override = "false"
			}
			g.config = append(g.config, "-c", key+"="+override)
		}
	}
}

func (g git) inspect(ctx context.Context) (string, string, error) {
	branch, err := g.run(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("an attached local branch is required; detached HEAD is not supported: %w", err)
	}
	ref := strings.TrimSuffix(string(branch), "\n")
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", "", errors.New("an attached local branch is required")
	}
	if _, err := g.run(ctx, "check-ref-format", ref); err != nil {
		return "", "", err
	}
	commit, err := g.run(ctx, "rev-parse", "--verify", "HEAD^{commit}")
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

func (g git) validateTrackedFiles(ctx context.Context) ([]byte, error) {
	files, err := g.run(ctx, "ls-files", "--stage", "-z")
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

func (g git) rejectLFSAttributes(ctx context.Context, paths []byte) error {
	attrs, err := g.input(ctx, paths, "check-attr", "--cached", "-z", "--stdin", "filter")
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

func (g git) rejectLFSPatternsAndPointers(ctx context.Context) error {
	for _, args := range [][]string{
		{"grep", "--cached", "-I", "-l", "-E", "-e", `(^|[[:space:]])filter[[:space:]]*=[[:space:]]*lfs([[:space:]]|$)`, "--", ".gitattributes", ":(glob)**/.gitattributes"},
		{"grep", "--cached", "-I", "-l", "-G", "-e", "^version https://git-lfs[.]github[.]com/spec/v1$", "--"},
	} {
		_, err := g.run(ctx, args...)
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

func (g git) requireCompleteWorktreeChecks(ctx context.Context) error {
	flaggedFiles, err := g.run(ctx, "ls-files", "-v", "-z")
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

func (g git) requireCleanWorktree(ctx context.Context) error {
	status, err := g.run(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
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
