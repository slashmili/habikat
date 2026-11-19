//go:build e2e

package e2e

import (
	"bytes"
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
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const fetchOtherInstance = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type fetchFixture struct {
	*harness
	host, branch, base string
}

func newFetchFixture(t *testing.T) *fetchFixture {
	t.Helper()
	h := newHarness(t)
	host, branch, base := projectFixture(t, h)
	fixtureGit(t, host, "config", "user.name", "Fetch Fixture")
	fixtureGit(t, host, "config", "user.email", "fetch@example.invalid")
	h.env = append(h.env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	return &fetchFixture{h, host, branch, base}
}

func (f *fetchFixture) create(name string) {
	f.t.Helper()
	f.must("", "create", "--name", name, "--with-git-config")
}

func instanceID(t *testing.T, h *harness, name string) string {
	t.Helper()
	var state struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(read(t, h.state(name, "state.json")), &state); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(state.InstanceID) {
		t.Fatalf("invalid stable instance ID: %q", state.InstanceID)
	}
	return state.InstanceID
}

func fetchedRef(t *testing.T, h *harness, name, branch string) string {
	t.Helper()
	return "refs/remotes/hk/" + instanceID(t, h, name) + "/" + branch
}

func guestCommit(t *testing.T, h *harness, name, ref string) string {
	t.Helper()
	commit := strings.TrimSpace(h.must("git rev-parse --verify "+quote(ref)+"\n", "attach", name))
	if !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(commit) {
		t.Fatalf("invalid guest object ID: %q", commit)
	}
	return commit
}

func hostRefs(t *testing.T, f *fetchFixture) string {
	t.Helper()
	return fixtureGit(t, f.host, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
}

func assertRef(t *testing.T, host, ref, commit string) {
	t.Helper()
	if got := fixtureGit(t, host, "rev-parse", "--verify", ref); got != commit {
		t.Fatalf("%s=%s, want %s", ref, got, commit)
	}
}

func assertCleanFetch(t *testing.T, f *fetchFixture, name, branch, commit string) string {
	t.Helper()
	out := f.must("", "fetch", name)
	ref := fetchedRef(t, f.harness, name, branch)
	for _, value := range []string{name, branch, commit[:12], ref} {
		if !strings.Contains(out, value) {
			t.Fatalf("fetch omitted %q: %s", value, out)
		}
	}
	if strings.Contains(out, "[y/N]") {
		t.Fatalf("clean guest prompted: %s", out)
	}
	assertRef(t, f.host, ref, commit)
	return out
}

func TestFetchCommittedWork(t *testing.T) {
	f := newFetchFixture(t)
	f.create("fetch-source")
	f.create("fetch-sibling")
	f.must(`set -eu
printf 'guest edit\n' > 'file with spaces'
printf '#!/bin/sh\nprintf "guest script\\n"\n' > guest-script
chmod 755 guest-script
head -c 196608 /dev/urandom > payload.bin
printf x >> payload.bin
git add 'file with spaces' guest-script payload.bin
git commit -m 'normal guest commit'
git tag guest-private-tag
`, "attach", "fetch-source")
	commit := guestCommit(t, f.harness, "fetch-source", "HEAD")
	payload := guestCommit(t, f.harness, "fetch-source", "HEAD:payload.bin")
	protectDirtyHost(t, f)
	protected := snapshotHost(t, f)
	out := assertCleanFetch(t, f, "fetch-source", f.branch, commit)
	ref := fetchedRef(t, f.harness, "fetch-source", f.branch)
	if !strings.Contains(strings.ToLower(out), "ignored") || !strings.Contains(strings.ToLower(out), "outside") {
		t.Fatalf("fetch did not explain excluded scope: %s", out)
	}
	if got := fixtureGit(t, f.host, "log", "-1", "--format=%an <%ae>|%cn <%ce>", ref); got != "Fetch Fixture <fetch@example.invalid>|Fetch Fixture <fetch@example.invalid>" {
		t.Fatalf("guest commit identity=%q", got)
	}
	if got := fixtureGit(t, f.host, "show", ref+":file with spaces"); got != "guest edit" {
		t.Fatalf("imported content=%q", got)
	}
	if got := fixtureGit(t, f.host, "ls-tree", ref, "guest-script"); !strings.HasPrefix(got, "100755 blob ") {
		t.Fatalf("executable mode missing: %s", got)
	}
	assertRef(t, f.host, ref+":payload.bin", payload)
	if got := fixtureGit(t, f.host, "cat-file", "-s", payload); got != "196609" {
		t.Fatalf("large payload size=%s", got)
	}
	if got := fixtureGit(t, f.host, "rev-list", "--count", ref); got != "3" {
		t.Fatalf("reachable history=%s", got)
	}
	if got := fixtureGit(t, f.host, "tag", "--list", "guest-private-tag"); got != "" {
		t.Fatalf("fetch imported guest tag: %s", got)
	}
	assertHostUnchanged(t, f, protected)
	before := fixtureGit(t, f.host, "reflog", "show", "--format=%H", ref)
	assertCleanFetch(t, f, "fetch-source", f.branch, commit)
	if after := fixtureGit(t, f.host, "reflog", "show", "--format=%H", ref); after != before {
		t.Fatal("no-op fetch changed reflog")
	}
	testFetchGuestConfirmation(t, f)
	testFetchUpdatesAndNamespaces(t, f)
	assertHostUnchanged(t, f, protected)
	testFetchRecreatedName(t, f)
	assertHostUnchanged(t, f, protected)
	testFetchStoppedVM(t, f, "fetch-source")
}

type hostFileEvidence struct {
	Mode    os.FileMode
	Content string
}
type hostEvidence struct {
	Files map[string]hostFileEvidence
	Refs  string
}

func protectDirtyHost(t *testing.T, f *fetchFixture) {
	t.Helper()
	write(t, filepath.Join(f.host, "file with spaces"), []byte("staged host content\n"))
	fixtureGit(t, f.host, "add", "file with spaces")
	write(t, filepath.Join(f.host, "file with spaces"), []byte("unstaged host content\n"))
	write(t, filepath.Join(f.host, "untracked-host"), []byte("untracked host content\n"))
	write(t, filepath.Join(f.host, ".git/FETCH_HEAD"), []byte("host fetch marker\n"))
	fixtureGit(t, f.host, "update-ref", "refs/remotes/hk/"+fetchOtherInstance+"/main", f.base)
}

func snapshotHost(t *testing.T, f *fetchFixture) hostEvidence {
	t.Helper()
	evidence := hostEvidence{Files: map[string]hostFileEvidence{}}
	err := filepath.WalkDir(f.host, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(f.host, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		evidence.Files[rel] = hostFileEvidence{info.Mode(), string(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".git/HEAD", ".git/index", ".git/config", ".git/FETCH_HEAD"} {
		info, err := os.Stat(filepath.Join(f.host, path))
		if err != nil {
			t.Fatal(err)
		}
		evidence.Files[path] = hostFileEvidence{info.Mode(), string(read(t, filepath.Join(f.host, path)))}
	}
	evidence.Refs = fixtureGit(t, f.host, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/heads", "refs/tags", "refs/remotes/origin", "refs/remotes/hk/"+fetchOtherInstance)
	return evidence
}

func assertHostUnchanged(t *testing.T, f *fetchFixture, want hostEvidence) {
	t.Helper()
	if got := snapshotHost(t, f); !reflect.DeepEqual(got, want) {
		t.Fatal("fetch changed protected host files, index, config, HEAD, local branches, tags, FETCH_HEAD, or another instance's refs")
	}
}

func dirtyGuestEvidence(t *testing.T, f *fetchFixture) string {
	t.Helper()
	return f.must(`set -eu
export GIT_OPTIONAL_LOCKS=0
git rev-parse HEAD
git for-each-ref --format='%(refname) %(objectname)'
git status --porcelain=v1 --untracked-files=all
git hash-object .git/index 'file with spaces' staged-only untracked-only ignored-guest /home/hk/outside-fetch
`, "attach", "fetch-source")
}

func assertDirtyGuestUnchanged(t *testing.T, f *fetchFixture, want string) {
	t.Helper()
	if got := dirtyGuestEvidence(t, f); got != want {
		t.Fatalf("fetch changed guest files/index/branches:\n%s\nwant:\n%s", got, want)
	}
}

func testFetchGuestConfirmation(t *testing.T, f *fetchFixture) {
	t.Helper()
	f.must(`set -eu
git checkout -b feature/second
printf 'second committed branch\n' > second.txt
git add second.txt
git commit -m 'second branch'
printf 'private staged content\n' > staged-only
git add staged-only
printf 'private unstaged content\n' >> 'file with spaces'
printf 'private untracked content\n' > untracked-only
printf 'private ignored content\n' > ignored-guest
printf 'outside repository content\n' > /home/hk/outside-fetch
`, "attach", "fetch-source")
	beforeRefs, beforeGuest := hostRefs(t, f), dirtyGuestEvidence(t, f)
	for _, input := range []string{"", "yes\n"} {
		out, err := f.run(input, "fetch", "fetch-source")
		if err == nil || !strings.Contains(out, "--force") {
			t.Fatalf("dirty nonterminal fetch accepted: %v %s", err, out)
		}
		if hostRefs(t, f) != beforeRefs {
			t.Fatal("nonterminal refusal changed refs")
		}
		assertDirtyGuestUnchanged(t, f, beforeGuest)
	}
	for _, answer := range []string{"n\n", "\n", "\x04", "\x03"} {
		out, _ := operationPTY(t, f.harness, answer, "fetch", "fetch-source")
		if !strings.Contains(strings.ToLower(out), "cancel") || strings.Contains(out, "refs/remotes/hk/") {
			t.Fatalf("cancelled fetch reported success: %s", out)
		}
		if hostRefs(t, f) != beforeRefs {
			t.Fatal("cancelled fetch changed refs")
		}
		assertDirtyGuestUnchanged(t, f, beforeGuest)
	}
	out, err := operationPTY(t, f.harness, "YeS\n", "fetch", "fetch-source")
	if err != nil {
		t.Fatalf("confirmed fetch: %v %s", err, out)
	}
	assertDirtyFetchOutput(t, out)
	assertRef(t, f.host, fetchedRef(t, f.harness, "fetch-source", "feature/second"), guestCommit(t, f.harness, "fetch-source", "HEAD"))
	assertDirtyGuestUnchanged(t, f, beforeGuest)
	if out, err := f.run("", "fetch", "fetch-source"); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("dirty no-op fetch bypassed approval: %v %s", err, out)
	}
	for i, args := range [][]string{{"fetch", "--force", "fetch-source"}, {"fetch", "fetch-source", "--force"}} {
		branch := fmt.Sprintf("forced-%d", i)
		f.must("git branch "+quote(branch)+"\n", "attach", "fetch-source")
		beforeGuest = dirtyGuestEvidence(t, f)
		assertDirtyFetchOutput(t, f.must("", args...))
		assertRef(t, f.host, fetchedRef(t, f.harness, "fetch-source", branch), guestCommit(t, f.harness, "fetch-source", "HEAD"))
		assertDirtyGuestUnchanged(t, f, beforeGuest)
	}
	assertUnforcedPipeDeletionRefused(t, f.harness, "fetch-source", "yes\n")
	f.must("set -eu\ngit reset --hard HEAD\nrm untracked-only\ntest -z \"$(git status --porcelain)\"\n", "attach", "fetch-source")
	assertCleanFetch(t, f, "fetch-source", "feature/second", guestCommit(t, f.harness, "fetch-source", "HEAD"))
	f.must("set -eu\ntest -f ignored-guest\ntest -f /home/hk/outside-fetch\n", "attach", "fetch-source")
}

func assertDirtyFetchOutput(t *testing.T, out string) {
	t.Helper()
	for _, fragment := range []string{"committed", "uncommitted", "remain"} {
		if !strings.Contains(strings.ToLower(out), fragment) {
			t.Fatalf("dirty fetch omitted %q: %s", fragment, out)
		}
	}
	for _, content := range []string{"private staged content", "private unstaged content", "private untracked content", "private ignored content"} {
		if strings.Contains(out, content) {
			t.Fatalf("fetch printed private file content: %s", out)
		}
	}
}

func testFetchUpdatesAndNamespaces(t *testing.T, f *fetchFixture) {
	t.Helper()
	ref := fetchedRef(t, f.harness, "fetch-source", "feature/second")
	old := fixtureGit(t, f.host, "rev-parse", ref)
	f.must("set -eu\nprintf 'next commit\\n' >> second.txt\ngit commit -am 'advance branch'\n", "attach", "fetch-source")
	next := guestCommit(t, f.harness, "fetch-source", "HEAD")
	assertCleanFetch(t, f, "fetch-source", "feature/second", next)
	if log := fixtureGit(t, f.host, "reflog", "show", "--format=%H", ref); !strings.Contains(log, old) {
		t.Fatalf("fast-forward lost previous tip: %s", log)
	}
	f.must("git reset --hard "+quote(old)+"\n", "attach", "fetch-source")
	out := assertCleanFetch(t, f, "fetch-source", "feature/second", old)
	if !strings.Contains(strings.ToLower(out), "non-fast-forward") {
		t.Fatalf("rewrite was not reported: %s", out)
	}
	if log := fixtureGit(t, f.host, "reflog", "show", "--format=%H", ref); !strings.Contains(log, next) {
		t.Fatalf("rewrite lost previous tip: %s", log)
	}
	retained := fetchedRef(t, f.harness, "fetch-source", "forced-0")
	f.must("git branch -D forced-0\n", "attach", "fetch-source")
	assertCleanFetch(t, f, "fetch-source", "feature/second", old)
	assertRef(t, f.host, retained, old)
	f.must("set -eu\nprintf 'sibling guest commit\\n' > 'file with spaces'\ngit commit -am 'sibling commit'\n", "attach", "fetch-sibling")
	sibling := guestCommit(t, f.harness, "fetch-sibling", "HEAD")
	assertCleanFetch(t, f, "fetch-sibling", f.branch, sibling)
	if instanceID(t, f.harness, "fetch-source") == instanceID(t, f.harness, "fetch-sibling") {
		t.Fatal("distinct VMs share fetched namespace")
	}
	assertRef(t, f.host, ref, old)
}

func testFetchRecreatedName(t *testing.T, f *fetchFixture) {
	t.Helper()
	oldID := instanceID(t, f.harness, "fetch-source")
	oldRefs := fixtureGit(t, f.host, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/hk/"+oldID)
	siblingRef := fetchedRef(t, f.harness, "fetch-sibling", f.branch)
	siblingTip := fixtureGit(t, f.host, "rev-parse", siblingRef)
	clone := filepath.Join(f.dir, "clean-recreation-source")
	fixtureGit(t, f.host, "clone", "--no-local", "--", f.host, clone)
	fixtureGit(t, clone, "config", "user.name", "Fetch Fixture")
	fixtureGit(t, clone, "config", "user.email", "fetch@example.invalid")
	f.must("", "delete", "--force", "fetch-source")
	cwd := f.cwd
	f.cwd = clone
	f.create("fetch-source")
	f.cwd = cwd
	if instanceID(t, f.harness, "fetch-source") == oldID {
		t.Fatal("recreated name reused old instance ID")
	}
	f.must("set -eu\nprintf 'recreated guest commit\\n' > 'file with spaces'\ngit commit -am 'recreated commit'\n", "attach", "fetch-source")
	assertCleanFetch(t, f, "fetch-source", f.branch, guestCommit(t, f.harness, "fetch-source", "HEAD"))
	if got := fixtureGit(t, f.host, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/hk/"+oldID); got != oldRefs {
		t.Fatal("recreated VM overwrote earlier namespace")
	}
	assertRef(t, f.host, siblingRef, siblingTip)
}

func stopFixtureVM(t *testing.T, h *harness, name string) {
	t.Helper()
	pid := vmPID(t, h.state(name, "state.json"))
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 5000); err != nil || n != 1 {
		t.Fatalf("fixture VM did not stop: %d %v", n, err)
	}
	assertGone(t, pid)
}

func vmRecoveryEvidence(t *testing.T, h *harness, name string) map[string]string {
	t.Helper()
	evidence := map[string]string{}
	for _, file := range []string{"rootfs.ext4", "seed.ext4", "state.json", "identity", "known_hosts", "config.json"} {
		input, err := os.Open(h.state(name, file))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, input)
		if err := errors.Join(copyErr, input.Close()); err != nil {
			t.Fatal(err)
		}
		evidence[file] = hex.EncodeToString(hash.Sum(nil))
	}
	return evidence
}

func testFetchStoppedVM(t *testing.T, f *fetchFixture, name string) {
	t.Helper()
	stopFixtureVM(t, f.harness, name)
	want, refs := vmRecoveryEvidence(t, f.harness, name), hostRefs(t, f)
	for _, args := range [][]string{{"fetch", name}, {"fetch", "--force", name}} {
		out, err := f.run("", args...)
		if err == nil || !strings.Contains(strings.ToLower(out), "running") || !strings.Contains(strings.ToLower(out), "preserv") || !strings.Contains(out, "hk start "+name) {
			t.Fatalf("offline fetch did not explain recovery: %v %s", err, out)
		}
		if strings.Contains(out, "recreate") || strings.Contains(out, "restart") {
			t.Fatalf("unsupported recovery advice: %s", out)
		}
		if !reflect.DeepEqual(want, vmRecoveryEvidence(t, f.harness, name)) || hostRefs(t, f) != refs {
			t.Fatal("offline fetch changed disk, keys, state, or fetched refs")
		}
		assertGone(t, vmPID(t, f.state(name, "state.json")))
	}
	f.must("test -f 'file with spaces' || exit 1\n", "attach", "fetch-sibling")
}

type pausedFetch struct {
	cmd                  *exec.Cmd
	done                 chan error
	output               bytes.Buffer
	marker, temp, bundle string
	completed            bool
}

func startPausedFetch(t *testing.T, f *fetchFixture, hook string) *pausedFetch {
	t.Helper()
	p := &pausedFetch{done: make(chan error, 1), marker: filepath.Join(t.TempDir(), "boundary"), temp: t.TempDir()}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	p.cmd = exec.CommandContext(ctx, f.bin, "fetch", "fetch-source")
	p.cmd.Dir, p.cmd.Env = f.cwd, append(append([]string{}, f.env...), hook+"="+p.marker, "TMPDIR="+p.temp)
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		if !p.completed {
			_ = p.cmd.Process.Kill()
			<-p.done
			p.completed = true
		}
	})
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-p.done:
			p.completed = true
			t.Fatalf("fetch exited before %s: %v %s", hook, err, p.output.String())
		case <-ctx.Done():
			t.Fatalf("fetch did not reach %s", hook)
		case <-tick.C:
			data, err := os.ReadFile(p.marker)
			if err != nil {
				continue
			}
			path := strings.TrimSpace(string(data))
			if !strings.HasPrefix(path, p.temp+string(filepath.Separator)) {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				continue
			}
			p.bundle = path
			return p
		}
	}
}

func (p *pausedFetch) release(t *testing.T) (string, error) {
	t.Helper()
	write(t, p.marker+".continue", nil)
	return p.wait(t)
}

func (p *pausedFetch) wait(t *testing.T) (string, error) {
	t.Helper()
	select {
	case err := <-p.done:
		p.completed = true
		return p.output.String(), err
	case <-time.After(60 * time.Second):
		t.Fatal("paused fetch did not finish")
		return "", context.DeadlineExceeded
	}
}

func (p *pausedFetch) assertPaused(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		p.completed = true
		t.Fatalf("fetch ended before controlled race completed: %v %s", err, p.output.String())
	default:
	}
}

