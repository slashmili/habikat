package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchConfigUsesWritebackRootDrive(t *testing.T) {
	dir := privateTempDir(t)
	config := firecrackerConfig("/images/vmlinux", dir)
	if kernel := config["boot-source"].(map[string]any)["kernel_image_path"]; kernel != "/images/vmlinux" {
		t.Fatalf("kernel path = %v", kernel)
	}
	drives := config["drives"].([]map[string]any)
	if drives[0]["cache_type"] != "Writeback" || drives[0]["path_on_host"] != filepath.Join(dir, "rootfs.ext4") {
		t.Fatalf("unsafe root drive: %+v", drives[0])
	}
}

func TestSocketPathLimit(t *testing.T) {
	longestSocket := len("/vsock.sock")
	atLimit := "/" + strings.Repeat("a", 107-longestSocket-1)
	if err := validateSocketPaths(atLimit); err != nil {
		t.Fatalf("107-byte socket path rejected: %v", err)
	}
	if err := validateSocketPaths(atLimit + "a"); err == nil {
		t.Fatal("108-byte socket path accepted")
	}
}

func TestImageShutdownCapability(t *testing.T) {
	dir, manifest := imageFixture(t)
	if _, err := verifyImages(dir); err != nil {
		t.Fatalf("current image: %v", err)
	}
	for _, capability := range []string{"", "unknown"} {
		manifest["shutdown"] = capability
		writeManifest(t, dir, manifest)
		_, err := verifyImages(dir)
		assertErrorContains(t, err, "graceful shutdown")
	}
	delete(manifest, "shutdown")
	writeManifest(t, dir, manifest)
	_, err := verifyImages(dir)
	assertErrorContains(t, err, "rebuild the image")
}

func TestImageLaunchArtifactsRecordsVerifiedDigests(t *testing.T) {
	dir, manifest := imageFixture(t)
	image, err := verifyImages(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := launchArtifacts{
		Firecracker: artifactRef{Path: filepath.Join(dir, "firecracker"), SHA256: manifest["firecracker"]},
		Kernel:      artifactRef{Path: filepath.Join(dir, "vmlinux"), SHA256: manifest["vmlinux"]},
	}
	if image.launch != want || !validLaunchArtifacts(image.launch) {
		t.Fatalf("launch = %+v; want %+v", image.launch, want)
	}
	wantDisk := artifactRef{Path: filepath.Join(dir, "rootfs.ext4"), SHA256: manifest["rootfs.ext4"]}
	if image.rootDisk != wantDisk {
		t.Fatalf("root disk = %+v; want %+v", image.rootDisk, wantDisk)
	}
}

func TestWriteLaunchConfigReplacesStaleConfig(t *testing.T) {
	dir := privateTempDir(t)
	writeTestFile(t, filepath.Join(dir, "config.json"), []byte(`{"boot-source":{"kernel_image_path":"/old/vmlinux"}}`))
	if err := writeLaunchConfig(dir, "/recorded/vmlinux"); err != nil {
		t.Fatal(err)
	}
	var config struct {
		BootSource struct {
			Kernel string `json:"kernel_image_path"`
		} `json:"boot-source"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil || json.Unmarshal(data, &config) != nil || config.BootSource.Kernel != "/recorded/vmlinux" {
		t.Fatalf("kernel = %q, %v", config.BootSource.Kernel, err)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config mode: %v %v", info, err)
	}
}

func TestPrivateDisksRejectSharedInodes(t *testing.T) {
	dir := privateTempDir(t)
	for _, name := range []string{"rootfs.ext4", "seed.ext4"} {
		writeTestFile(t, filepath.Join(dir, name), []byte("valuable data"))
	}
	outside := filepath.Join(privateTempDir(t), "sibling-disk")
	if err := os.Link(filepath.Join(dir, "rootfs.ext4"), outside); err != nil {
		t.Fatal(err)
	}
	if err := requireUnusedDisks(dir); err == nil {
		t.Fatal("accepted a private disk shared with another directory")
	}
	assertFileContents(t, outside, []byte("valuable data"))
}
