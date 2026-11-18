package vm

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchConfigUsesWritebackRootDrive(t *testing.T) {
	dir := privateTempDir(t)
	config := firecrackerConfig("/images", dir)
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
