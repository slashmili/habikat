#!/usr/bin/env python3
"""Build the small x86_64 guest without mount, chroot, or root privileges."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def download_artifact(artifact, cache):
    """Verify both downloads and cache hits; publish only verified artifacts."""
    path = cache / artifact["file"]
    if not path.exists():
        with tempfile.TemporaryDirectory(prefix=".hk-download-", dir=cache) as download:
            target = Path(download) / path.name
            subprocess.run(["curl", "--disable", "--fail", "--location", "--show-error", "--silent",
                            "--proto", "=https", "--proto-redir", "=https",
                            "--connect-timeout", "20", "--max-time", "300", "--retry", "2",
                            artifact["url"], "-o", str(target)], check=True)
            if digest(target) != artifact["sha256"]:
                raise RuntimeError(f"download SHA-256 mismatch: {path.name}")
            target.rename(path)
    if digest(path) != artifact["sha256"]:
        raise RuntimeError(f"cached SHA-256 mismatch: {path.name}")
    return path


def build():
    recipe = Path(__file__).resolve().parent
    cache_home = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache"))
    output = Path(sys.argv[1]) if len(sys.argv) > 1 else cache_home / "habikat/image"
    cache = Path(sys.argv[2]) if len(sys.argv) > 2 else cache_home / "habikat/downloads"
    output, cache = output.absolute(), cache.absolute()
    if len(sys.argv) > 3 or output.exists():
        sys.exit("usage: images/build.py [NEW_OUTPUT_DIR [DOWNLOAD_CACHE]] (output must not exist)")
    if os.uname().machine != "x86_64":
        sys.exit("this image supports Linux x86_64 only")
    os.umask(0o077)
    for tool in ("curl", "tar", "mkfs.ext4"):
        if not shutil.which(tool):
            sys.exit(f"missing required build tool: {tool}")
    cache.mkdir(parents=True, exist_ok=True)
    artifacts = json.loads((recipe / "artifacts.lock.json").read_text())
    for artifact in artifacts:
        download_artifact(artifact, cache)

    output.parent.mkdir(parents=True, exist_ok=True)
    # Publish only a fully built image; leave no partially usable output.
    with tempfile.TemporaryDirectory(prefix=".hk-build-", dir=output.parent) as work:
        work = Path(work)
        root = work / "root"
        root.mkdir()
        for artifact in artifacts:
            if artifact["file"].endswith(".apk"):
                # APK v2 uses concatenated tar streams. GNU tar reads all of
                # them with --ignore-zeros; package install scripts are not run.
                subprocess.run(["tar", "--extract", "--gzip", "--ignore-zeros", "--no-same-owner", "--same-permissions",
                                "--warning=no-unknown-keyword", "--file", str(cache / artifact["file"]),
                                "--directory", str(root)], check=True)
        for path in root.glob(".*"):
            if path.is_file():
                path.unlink()  # APK signatures/metadata; input lock retains provenance.
        for directory in ("dev/pts", "proc", "sys", "run", "tmp", "root", "home/hk", "etc/ssh",
                          "var/empty", "var/log", "sbin", "bin"):
            (root / directory).mkdir(parents=True, exist_ok=True)
        for applet in ("sh", "mount", "mkdir", "chmod", "chown", "cp", "cat", "hostname", "id",
                       "ls", "pwd", "stty", "sleep", "touch", "rm", "uname", "ps", "grep", "env",
                       "wc", "head", "tail", "find", "sync", "false", "true", "kill", "echo", "mv"):
            dest = root / "bin" / applet
            if not dest.exists():
                dest.symlink_to("busybox")
        (root / "sbin/init").symlink_to("../bin/busybox")
        files = {
            "etc/passwd": "root:x:0:0:root:/root:/bin/sh\nhk:x:1000:1000:Habikat:/home/hk:/bin/sh\nsshd:x:22:22:sshd:/var/empty:/bin/false\n",
            "etc/group": "root:x:0:\nhk:x:1000:\nsshd:x:22:\n",
            "etc/shadow": "root:!:20000:0:99999:7:::\nhk:*:20000:0:99999:7:::\nsshd:!:20000:0:99999:7:::\n",
            "etc/shells": "/bin/sh\n",
            "etc/profile": "export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\nexport PS1='\\u@\\h:\\w\\$ '\n",
            "etc/ssh/sshd_config": """HostKey /etc/ssh/host_key
AuthorizedKeysFile .ssh/authorized_keys
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
AuthenticationMethods publickey
AllowUsers hk
DisableForwarding yes
PermitUserEnvironment no
PrintMotd no
UseDNS no
PidFile /run/sshd.pid
""",
            "etc/inittab": "::sysinit:/etc/rcS\n::respawn:/usr/bin/socat VSOCK-LISTEN:22,reuseaddr,fork EXEC:'/usr/sbin/sshd -i -e'\n",
            "etc/rcS": """#!/bin/sh
set -eu
mount -t proc proc /proc
mount -t sysfs sysfs /sys
# The pinned kernel mounts devtmpfs before init.
mkdir -p /dev/pts /run/provision/seed /run/sshd /home/hk/.ssh
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /tmp
chmod 1777 /tmp
# mkfs -d is unprivileged on the host; establish guest ownership before SSH.
chown -R 0:0 /bin /sbin /lib /usr /etc /var /root /run
chown 0:0 / /home
chmod 755 / /home
chmod 700 /root
chmod 700 /run/provision
mount -o ro /dev/vdb /run/provision/seed
cp /run/provision/seed/authorized_keys /home/hk/.ssh/authorized_keys
cp /run/provision/seed/host_key /etc/ssh/host_key
hostname "$(cat /run/provision/seed/hostname)"
chown -R 1000:1000 /home/hk
chmod 700 /home/hk/.ssh
chmod 600 /home/hk/.ssh/authorized_keys /etc/ssh/host_key
""",
        }
        for name, data in files.items():
            (root / name).write_text(data)
            (root / name).chmod(0o755 if name == "etc/rcS" else 0o644)
        (root / "etc/shadow").chmod(0o600)
        # umask must not leave packaged parent directories inaccessible to hk.
        for directory, dirs, _ in os.walk(root):
            Path(directory).chmod(0o755)
        built = work / "image"
        built.mkdir()
        subprocess.run(["mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-d", str(root),
                        str(built / "rootfs.ext4"), "131072"], check=True)
        with tarfile.open(cache / artifacts[0]["file"]) as archive:
            member = archive.getmember("release-v1.18.0-x86_64/firecracker-v1.18.0-x86_64")
            with archive.extractfile(member) as source, (built / "firecracker").open("wb") as target:
                shutil.copyfileobj(source, target)
        (built / "firecracker").chmod(0o755)
        shutil.copyfile(cache / "vmlinux", built / "vmlinux")
        manifest = {name: digest(built / name) for name in ("firecracker", "vmlinux", "rootfs.ext4")}
        (built / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        shutil.copyfile(recipe / "artifacts.lock.json", built / "artifacts.lock.json")
        built.rename(output)
    print(f"Built verified image: {output}")
    print(f"export HK_IMAGE_DIR='{output}'")


if __name__ == "__main__":
    build()