func assertFetchTemporaryFilesRemoved(t *testing.T, p *pausedFetch) {
	t.Helper()
	entries, err := os.ReadDir(p.temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("host export/staging files leaked: %v %v", entries, err)
	}
}

func assertGuestExportsRemoved(t *testing.T, f *fetchFixture) {
	t.Helper()
	f.must("set -eu\nexports=$(find /tmp -maxdepth 1 -type d -name 'hk-export.*' -print)\ntest -z \"$exports\"\n", "attach", "fetch-source")
}

func TestFetchTransferFailuresAndSnapshot(t *testing.T) {
	f := newFetchFixture(t)
	f.create("fetch-source")
	f.create("fetch-sibling")
	f.must("set -eu\nhead -c 196608 /dev/urandom > payload.bin\nprintf x >> payload.bin\ngit add payload.bin\ngit commit -m 'large payload'\ngit branch fetch-extra\n", "attach", "fetch-source")
	assertCleanFetch(t, f, "fetch-source", f.branch, guestCommit(t, f.harness, "fetch-source", "HEAD"))
	assertCleanFetch(t, f, "fetch-sibling", f.branch, f.base)
	f.must("set -eu\ngit commit --allow-empty -m 'unfetched commit'\ngit branch -f fetch-extra HEAD\n", "attach", "fetch-source")
	testFetchCorruptedDownload(t, f)
	testFetchInterruptedDownload(t, f)
	testFetchConsistentEarlierSnapshot(t, f)
	testFetchCleanToDirtyTransition(t, f)
	testFetchSourceLostDuringDownload(t, f)
}

