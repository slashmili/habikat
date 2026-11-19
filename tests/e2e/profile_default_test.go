//go:build e2e && !nixexperiment

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const e2eBuildTags = "e2e"
const expectedGuestRelease = "6.1.188"
const expectedGuestGit = "git version 2.52.1"

func TestBootFailureCleanup(t *testing.T) {
	h := newHarness(t)
	images := os.Getenv("HK_IMAGE_DIR")
	if images == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		images = filepath.Join(cache, "habikat", "image")
	}
	broken := filepath.Join(h.dir, "broken-image")
	if err := os.Mkdir(broken, 0700); err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(read(t, filepath.Join(images, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"firecracker", "rootfs.ext4"} {
		if err := os.Symlink(filepath.Join(images, file), filepath.Join(broken, file)); err != nil {
			t.Fatal(err)
		}
	}
	badKernel := []byte("not an ELF kernel")
	write(t, filepath.Join(broken, "vmlinux"), badKernel)
	hash := sha256.Sum256(badKernel)
	manifest["vmlinux"] = hex.EncodeToString(hash[:])
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(broken, "manifest.json"), data)
	h.env = append(h.env, "HK_IMAGE_DIR="+broken)
	if out, err := h.run("", "create", "--name", "badboot"); err == nil || !strings.Contains(out, "Firecracker") {
		t.Fatalf("expected actual Firecracker boot failure: %v %s", err, out)
	}
	if _, err := os.Stat(h.state("badboot", "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("boot failure retained disposable state: %v", err)
	}
}
