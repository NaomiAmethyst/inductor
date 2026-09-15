# SPDX-License-Identifier: GPL-3.0-only
"""Model-free queue and decoding tests. Run with python -m unittest discover."""
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch
import wave

HERE = pathlib.Path(__file__).parent
spec = importlib.util.spec_from_file_location("queue_worker", HERE / "queue_worker.py")
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)

class QueueTests(unittest.TestCase):
    def test_success_failure_and_unknown_jobs_settle_before_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            for name in ("jobs", "out", "failed", "audio"):
                (root / name).mkdir()
            jobs = [dict(id="a", kind="fixture", audio="audio/a.wav"),
                    dict(id="b", kind="fixture", audio="audio/b.wav"),
                    dict(id="c", kind="unknown", audio="audio/c.wav")]
            for job in jobs:
                (root / "jobs" / (job["id"] + ".json")).write_text(json.dumps(job))
                (root / job["audio"]).write_bytes(b"audio")
            # STOP drains queued work; it must not abandon accepted jobs.
            (root / "STOP").touch()
            def handler(job):
                self.assertTrue((root / "jobs" / (job["id"] + ".json")).exists())
                if job["id"] == "b":
                    raise ValueError("bad recording")
                return {"text": "ready"}
            with patch.multiple(worker, HERE=root, JOBS=root/"jobs", OUT=root/"out",
                                FAILED=root/"failed", STOP=root/"STOP",
                                HANDLERS={"fixture": handler}):
                self.assertEqual(worker.main(), 0)
            self.assertEqual(list((root / "jobs").glob("*.json")), [])
            self.assertTrue(json.loads((root / "out/a.json").read_text())["ok"])
            for name in ("b", "c"):
                result = json.loads((root / "out" / (name + ".json")).read_text())
                self.assertFalse(result["ok"])
                self.assertTrue((root / "failed" / (name + ".json")).exists())
            self.assertFalse((root / "audio/a.wav").exists())

    def test_resilient_decoder_and_duration(self):
        try:
            import av
            import numpy
        except ImportError:
            self.skipTest("install av and numpy for decoder test")
        spec = importlib.util.spec_from_file_location("decode", HERE / "decode.py")
        decode = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(decode)
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "test.wav"
            with wave.open(str(path), "wb") as output:
                output.setnchannels(1)
                output.setsampwidth(2)
                output.setframerate(16000)
                output.writeframes(b"\x00\x01" * 32000)
            samples, bad = decode.resilient_decode(path)
            self.assertGreater(len(samples), 31000)
            self.assertEqual(bad, 0)
            self.assertAlmostEqual(decode.container_duration(path), 2.0, places=2)
            samples, _ = decode.resilient_decode(path.parent / "missing.wav")
            self.assertEqual(len(samples), 0)

if __name__ == "__main__":
    unittest.main()
