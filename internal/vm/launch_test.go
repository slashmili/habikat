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
	capability, err := imageShutdownCapability(dir)
	if err != nil || capability != "" {
		t.Fatalf("legacy image: %q %v", capability, err)
	}
	manifest["shutdown"] = shutdownCapability
	writeManifest(t, dir, manifest)
	capability, err = imageShutdownCapability(dir)
	if err != nil || capability != shutdownCapability {
		t.Fatalf("new image: %q %v", capability, err)
	}
	manifest["shutdown"] = "unknown"
	writeManifest(t, dir, manifest)
	if _, err := imageShutdownCapability(dir); err == nil {
		t.Fatal("unknown capability accepted")
	}
}

func TestImageLaunchArtifactsRecordsVerifiedDigests(t *testing.T) {
	dir, manifest := imageFixture(t)
	launch, err := imageLaunchArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := launchArtifacts{
		Firecracker: artifactRef{Path: filepath.Join(dir, "firecracker"), SHA256: manifest["firecracker"]},
		Kernel:      artifactRef{Path: filepath.Join(dir, "vmlinux"), SHA256: manifest["vmlinux"]},
	}
	if launch != want || !validLaunchArtifacts(launch) {
		t.Fatalf("launch = %+v; want %+v", launch, want)
	}
	if err := verifyLaunchArtifacts(launch); err != nil {
		t.Fatal(err)
	}
	delete(manifest, "rootfs.ext4")
	writeManifest(t, dir, manifest)
	if _, err := imageLaunchArtifacts(dir); err != nil {
		t.Fatalf("launch artifacts must not depend on the base root disk: %v", err)
	}
	manifest["vmlinux"] = "short"
	writeManifest(t, dir, manifest)
	assertErrorContains(t, func() error { _, err := imageLaunchArtifacts(dir); return err }(), "vmlinux")
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