func testFetchCorruptedDownload(t *testing.T, f *fetchFixture) {
	t.Helper()
	refs := hostRefs(t, f)
	p := startPausedFetch(t, f, "HK_E2E_FETCH_AFTER_DOWNLOAD")
	info, err := os.Stat(p.bundle)
	if err != nil || info.Size() <= 65536 {
		t.Fatalf("completed bundle was not larger than 64 KiB: %v %v", info, err)
	}
	write(t, p.bundle, []byte("truncated corrupt bundle\n"))
	out, err := p.release(t)
	if err == nil || strings.Contains(out, "refs/remotes/hk/") {
		t.Fatalf("corrupt bundle reported success: %v %s", err, out)
	}
	if hostRefs(t, f) != refs {
		t.Fatal("validation failure partially published refs")
	}
	assertFetchTemporaryFilesRemoved(t, p)
	assertGuestExportsRemoved(t, f)
	f.must("test -f payload.bin || exit 1\n", "attach", "fetch-source")
	f.must("test -f 'file with spaces' || exit 1\n", "attach", "fetch-sibling")
}

func testFetchInterruptedDownload(t *testing.T, f *fetchFixture) {
	t.Helper()
	refs := hostRefs(t, f)
	p := startPausedFetch(t, f, "HK_E2E_FETCH_DURING_DOWNLOAD")
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	out, err := p.wait(t)
	if err == nil && !strings.Contains(strings.ToLower(out), "cancel") {
		t.Fatalf("interrupted fetch reported success: %s", out)
	}
	if hostRefs(t, f) != refs {
		t.Fatal("interrupted download partially published refs")
	}
	assertFetchTemporaryFilesRemoved(t, p)
	assertGuestExportsRemoved(t, f)
	f.must("test -f payload.bin || exit 1\n", "attach", "fetch-source")
	f.must("test -f 'file with spaces' || exit 1\n", "attach", "fetch-sibling")
}

