package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nixosManifestFixture() nixosManifest {
	digest := strings.Repeat("a", 64)
	return nixosManifest{Version: 1, Profile: "nixos-v1", Firecracker: digest, Kernel: digest, Initrd: digest, System: digest, Boot: digest}
}

func nixosBootFixture() nixosBoot {
	return nixosBoot{Profile: "nixos-v1", BootArgs: "console=ttyS0,115200 reboot=k panic=1 i8042.noaux i8042.nomux i8042.dumbkbd 8250.nr_uarts=1 quiet root=fstab loglevel=4 lsm=landlock,yama,bpf init=/nix/store/" + strings.Repeat("a", 32) + "-nixos-system-habikat-26.11/init", MemoryMiB: 512, VCPUs: 1, EnablePCI: true, Disks: []string{"system", "data", "seed"}}
}

func TestNixosManifestBoundary(t *testing.T) {
	data, _ := json.Marshal(nixosManifestFixture())
	if _, err := decodeNixosManifest(data); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{}`, `null`, string(data) + `{}`, strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(data), `"version":1`, `"version":null`, 1),
		strings.Replace(string(data), `"nixos-v1"`, `"alpine-v1"`, 1),
		strings.Replace(string(data), `"kernel":`, `"unknown":`, 1),
		strings.Replace(string(data), strings.Repeat("a", 64), "../outside", 1),
	} {
		if _, err := decodeNixosManifest([]byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestNixosBootBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*nixosBoot)
	}{
		{"disk order", func(b *nixosBoot) { b.Disks = []string{"data", "system", "seed"} }},
		{"extra disk", func(b *nixosBoot) { b.Disks = append(b.Disks, "../outside") }},
		{"memory", func(b *nixosBoot) { b.MemoryMiB = 256 }},
		{"pci", func(b *nixosBoot) { b.EnablePCI = false }},
		{"unsafe init", func(b *nixosBoot) { b.BootArgs = "init=/tmp/program" }},
		{"extra init", func(b *nixosBoot) { b.BootArgs += " init=/bin/sh" }},
		{"newline", func(b *nixosBoot) { b.BootArgs += "\n" }},
		{"duplicate parameter", func(b *nixosBoot) { b.BootArgs += " quiet" }},
		{"missing root", func(b *nixosBoot) { b.BootArgs = strings.Replace(b.BootArgs, "root=fstab ", "", 1) }},
		{"control character in init", func(b *nixosBoot) { b.BootArgs = strings.Replace(b.BootArgs, "26.11/init", "26.11\x01/init", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := nixosBootFixture()
			tc.change(&boot)
			if validateNixosBoot(boot) == nil {
				t.Fatal("accepted invalid boot metadata")
			}
		})
	}
	if err := validateNixosBoot(nixosBootFixture()); err != nil {
		t.Fatal(err)
	}
}

func TestNixosConfigKeepsSharedArtifactsReadOnly(t *testing.T) {
	launch := launchArtifacts{Kernel: artifactRef{Path: "/images/vmlinux"}, NixOS: &nixosArtifacts{Initrd: artifactRef{Path: "/images/initrd"}, System: artifactRef{Path: "/images/system.erofs"}, Boot: nixosBootFixture()}}
	dir := privateTempDir(t)
	config := nixosConfig(launch, dir)
	drives := config["drives"].([]map[string]any)
	if drives[0]["path_on_host"] != "/images/system.erofs" || drives[0]["is_read_only"] != true {
		t.Fatal(drives)
	}
	for _, drive := range drives[1:] {
		if filepath.Dir(drive["path_on_host"].(string)) != dir {
			t.Fatal("disk escaped private directory")
		}
	}
	if drives[1]["cache_type"] != "Writeback" || drives[2]["is_read_only"] != true {
		t.Fatal(drives)
	}
	if _, ok := config["network-interfaces"]; ok {
		t.Fatal("unexpected guest network")
	}
}

func FuzzNixosManifest(f *testing.F) {
	valid, _ := json.Marshal(nixosManifestFixture())
	f.Add(valid)
	f.Add([]byte(`null`))
	f.Add([]byte(`{"version":1,"version":2}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16384 {
			return
		}
		manifest, err := decodeNixosManifest(data)
		if err != nil {
			return
		}
		encoded, _ := json.Marshal(manifest)
		if _, err := decodeNixosManifest(encoded); err != nil {
			t.Fatal(err)
		}
		if manifest.Profile != "nixos-v1" || !validDigest.MatchString(manifest.System) {
			t.Fatal("invalid manifest accepted")
		}
	})
}

