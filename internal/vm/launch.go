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

type verifiedImage struct {
	launch   launchArtifacts
	rootDisk artifactRef
}

func verifyImages(images string) (verifiedImage, error) {
	data, err := os.ReadFile(filepath.Join(images, "manifest.json"))
	if err != nil {
		return verifiedImage{}, fmt.Errorf("build the VM image with images/build.py first: %w", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return verifiedImage{}, fmt.Errorf("read image manifest: %w", err)
	}
	image := verifiedImage{
		launch: launchArtifacts{
			Firecracker: artifactRef{Path: filepath.Join(images, "firecracker"), SHA256: manifest["firecracker"]},
			Kernel:      artifactRef{Path: filepath.Join(images, "vmlinux"), SHA256: manifest["vmlinux"]},
		},
		rootDisk: artifactRef{Path: filepath.Join(images, "rootfs.ext4"), SHA256: manifest["rootfs.ext4"]},
	}
	for _, artifact := range []artifactRef{image.launch.Firecracker, image.launch.Kernel, image.rootDisk} {
		if len(artifact.SHA256) != 64 {
			return verifiedImage{}, fmt.Errorf("missing SHA-256 for %s", filepath.Base(artifact.Path))
		}
		if err := verifyImageFile(artifact.Path, artifact.SHA256); err != nil {
			return verifiedImage{}, err
		}
	}
	if err := requireShutdownCapability(manifest); err != nil {
		return verifiedImage{}, err
	}
	return image, nil
}

func verifyImageFile(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	digest, err := readerSHA256(file)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	if digest != expected {
		return fmt.Errorf("SHA-256 mismatch for %s", filepath.Base(path))
	}
	return nil
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

const shutdownCapability = "ctrl-alt-del-v1"

func requireShutdownCapability(manifest map[string]string) error {
	if manifest["shutdown"] != shutdownCapability {
		return fmt.Errorf("image does not support graceful shutdown (requires %q); rebuild the image with images/build.py into a new directory", shutdownCapability)
	}
	return nil
}