func testFetchConsistentEarlierSnapshot(t *testing.T, f *fetchFixture) {
	t.Helper()
	earlier := guestCommit(t, f.harness, "fetch-source", "HEAD")
	refs := hostRefs(t, f)
	p := startPausedFetch(t, f, "HK_E2E_FETCH_AFTER_DOWNLOAD")
	f.must("git commit --allow-empty -m 'concurrent clean commit'\n", "attach", "fetch-source")
	later := guestCommit(t, f.harness, "fetch-source", "HEAD")
	if out, err := p.release(t); err != nil {
		if hostRefs(t, f) != refs || (!strings.Contains(strings.ToLower(out), "changed") && !strings.Contains(strings.ToLower(out), "snapshot")) {
			t.Fatalf("concurrent source change failed without a clear atomic result: %v %s", err, out)
		}
	} else {
		assertRef(t, f.host, fetchedRef(t, f.harness, "fetch-source", f.branch), earlier)
	}
	assertFetchTemporaryFilesRemoved(t, p)
	assertGuestExportsRemoved(t, f)
	assertCleanFetch(t, f, "fetch-source", f.branch, later)
}

func testFetchCleanToDirtyTransition(t *testing.T, f *fetchFixture) {
	t.Helper()
	f.must("git commit --allow-empty -m 'commit before dirty transition'\n", "attach", "fetch-source")
	refs := hostRefs(t, f)
	p := startPausedFetch(t, f, "HK_E2E_FETCH_AFTER_DOWNLOAD")
	f.must("printf 'new uncommitted change\\n' >> 'file with spaces'\n", "attach", "fetch-source")
	out, err := p.release(t)
	if err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("clean-to-dirty transition bypassed approval: %v %s", err, out)
	}
	if hostRefs(t, f) != refs {
		t.Fatal("newly dirty guest published unapproved refs")
	}
	assertFetchTemporaryFilesRemoved(t, p)
	assertGuestExportsRemoved(t, f)
	assertDirtyFetchOutput(t, f.must("", "fetch", "--force", "fetch-source"))
	assertRef(t, f.host, fetchedRef(t, f.harness, "fetch-source", f.branch), guestCommit(t, f.harness, "fetch-source", "HEAD"))
	f.must("set -eu\ngrep 'new uncommitted change' 'file with spaces'\ngit checkout -- 'file with spaces'\n", "attach", "fetch-source")
}

