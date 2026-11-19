package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func firecrackerConfig(kernel, dir string) map[string]any {
	return map[string]any{
		"boot-source":    map[string]any{"kernel_image_path": kernel, "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init"},
		"machine-config": map[string]any{"vcpu_count": 1, "mem_size_mib": 256},
		"drives": []map[string]any{
			{"drive_id": "root", "path_on_host": filepath.Join(dir, "rootfs.ext4"), "is_root_device": true, "is_read_only": false, "cache_type": "Writeback"},
			{"drive_id": "seed", "path_on_host": filepath.Join(dir, "seed.ext4"), "is_root_device": false, "is_read_only": true},
		},
		"vsock": map[string]any{"guest_cid": 3, "uds_path": filepath.Join(dir, "vsock.sock")},
	}
}

func writeLaunchConfig(dir, kernel string) error {
	data, err := json.Marshal(firecrackerConfig(kernel, dir))
	if err != nil {
		return err
	}
	return replaceFile(dir, "config.json", data)
}

func imageLaunchArtifacts(images string) (launchArtifacts, error) {
	data, err := os.ReadFile(filepath.Join(images, "manifest.json"))
	if err != nil {
		return launchArtifacts{}, err
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return launchArtifacts{}, fmt.Errorf("read image manifest: %w", err)
	}
	launch := launchArtifacts{
		Firecracker: artifactRef{Path: filepath.Join(images, "firecracker"), SHA256: manifest["firecracker"]},
		Kernel:      artifactRef{Path: filepath.Join(images, "vmlinux"), SHA256: manifest["vmlinux"]},
	}
	for _, artifact := range []artifactRef{launch.Firecracker, launch.Kernel} {
		if !validArtifact(artifact) {
			return launchArtifacts{}, fmt.Errorf("image manifest has no valid SHA-256 for %s", filepath.Base(artifact.Path))
		}
	}
	return launch, nil
}

func verifyLaunchArtifacts(launch launchArtifacts) error {
	for _, artifact := range []artifactRef{launch.Firecracker, launch.Kernel} {
		if err := verifyArtifact(artifact); err != nil {
			return err
		}
	}
	return nil
}

func verifyArtifact(artifact artifactRef) error {
	file, err := os.Open(artifact.Path)
	if err != nil {
		return fmt.Errorf("launch artifact %s is unavailable: %w", artifact.Path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("launch artifact %s: %w", artifact.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("launch artifact %s must be a regular file", artifact.Path)
	}
	digest, err := readerSHA256(file)
	if err != nil {
		return fmt.Errorf("launch artifact %s: %w", artifact.Path, err)
	}
	if digest != artifact.SHA256 {
		return fmt.Errorf("launch artifact %s does not match its recorded SHA-256", artifact.Path)
	}
	return nil
}

func readerSHA256(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateSocketPaths(dir string) error {
	for _, name := range []string{"vsock.sock", "api.sock"} {
		if len(filepath.Join(dir, name)) > 107 {
			return errors.New("state path is too long for a Unix socket; choose a shorter HK_STATE_DIR")
		}
	}
	return nil
}

func imageShutdownCapability(images string) (string, error) {
	data, err := os.ReadFile(filepath.Join(images, "manifest.json"))
	if err != nil {
		return "", err
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	capability := manifest["shutdown"]
	if capability != "" && capability != shutdownCapability {
		return "", errors.New("unsupported image shutdown capability")
	}
	return capability, nil
}
