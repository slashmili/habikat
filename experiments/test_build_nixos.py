import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("build_nixos", Path(__file__).with_name("build_nixos.py"))
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)


class NixosExport(unittest.TestCase):
    def test_outputs_cannot_enter_flake_input(self):
        with tempfile.TemporaryDirectory() as root:
            source = Path(root) / "source"
            source.mkdir()
            for output in [source, source / "image", source / "a" / ".." / "image"]:
                with self.subTest(output=output), self.assertRaises(RuntimeError):
                    builder.outside_source(output, source)
            alias = Path(root) / "alias"
            alias.symlink_to(source)
            with self.assertRaises(RuntimeError):
                builder.outside_source(alias / "output", source)

    def test_export_is_immutable_and_repeatable(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            built = root / "built"
            built.mkdir()
            for name in ["vmlinux", "initrd", "system.erofs", "boot.json", "closure-paths"]:
                (built / name).write_bytes(name.encode())
            binary = root / "firecracker"
            binary.write_bytes(b"verified fixture binary")
            output = root / "export"
            builder.export_image(built, binary, output)
            before = {p.name: p.read_bytes() for p in output.iterdir()}
            builder.export_image(built, binary, output)
            self.assertEqual(before, {p.name: p.read_bytes() for p in output.iterdir()})
            self.assertEqual(0o555, (output / "firecracker").stat().st_mode & 0o777)
            self.assertEqual(0o444, (output / "system.erofs").stat().st_mode & 0o777)
            (built / "initrd").write_bytes(b"different")
            with self.assertRaises(RuntimeError):
                builder.export_image(built, binary, output)
            self.assertEqual(before, {p.name: p.read_bytes() for p in output.iterdir()})