func testFetchSourceLostDuringDownload(t *testing.T, f *fetchFixture) {
	t.Helper()
	f.must("git commit --allow-empty -m 'unfetched before source stop'\n", "attach", "fetch-source")
	refs := hostRefs(t, f)
	p := startPausedFetch(t, f, "HK_E2E_FETCH_DURING_DOWNLOAD")
	stopFixtureVM(t, f.harness, "fetch-source")
	preserved := vmRecoveryEvidence(t, f.harness, "fetch-source")
	out, err := p.release(t)
	if err == nil {
		t.Fatalf("stopped source reported fetch success: %s", out)
	}
	if hostRefs(t, f) != refs || !reflect.DeepEqual(preserved, vmRecoveryEvidence(t, f.harness, "fetch-source")) {
		t.Fatal("source loss changed host refs or recovery disk/keys/state")
	}
	assertFetchTemporaryFilesRemoved(t, p)
	f.must("test -f 'file with spaces' || exit 1\n", "attach", "fetch-sibling")
}

func TestFetchUnavailableSourcesAndDestinations(t *testing.T) {
	f := newFetchFixture(t)
	f.create("fetch-source")
	makeLegacyFetchState(t, f)
	assertCleanFetch(t, f, "fetch-source", f.branch, f.base)
	id, pid := instanceID(t, f.harness, "fetch-source"), vmPID(t, f.state("fetch-source", "state.json"))
	assertCleanFetch(t, f, "fetch-source", f.branch, f.base)
	if instanceID(t, f.harness, "fetch-source") != id || vmPID(t, f.state("fetch-source", "state.json")) != pid {
		t.Fatal("legacy fetch identity was not stable or recreated VM")
	}
	cwd := f.cwd
	unrelated := filepath.Join(f.dir, "unrelated")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, unrelated, "init", "--initial-branch=main")
	fixtureGit(t, unrelated, "commit", "--allow-empty", "-m", "unrelated base")
	f.cwd = unrelated
	assertFetchRefusal(t, f, "destination", "fetch", "fetch-source")
	f.cwd = cwd
	f.must("set -eu\ngit checkout --detach\ngit commit --allow-empty -m 'unreferenced detached work'\n", "attach", "fetch-source")
	assertFetchRefusal(t, f, "branch", "fetch", "--force", "fetch-source")
	f.must("git checkout "+quote(f.branch)+"\n", "attach", "fetch-source")
	f.must("mv .git .git-saved\n", "attach", "fetch-source")
	assertFetchRefusal(t, f, "", "fetch", "--force", "fetch-source")
	f.must("mv .git-saved .git\n", "attach", "fetch-source")
	f.cwd = f.dir
	f.must("", "create", "--name", "fetch-empty")
	f.cwd = cwd
	assertFetchRefusal(t, f, "project", "fetch", "fetch-empty")
	socket := f.state("fetch-source", "vsock.sock")
	if err := os.Rename(socket, socket+".disconnected"); err != nil {
		t.Fatal(err)
	}
	assertFetchRefusal(t, f, "", "fetch", "--force", "fetch-source")
	if err := os.Rename(socket+".disconnected", socket); err != nil {
		t.Fatal(err)
	}
	f.must("test -f 'file with spaces' || exit 1\n", "attach", "fetch-source")
	f.must("set -eu\ncd /home/hk\nmv workspace workspace-missing\n", "attach", "fetch-source")
	assertFetchRefusal(t, f, "workspace", "fetch", "--force", "fetch-source")
	if instanceID(t, f.harness, "fetch-source") != id || vmPID(t, f.state("fetch-source", "state.json")) != pid {
		t.Fatal("failed fetch replaced the source VM")
	}
	for _, name := range []string{"fetch-source", "fetch-empty"} {
		for _, file := range []string{"rootfs.ext4", "identity", "known_hosts", "state.json"} {
			if _, err := os.Stat(f.state(name, file)); err != nil {
				t.Fatalf("failed fetch lost %s/%s: %v", name, file, err)
			}
		}
	}
}

