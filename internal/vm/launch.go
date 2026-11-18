package vm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func firecrackerConfig(images, dir string) map[string]any {
	return map[string]any{
		"boot-source":    map[string]any{"kernel_image_path": filepath.Join(images, "vmlinux"), "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init"},
		"machine-config": map[string]any{"vcpu_count": 1, "mem_size_mib": 256},
		"drives": []map[string]any{
			{"drive_id": "root", "path_on_host": filepath.Join(dir, "rootfs.ext4"), "is_root_device": true, "is_read_only": false, "cache_type": "Writeback"},
			{"drive_id": "seed", "path_on_host": filepath.Join(dir, "seed.ext4"), "is_root_device": false, "is_read_only": true},
		},
		"vsock": map[string]any{"guest_cid": 3, "uds_path": filepath.Join(dir, "vsock.sock")},
	}
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
