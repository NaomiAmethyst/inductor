#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-only
"""One worker, one queue, both kinds of GPU work.

Runs on the machine with the GPU and imports nothing from Inductor -- it is
copied up on its own and has to stand alone.

    jobs/<id>.json   {"id": ..., "kind": "transcribe"|"embed", "audio": "audio/<id>.mp3"}
    out/<id>.json    {"id": ..., "kind": ..., "ok": true, "result": {...}}
    failed/<id>.json {"id": ..., "kind": ..., "ok": false, "error": "..."}

Taking one job at a time is the whole design. Transcription and voice embedding
both want the GPU and neither knows about the other; keeping them in one process
means the serialisation is structural rather than something the caller has to
remember. The queue is how they wait, and it costs the caller nothing to fill
it -- which is what lets a scheduler enqueue everything the moment the audio
exists, as it should.

Models are loaded once, on first use, and kept. Loading Whisper per job costs
more than most jobs.
"""
from __future__ import annotations

import json
import fcntl
import os
import pathlib
import sys
import time
import traceback

HERE = pathlib.Path(__file__).resolve().parent
JOBS, OUT, FAILED = HERE / "jobs", HERE / "out", HERE / "failed"
STOP = HERE / "STOP"
IDLE = float(os.environ.get("QUEUE_IDLE", "2"))
STT_MODEL = os.environ.get("STT_MODEL", "distil-large-v3")

_models: dict = {}


def whisper():
    if "whisper" not in _models:
        from faster_whisper import BatchedInferencePipeline, WhisperModel
        device = os.environ.get("INDUCTOR_DEVICE", "cuda")
        model = WhisperModel(STT_MODEL, device=device, compute_type="int8" if device == "cpu" else "float16")
        _models["whisper"] = (model, BatchedInferencePipeline(model=model))
    return _models["whisper"]


def encoder():
    if "ecapa" not in _models:
        from speechbrain.inference.speaker import EncoderClassifier
        _models["ecapa"] = EncoderClassifier.from_hparams(
            source="speechbrain/spkrec-ecapa-voxceleb",
            savedir=str(HERE / "ecapa"),
            run_opts={"device": os.environ.get("INDUCTOR_DEVICE", "cuda")})
    return _models["ecapa"]


def do_transcribe(spec: dict) -> dict:
    from decode import transcribe
    model, pipe = whisper()
    return transcribe(str(HERE / spec["audio"]), model, pipe,
                      model_name=STT_MODEL,
                      batch_size=int(spec.get("batch_size", 16)),
                      language=spec.get("language") or None)


def do_embed(spec: dict) -> dict:
    # voice_worker is shipped alongside this file and holds the decode and
    # windowing rules -- which slice of a recording is the speaker, and how the
    # short windows that catch a guest are cut.
    sys.path.insert(0, str(HERE))
    import voice_worker as vw

    wav = vw.load(HERE / spec["audio"])
    total = int(wav.shape[1])
    if not total:
        raise RuntimeError("nothing decodable in that file")
    enc = encoder()
    device = os.environ.get("INDUCTOR_DEVICE", "cuda")
    # The speaker comes from a slice a third of the way in: openings are
    # routinely music, a logo sting, or a warning read by somebody else. The
    # short windows are what catch a guest, who owns the ninety seconds they
    # are in and moves the average of a forty-minute recording not at all.
    start = total // 3
    voice = vw.embed(enc, [wav[:, start:start + vw.RATE * vw.VOICE_SECONDS]], device)
    step = vw.RATE * vw.WINDOW_SECONDS
    cuts = [wav[:, i:i + step] for i in range(0, total, step)][:vw.MAX_WINDOWS]
    windows = vw.embed(enc, cuts, device)
    return {"seconds": round(total / vw.RATE, 1),
            "voice": voice[0] if voice else None,
            "windows": windows, "window_seconds": vw.WINDOW_SECONDS}


HANDLERS = {"transcribe": do_transcribe, "embed": do_embed}


def settle(spec: dict, *, ok: bool, result=None, error: str = "") -> None:
    """Every answer goes to `out`, including "it did not work".

    A failure filed somewhere the collector does not look is a job that never
    comes back: the caller polls for a result that exists, in a directory it
    never reads, until its timeout. Six workers did exactly that here and the
    whole run stood still with the GPU idle. `failed/` is kept as a copy to read
    afterwards, but it is not part of the protocol.
    """
    payload = {"id": spec.get("id"), "kind": spec.get("kind"), "ok": ok}
    if ok:
        payload["result"] = result
    else:
        payload["error"] = error[:2000]
    body = json.dumps(payload)
    for where in ((OUT,) if ok else (OUT, FAILED)):
        where.mkdir(parents=True, exist_ok=True)
        # Written aside and moved, so the collector never sees half a file.
        tmp = where / f".{spec.get('id')}.json"
        tmp.write_text(body, encoding="utf-8")
        tmp.rename(where / f"{spec.get('id')}.json")


def main() -> int:
    with (HERE / "worker.lock").open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return 0
        return drain()


def drain() -> int:
    (HERE / "worker.pid").write_text(str(os.getpid()), encoding="ascii")
    for folder in (JOBS, OUT, FAILED):
        folder.mkdir(parents=True, exist_ok=True)
    print(f"queue worker up; model {STT_MODEL}", flush=True)
    idle_since = time.time()
    while True:
        jobs = sorted(p for p in JOBS.glob("*.json") if not p.name.startswith("."))
        if not jobs:
            if STOP.exists():
                print("STOP seen; queue drained", flush=True)
                return 0
            if time.time() - idle_since > 3600:
                print("idle for an hour; stopping", flush=True)
                return 0
            time.sleep(IDLE)
            continue
        idle_since = time.time()
        path = jobs[0]
        try:
            spec = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            path.unlink(missing_ok=True)
            continue
        handler = HANDLERS.get(str(spec.get("kind")))
        started = time.time()
        try:
            if handler is None:
                raise ValueError(f"unknown kind {spec.get('kind')!r}")
            result = handler(spec)
            settle(spec, ok=True, result=result)
            print(f"  {spec.get('kind')} {spec.get('id')} "
                  f"in {time.time() - started:.0f}s", flush=True)
        except Exception:
            settle(spec, ok=False, error=traceback.format_exc())
            print(f"  FAILED {spec.get('kind')} {spec.get('id')}", flush=True)
        finally:
            path.unlink(missing_ok=True)
            audio = HERE / str(spec.get("audio") or "")
            if audio.is_file() and audio.parent.name == "audio":
                audio.unlink(missing_ok=True)
    print("STOP seen; exiting", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
