#!/usr/bin/env python3
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time


def run(command, *, directory, environment, input=None):
    started = time.monotonic()
    output = subprocess.check_output(command, cwd=directory, env=environment,
                                     input=input, text=True, stderr=subprocess.STDOUT,
                                     timeout=120)
    return time.monotonic() - started, output


def memory_fields(text):
    return {line.split(":", 1)[0]: line.split(":", 1)[1].strip()
            for line in text.splitlines() if ":" in line}


def measure(cli, image, samples):
    root = Path(tempfile.mkdtemp(prefix="hk-measure-", dir="/var/tmp"))
    environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    environment.update(HK_STATE_DIR=str(root / "state"), HK_IMAGE_DIR=str(image),
                       GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1")
    source = root / "project"
    source.mkdir()
    execute = lambda command, **kwargs: run(command, directory=source, environment=environment, **kwargs)
    execute(["git", "init", "--template=", "-b", "main"])
    (source / "source.txt").write_text("measurement fixture\n")
    execute(["git", "add", "source.txt"])
    execute(["git", "-c", "user.name=Measurement Fixture", "-c", "user.email=measure@example.invalid",
             "commit", "-m", "fixture"])
    records = []
    name = None
    try:
        for sample in range(samples):
            name = f"sample-{sample}"
            directory = root / "state" / name
            record = {"sample": sample + 1}
            record["create_seconds"], _ = execute([str(cli), "create", "--name", name])
            state = json.loads((directory / "state.json").read_text())
            record["vmm_rss"] = memory_fields(Path(f"/proc/{state['pid']}/status").read_text())["VmRSS"]
            record["resources"] = json.loads((directory / "config.json").read_text())["machine-config"]
            _, output = execute([str(cli), "attach", name], input="cat /proc/meminfo\n")
            memory = memory_fields(output)
            record["guest_memory"] = {key: memory[key] for key in ["MemTotal", "MemAvailable", "MemFree"]}
            _, record["guest_versions"] = execute([str(cli), "attach", name], input="uname -r\ngit --version\ncommand -v psql >/dev/null && psql --version || true\n")
            record["stop_seconds"], _ = execute([str(cli), "stop", name])
            execute(["e2fsck", "-fn", str(directory / "rootfs.ext4")])
            disk = (directory / "rootfs.ext4").stat()
            record["private_disk"] = {"apparent_bytes": disk.st_size, "allocated_bytes": disk.st_blocks * 512}
            record["restart_seconds"], _ = execute([str(cli), "start", name])
            record["second_stop_seconds"], _ = execute([str(cli), "stop", name])
            execute(["e2fsck", "-fn", str(directory / "rootfs.ext4")])
            execute([str(cli), "delete", "--force", name])
            records.append(record)
    except BaseException:
        if name is not None and (root / "state" / name / "state.json").exists():
            stop_failed_sample(cli, name, source, environment)
        print(f"Measurement failure; test evidence retained at {root}", file=sys.stderr)
        raise
    shutil.rmtree(root)
    return {"cli": str(cli), "image": str(image), "samples": records}


def stop_failed_sample(cli, name, source, environment):
    try:
        result = subprocess.run([str(cli), "stop", "--force", name], cwd=source,
                                env=environment, text=True, capture_output=True,
                                timeout=30)
        if result.returncode:
            print(f"Cleanup could not stop {name}: {result.stdout}{result.stderr}", file=sys.stderr)
    except (OSError, subprocess.TimeoutExpired) as error:
        print(f"Cleanup could not stop {name}: {error}", file=sys.stderr)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli", type=Path, required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--samples", type=int, default=3, choices=range(1, 11))
    args = parser.parse_args()
    print(json.dumps(measure(args.cli.resolve(), args.image.resolve(), args.samples), indent=2))
