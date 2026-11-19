import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("measure", Path(__file__).with_name("measure.py"))
measure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(measure)


class MeasurementCleanup(unittest.TestCase):
    def test_failure_stops_only_current_sample_and_keeps_evidence(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)

            def fail_create(command, **kwargs):
                if "create" in command:
                    state = root / "state" / "sample-0"
                    state.mkdir(parents=True)
                    (state / "state.json").write_text("fixture evidence")
                    raise RuntimeError("fixture failure")
                return 0.0, ""

            with patch.object(measure.tempfile, "mkdtemp", return_value=str(root)), \
                 patch.object(measure, "run", side_effect=fail_create), \
                 patch.object(measure.subprocess, "run") as cleanup:
                cleanup.return_value.returncode = 0
                with self.assertRaises(RuntimeError):
                    measure.measure(Path("/fixture/hk"), Path("/fixture/image"), 1)
                self.assertEqual(1, cleanup.call_count)
                self.assertEqual(["/fixture/hk", "stop", "--force", "sample-0"], cleanup.call_args.args[0])
            self.assertEqual("fixture evidence", (root / "state" / "sample-0" / "state.json").read_text())
