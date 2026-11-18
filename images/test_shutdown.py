import importlib.util
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("image_build", Path(__file__).with_name("build.py"))
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)


class ShutdownImage(unittest.TestCase):
    def test_ctrl_alt_del_runs_orderly_reboot(self):
        files = build.guest_files()
        self.assertIn("::ctrlaltdel:/sbin/reboot\n", files["etc/inittab"])
        self.assertIn("::shutdown:/etc/rcShutdown\n", files["etc/inittab"])
        shutdown = files["etc/rcShutdown"]
        self.assertLess(shutdown.index("kill -TERM"), shutdown.index("kill -KILL"))
        self.assertLess(shutdown.index("kill -KILL"), shutdown.index("sync"))
        self.assertLess(shutdown.index("sync"), shutdown.index("remount,ro"))
        self.assertNotIn("reboot -f", shutdown)
        for name in ("etc/rcS", "etc/rcShutdown", "etc/hk-first-boot"):
            subprocess.run(["sh", "-n"], input=files[name], text=True, check=True)

    def test_runtime_setup_cannot_reinitialize_user_data(self):
        files = build.guest_files()
        runtime = files["etc/rcS"]
        self.assertIn("mount -t tmpfs tmpfs /run", runtime)
        self.assertIn("if [ ! -e /etc/hk-initialized ]; then", runtime)
        self.assertIn("/etc/hk-first-boot", runtime)
        self.assertNotIn("chown -R", runtime)
        self.assertNotIn("cp ", runtime)
        first_boot = files["etc/hk-first-boot"]
        self.assertNotIn("chown -R 1000:1000 /home/hk", first_boot)
        self.assertIn("touch /etc/hk-initialized", first_boot)
        self.assertNotIn("/home/hk/workspace", runtime + first_boot)

    def test_capability_is_versioned(self):
        self.assertEqual(build.SHUTDOWN_CAPABILITY, "ctrl-alt-del-v1")