func FuzzNixosBoot(f *testing.F) {
	valid, _ := json.Marshal(nixosBootFixture())
	f.Add(valid)
	f.Add([]byte(`null`))
	f.Add([]byte(`{"disks":["../../outside"]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16384 {
			return
		}
		boot, err := decodeNixosBoot(data)
		if err != nil {
			return
		}
		if len(boot.Disks) != 3 || boot.Disks[1] != "data" || boot.MemoryMiB != 512 || !boot.EnablePCI {
			t.Fatal("unsafe boot accepted")
		}
	})
}

func TestNixosRestartArtifactFailurePreservesPrivateData(t *testing.T) {
	env := startFixture(t)
	boot := nixosBootFixture()
	initrd := filepath.Join(env.image, "initrd")
	system := filepath.Join(env.image, "system.erofs")
	metadata := filepath.Join(env.image, "boot.json")
	for _, path := range []string{initrd, system, metadata} {
		writeTestFile(t, path, []byte("fixture"))
	}
	digest, _ := fileDigest(initrd)
	env.state.Launch.NixOS = &nixosArtifacts{Initrd: artifactRef{Path: initrd, SHA256: digest}, System: artifactRef{Path: system, SHA256: digest}, Metadata: artifactRef{Path: metadata, SHA256: digest}, Boot: boot}
	if err := os.Remove(initrd); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLaunch(env.dir, env.state); err == nil {
		t.Fatal("missing initrd accepted")
	}
	env.assertStorage(t)
}

func testProfileLaunch(launch *launchArtifacts) *launchArtifacts {
	if !nixosExperiment {
		return launch
	}
	dir := filepath.Dir(launch.Kernel.Path)
	ref := func(name string) artifactRef {
		return artifactRef{Path: filepath.Join(dir, name), SHA256: strings.Repeat("a", 64)}
	}
	launch.NixOS = &nixosArtifacts{Initrd: ref("initrd"), System: ref("system.erofs"), Metadata: ref("boot.json"), Boot: nixosBootFixture()}
	return launch
}

func TestNixosRejectsWrongProfileAndUnsafeRecordedPaths(t *testing.T) {
	state := currentRecord(t)
	data, err := encodeRecord(state)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["profile"] = "other-profile"
	data, _ = json.Marshal(fields)
	if _, err := decodeRecord(data); err == nil {
		t.Fatal("accepted wrong-profile state")
	}
	if !nixosExperiment {
		return
	}
	for _, path := range []string{"relative/initrd", "/images/../initrd", "/images/initrd\x00"} {
		state := currentRecord(t)
		state.Launch.NixOS.Initrd.Path = path
		data, _ := encodeRecord(state)
		if _, err := decodeRecord(data); err == nil {
			t.Fatalf("accepted unsafe path %q", path)
		}
	}
	state = currentRecord(t)
	state.Launch.NixOS = nil
	data, _ = encodeRecord(state)
	if _, err := decodeRecord(data); err == nil {
		t.Fatal("accepted incomplete experiment state")
	}
}

func TestNixosPrivateDiskCannotOverwriteOutsideState(t *testing.T) {
	dir := privateTempDir(t)
	outside := filepath.Join(privateTempDir(t), "data")
	writeTestFile(t, outside, []byte("valuable data"))
	if err := os.Symlink(outside, filepath.Join(dir, "rootfs.ext4")); err != nil {
		t.Fatal(err)
	}
	image := verifiedImage{launch: launchArtifacts{NixOS: &nixosArtifacts{}}}
	if err := provisionImageDisk(t.Context(), image, dir); err == nil {
		t.Fatal("overwrote existing private disk path")
	}
	assertFileContents(t, outside, []byte("valuable data"))
}

func nixosImageFixture(t *testing.T) string {
	t.Helper()
	dir := privateTempDir(t)
	manifest := nixosManifestFixture()
	for _, name := range []string{"firecracker", "vmlinux", "initrd", "system.erofs"} {
		writeTestFile(t, filepath.Join(dir, name), []byte("artifact"))
	}
	digest, _ := fileDigest(filepath.Join(dir, "initrd"))
	manifest.Firecracker, manifest.Kernel, manifest.Initrd, manifest.System = digest, digest, digest, digest
	boot, _ := json.Marshal(nixosBootFixture())
	writeTestFile(t, filepath.Join(dir, "boot.json"), boot)
	manifest.Boot, _ = fileDigest(filepath.Join(dir, "boot.json"))
	data, _ := json.Marshal(manifest)
	writeTestFile(t, filepath.Join(dir, "manifest.json"), data)
	return dir
}

func FuzzNixosArtifactValidation(f *testing.F) {
	f.Add(uint8(0), []byte("changed"), false)
	f.Add(uint8(3), []byte{}, true)
	f.Add(uint8(4), []byte("../outside"), false)
	f.Add(uint8(1), []byte("artifact"), false)
	f.Fuzz(func(t *testing.T, selected uint8, data []byte, missing bool) {
		if len(data) > 16384 {
			return
		}
		dir := nixosImageFixture(t)
		outside := filepath.Join(privateTempDir(t), "preserved")
		writeTestFile(t, outside, []byte("preserved"))
		names := []string{"firecracker", "vmlinux", "initrd", "system.erofs", "boot.json"}
		path := filepath.Join(dir, names[int(selected)%len(names)])
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if missing {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else {
			writeTestFile(t, path, data)
		}
		_, err = verifyNixosImages(dir)
		if (missing || string(original) != string(data)) && err == nil {
			t.Fatal("accepted unavailable or changed launch artifact")
		}
		assertFileContents(t, outside, []byte("preserved"))
		if !missing {
			assertFileContents(t, path, data)
		}
	})
}

func TestNixosRequiredManifestAndBootFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		decode func([]byte) error
	}{
		{"manifest", nixosManifestFixture(), func(data []byte) error { _, err := decodeNixosManifest(data); return err }},
		{"boot", nixosBootFixture(), func(data []byte) error { _, err := decodeNixosBoot(data); return err }},
	} {
		data, _ := json.Marshal(tc.value)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for key, original := range fields {
			t.Run(tc.name+" missing "+key, func(t *testing.T) {
				delete(fields, key)
				data, _ := json.Marshal(fields)
				if tc.decode(data) == nil {
					t.Fatal("accepted missing field")
				}
				fields[key] = original
			})
			t.Run(tc.name+" null "+key, func(t *testing.T) {
				fields[key] = json.RawMessage("null")
				data, _ := json.Marshal(fields)
				if tc.decode(data) == nil {
					t.Fatal("accepted null field")
				}
				fields[key] = original
			})
		}
		for _, bad := range []string{strings.Replace(string(data), `{`, `{"unexpected":1,`, 1), strings.Replace(string(data), `{`, `{"profile":"nixos-v1",`, 1)} {
			if tc.decode([]byte(bad)) == nil {
				t.Fatal("accepted unknown or duplicate field")
			}
		}
	}
}
