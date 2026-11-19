package vm

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type nixosManifest struct {
	Version     int    `json:"version"`
	Profile     string `json:"profile"`
	Firecracker string `json:"firecracker"`
	Kernel      string `json:"kernel"`
	Initrd      string `json:"initrd"`
	System      string `json:"system"`
	Boot        string `json:"boot"`
}

type nixosBoot struct {
	Profile   string   `json:"profile"`
	BootArgs  string   `json:"boot_args"`
	MemoryMiB int      `json:"memory_mib"`
	VCPUs     int      `json:"vcpus"`
	EnablePCI bool     `json:"enable_pci"`
	Disks     []string `json:"disks"`
}

type nixosArtifacts struct {
	Initrd   artifactRef `json:"initrd"`
	System   artifactRef `json:"system"`
	Metadata artifactRef `json:"metadata"`
	Boot     nixosBoot   `json:"boot"`
}

func decodeNixosManifest(data []byte) (nixosManifest, error) {
	var manifest nixosManifest
	if err := jsonv2.Unmarshal(data, &manifest, jsonv2.RejectUnknownMembers(true)); err != nil {
		return manifest, fmt.Errorf("NixOS manifest: %w", err)
	}
	if manifest.Version != 1 || manifest.Profile != "nixos-v1" {
		return manifest, errors.New("unsupported NixOS manifest version or profile")
	}
	for _, digest := range []string{manifest.Firecracker, manifest.Kernel, manifest.Initrd, manifest.System, manifest.Boot} {
		if !validDigest.MatchString(digest) {
			return manifest, errors.New("NixOS manifest requires every SHA-256 digest")
		}
	}
	return manifest, nil
}

func decodeNixosBoot(data []byte) (nixosBoot, error) {
	var boot nixosBoot
	if err := jsonv2.Unmarshal(data, &boot, jsonv2.RejectUnknownMembers(true)); err != nil {
		return boot, fmt.Errorf("NixOS boot metadata: %w", err)
	}
	return boot, validateNixosBoot(boot)
}

var nixosInit = regexp.MustCompile(`^init=/nix/store/[0-9a-df-np-sv-z]{32}-nixos-system-habikat-[0-9a-z.+-]+/init$`)

const nixosBootPrefix = "console=ttyS0,115200 reboot=k panic=1 i8042.noaux i8042.nomux i8042.dumbkbd 8250.nr_uarts=1 quiet root=fstab loglevel=4 lsm=landlock,yama,bpf "

func validateNixosBoot(boot nixosBoot) error {
	if boot.Profile != "nixos-v1" || boot.MemoryMiB != 512 || boot.VCPUs != 1 || !boot.EnablePCI || !slices.Equal(boot.Disks, []string{"system", "data", "seed"}) {
		return errors.New("unsupported NixOS boot profile, resources, or disk roles")
	}
	init, ok := strings.CutPrefix(boot.BootArgs, nixosBootPrefix)
	if !ok || len(boot.BootArgs) > 2048 || !nixosInit.MatchString(init) {
		return errors.New("unsupported NixOS boot parameters: requires the fixed profile and one immutable init path")
	}
	return nil
}

func verifyNixosImages(images string) (verifiedImage, error) {
	data, err := os.ReadFile(filepath.Join(images, "manifest.json"))
	if err != nil {
		return verifiedImage{}, fmt.Errorf("build the experiment with make nixos-image first: %w", err)
	}
	manifest, err := decodeNixosManifest(data)
	if err != nil {
		return verifiedImage{}, err
	}
	ref := func(name, digest string) artifactRef {
		return artifactRef{Path: filepath.Join(images, name), SHA256: digest}
	}
	launch := launchArtifacts{Firecracker: ref("firecracker", manifest.Firecracker), Kernel: ref("vmlinux", manifest.Kernel), NixOS: &nixosArtifacts{Initrd: ref("initrd", manifest.Initrd), System: ref("system.erofs", manifest.System), Metadata: ref("boot.json", manifest.Boot)}}
	for _, artifact := range launchFiles(launch) {
		if err := verifyArtifact(artifact); err != nil {
			return verifiedImage{}, err
		}
	}
	data, err = os.ReadFile(launch.NixOS.Metadata.Path)
	if err != nil {
		return verifiedImage{}, err
	}
	launch.NixOS.Boot, err = decodeNixosBoot(data)
	if err != nil {
		return verifiedImage{}, err
	}
	return verifiedImage{launch: launch}, nil
}

func launchFiles(launch launchArtifacts) []artifactRef {
	files := []artifactRef{launch.Firecracker, launch.Kernel}
	if launch.NixOS != nil {
		files = append(files, launch.NixOS.Initrd, launch.NixOS.System, launch.NixOS.Metadata)
	}
	return files
}

func nixosConfig(launch launchArtifacts, dir string) map[string]any {
	boot := launch.NixOS.Boot
	return map[string]any{
		"boot-source":    map[string]any{"kernel_image_path": launch.Kernel.Path, "initrd_path": launch.NixOS.Initrd.Path, "boot_args": boot.BootArgs},
		"machine-config": map[string]any{"vcpu_count": boot.VCPUs, "mem_size_mib": boot.MemoryMiB},
		"drives": []map[string]any{
			{"drive_id": "system", "path_on_host": launch.NixOS.System.Path, "is_root_device": false, "is_read_only": true},
			{"drive_id": "data", "path_on_host": filepath.Join(dir, "rootfs.ext4"), "is_root_device": false, "is_read_only": false, "cache_type": "Writeback"},
			{"drive_id": "seed", "path_on_host": filepath.Join(dir, "seed.ext4"), "is_root_device": false, "is_read_only": true},
		},
		"vsock": map[string]any{"guest_cid": 3, "uds_path": filepath.Join(dir, "vsock.sock")},
	}
}

func writeImageLaunchConfig(dir string, launch launchArtifacts) error {
	if launch.NixOS == nil {
		return writeLaunchConfig(dir, launch.Kernel.Path)
	}
	data, err := json.Marshal(nixosConfig(launch, dir))
	if err != nil {
		return err
	}
	return replaceFile(dir, "config.json", data)
}

func provisionImageDisk(ctx context.Context, image verifiedImage, dir string) error {
	if image.launch.NixOS == nil {
		return copyVerifiedRootDisk(ctx, image.rootDisk, dir)
	}
	path := filepath.Join(dir, "rootfs.ext4")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := errors.Join(file.Truncate(2<<30), file.Close()); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-E", "root_owner=0:0", path)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("initialize private NixOS data disk: %w: %s", err, output)
	}
	return nil
}

func verifyActiveImages(images string) (verifiedImage, error) {
	if nixosExperiment {
		return verifyNixosImages(images)
	}
	return verifyImages(images)
}
