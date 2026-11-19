#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time


SOURCE = Path(__file__).resolve().parent / "nixos"


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def outside_source(path, source):
    path = path.resolve()
    if path == source or source in path.parents:
        raise RuntimeError("build output must be outside the flake input directory")
    return path


def verified_firecracker(baseline):
    manifest = json.loads((baseline / "manifest.json").read_text())
    binary = baseline / "firecracker"
    if digest(binary) != manifest["firecracker"]:
        raise RuntimeError("baseline Firecracker digest mismatch")
    version = subprocess.check_output([str(binary), "--version"], text=True)
    if "Firecracker v1.18.0" not in version:
        raise RuntimeError("this experiment requires the verified Firecracker 1.18.0 binary")
    return binary


def export_image(built, binary, destination):
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".nixos-export-", dir=destination.parent) as temp:
        staging = Path(temp)
        for name in ["vmlinux", "initrd", "system.erofs", "boot.json", "closure-paths"]:
            shutil.copyfile(built / name, staging / name)
            (staging / name).chmod(0o444)
        shutil.copyfile(binary, staging / "firecracker")
        (staging / "firecracker").chmod(0o555)
        names = {"kernel": "vmlinux", "initrd": "initrd", "system": "system.erofs",
                 "boot": "boot.json", "firecracker": "firecracker"}
        manifest = {"version": 1, "profile": "nixos-v1"}
        manifest.update({key: digest(staging / name) for key, name in names.items()})
        (staging / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        (staging / "manifest.json").chmod(0o444)
        if destination.exists():
            for path in staging.iterdir():
                if digest(path) != digest(destination / path.name):
                    raise RuntimeError("output exists with different artifacts; choose a fresh HK_NIXOS_IMAGE_DIR")
        else:
            staging.rename(destination)


def build():
    if shutil.which("nix") is None:
        raise RuntimeError("Nix with flakes support is required; install/configure it separately")
    source = SOURCE.resolve()
    cache = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "habikat"
    destination = outside_source(Path(os.environ.get("HK_NIXOS_IMAGE_DIR", cache / "nixos-image")), source)
    baseline = Path(os.environ.get("HK_BASELINE_IMAGE_DIR", cache / "image")).resolve()
    binary = verified_firecracker(baseline)
    started = time.monotonic()
    command = ["nix", "--extra-experimental-features", "nix-command flakes", "build",
               "--no-update-lock-file", "--no-link", "--print-out-paths", f"path:{source}"]
    built = Path(subprocess.check_output(command, text=True).strip())
    elapsed = time.monotonic() - started
    export_image(built, binary, destination)
    print(json.dumps({"image": str(destination), "realization_seconds": elapsed,
                      "store_export": str(built), "artifacts": {
                          p.name: {"apparent_bytes": p.stat().st_size,
                                   "allocated_bytes": p.stat().st_blocks * 512}
                          for p in destination.iterdir()}}, indent=2))


if __name__ == "__main__":
    try:
        build()
    except (OSError, RuntimeError, subprocess.CalledProcessError, KeyError, ValueError) as error:
        sys.exit(f"NixOS experiment build failed: {error}")