func makeLegacyFetchState(t *testing.T, f *fetchFixture) {
	t.Helper()
	path := f.state("fetch-source", "state.json")
	var state map[string]any
	if err := json.Unmarshal(read(t, path), &state); err != nil {
		t.Fatal(err)
	}
	delete(state, "instance_id")
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, data)
}

func assertFetchRefusal(t *testing.T, f *fetchFixture, fragment string, args ...string) {
	t.Helper()
	refs := hostRefs(t, f)
	out, err := f.run("", args...)
	if err == nil || !strings.Contains(strings.ToLower(out), fragment) {
		t.Fatalf("invalid fetch was not explained: %v %s", err, out)
	}
	if hostRefs(t, f) != refs {
		t.Fatal("failed fetch changed host refs")
	}
}

func TestFetchRejectsReplacedInstance(t *testing.T) {
	f := newFetchFixture(t)
	f.create("fetch-source")
	assertCleanFetch(t, f, "fetch-source", f.branch, f.base)
	oldID := instanceID(t, f.harness, "fetch-source")
	oldRef := fetchedRef(t, f.harness, "fetch-source", f.branch)
	f.must("set -eu\nprintf 'old instance unfetched work\\n' > 'file with spaces'\ngit commit -am 'old instance work'\n", "attach", "fetch-source")
	refs := hostRefs(t, f)
	p := startPausedFetch(t, f, "HK_E2E_FETCH_AFTER_DOWNLOAD")
	f.must("", "delete", "--force", "fetch-source")
	p.assertPaused(t)
	f.create("fetch-source")
	p.assertPaused(t)
	newID := instanceID(t, f.harness, "fetch-source")
	if oldID == newID {
		t.Fatal("replacement retained old identity")
	}
	out, err := p.release(t)
	if err == nil {
		t.Fatalf("fetch published deleted instance snapshot: %s", out)
	}
	if hostRefs(t, f) != refs {
		t.Fatal("replacement race published partial or misattributed refs")
	}
	if got := fixtureGit(t, f.host, "for-each-ref", "--format=%(refname)", "refs/remotes/hk/"+newID); got != "" {
		t.Fatalf("old snapshot escaped into replacement namespace: %s", got)
	}
	assertFetchTemporaryFilesRemoved(t, p)
	assertRef(t, f.host, oldRef, f.base)
	if got := guestCommit(t, f.harness, "fetch-source", "HEAD"); got != f.base {
		t.Fatalf("replacement inherited old guest work: %s", got)
	}
	f.must("set -eu\nprintf 'replacement instance work\\n' > 'file with spaces'\ngit commit -am 'replacement instance work'\n", "attach", "fetch-source")
	assertCleanFetch(t, f, "fetch-source", f.branch, guestCommit(t, f.harness, "fetch-source", "HEAD"))
	assertRef(t, f.host, oldRef, f.base)
}
