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

decode_spec = importlib.util.spec_from_file_location("decode", HERE / "decode.py")
decode = importlib.util.module_from_spec(decode_spec)
decode_spec.loader.exec_module(decode)


class Segments(list):
    """Whatever a stub model was told to return, in the shape faster-whisper gives."""

    @staticmethod
    def of(*texts):
        return [type("S", (), {"start": i * 2.0, "end": i * 2.0 + 1.0, "text": t})()
                for i, t in enumerate(texts)]


class ResidueTests(unittest.TestCase):
    def test_filler_is_not_counted_as_words(self):
        # The decoder's stock phrases, and the sounds a real recording is made
        # of. Counting the second lot as invention would call a wordless
        # recording broken; counting the first as words is how a tone track
        # ends up with four hundred of them.
        residue = ["Thank you.", "You know,", "Thanks for watching", "Bye bye.",
                   "I don't know.", "Subscribe", "www.example.com"]
        real = ["Oh.", "Mm-hmm.", "Ah...", "Good girl.", "Close your eyes."]
        for text in residue:
            self.assertTrue(decode.RESIDUE.match(text), text)
        for text in real:
            self.assertFalse(decode.RESIDUE.match(text), text)
        words, kept = decode.substantive(
            [{"text": t} for t in residue + ["Drift down for me."]])
        self.assertEqual(words, 4)
        self.assertEqual([k["text"] for k in kept], ["Drift down for me."])

    def test_an_invented_retry_never_displaces_an_honest_silence(self):
        """The gate is right about a recording with no words in it.

        Every retry that follows returns more text than the gate did, so a
        keep-test on raw word count hands the transcript to whichever pass
        hallucinated hardest. What is compared is substantive words, and when
        none of the passes finds any, the answer is that there is no speech.
        """
        calls = []

        class Stub:
            def __init__(self, name):
                self.name = name

            def transcribe(self, *a, **kw):
                calls.append((self.name, kw.get("vad_filter")))
                info = type("I", (), {"duration": 600.0})()
                if kw.get("vad_filter"):
                    return Segments.of(), info
                return Segments.of(*(["Thank you."] * 40)), info

        def load(name):
            stub = Stub(name)
            return stub, stub

        with patch.object(decode, "container_duration", lambda p: 600.0), \
             patch.object(decode, "resilient_decode",
                          lambda p, rate=16000: (type("A", (), {"size": 0})(), 0)):
            out = decode.transcribe("nowhere.mp3", load, model_name="small",
                                    fallback=("large",), batch_size=8, language="en")
        self.assertEqual(out["speech"], "none")
        self.assertEqual(out["text"], "")
        self.assertEqual(out["segments"], [])
        self.assertEqual(out["substantive_words"], 0)
        # It really did try the stronger model before concluding that.
        self.assertIn(("large", True), calls)

    def test_a_stronger_model_is_kept_when_it_finds_real_words(self):
        class Stub:
            def __init__(self, name):
                self.name = name

            def transcribe(self, *a, **kw):
                info = type("I", (), {"duration": 600.0})()
                if self.name == "large" and kw.get("vad_filter"):
                    return Segments.of(*(["Close your eyes and sink."] * 60)), info
                return Segments.of("Thank you."), info

        def load(name):
            stub = Stub(name)
            return stub, stub

        with patch.object(decode, "container_duration", lambda p: 600.0), \
             patch.object(decode, "resilient_decode",
                          lambda p, rate=16000: (type("A", (), {"size": 0})(), 0)):
            out = decode.transcribe("nowhere.mp3", load, model_name="small",
                                    fallback=("large",), batch_size=8, language="en")
        self.assertEqual(out["model"], "large")
        self.assertEqual(out["speech"], "present")
        self.assertIn("Close your eyes", out["text"])


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


class StagingTests(unittest.TestCase):
    def test_only_the_staged_copy_is_ever_deleted(self):
        """A shared archive is not this worker's to tidy up.

        When both machines mount the same filesystem the job names the original
        rather than a copy, and the old test -- is the parent directory called
        "audio"? -- says yes for any archive laid out that way. It would have
        deleted the recording.
        """
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            with patch.object(worker, "HERE", root):
                (root / "audio").mkdir()
                staged = root / "audio" / "a.mp3"
                staged.write_text("copy")
                shared = root / "archive" / "audio" / "real.mp3"
                shared.parent.mkdir(parents=True)
                shared.write_text("the recording")
                worker.discard("audio/a.mp3")
                self.assertFalse(staged.exists())
                worker.discard(str(shared))
                self.assertTrue(shared.exists())
                worker.discard("../escape.mp3")
                worker.discard(None)
