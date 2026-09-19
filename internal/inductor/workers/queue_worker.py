#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-only
"""One worker, one queue, both kinds of GPU work.

Runs on the machine with the GPU and imports nothing from Inductor -- it is
copied up on its own and has to stand alone.

    jobs/<id>.json   {"id": ..., "kind": "transcribe"|"embed"|"sound", "audio": "audio/<id>.mp3"}
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


def whisper(name=None):
    """One loaded model per name, kept.

    Keyed by name rather than held as a single model because a thin transcript
    is offered to a stronger one, and a run that does that for a handful of
    recordings should not reload either model for each of them.
    """
    name = name or STT_MODEL
    key = "whisper:" + name
    if key not in _models:
        from faster_whisper import BatchedInferencePipeline, WhisperModel
        device = os.environ.get("INDUCTOR_DEVICE", "cuda")
        model = WhisperModel(name, device=device, compute_type="int8" if device == "cpu" else "float16")
        _models[key] = (model, BatchedInferencePipeline(model=model))
    return _models[key]


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
    return transcribe(str(HERE / spec["audio"]), whisper,
                      model_name=spec.get("model") or STT_MODEL,
                      fallback=tuple(spec.get("fallback") or ()),
                      batch_size=int(spec.get("batch_size", 16)),
                      language=spec.get("language") or None)


def do_sound(spec: dict) -> dict:
    sys.path.insert(0, str(HERE))
    import sound_worker as sw

    return sw.analyse(HERE / spec["audio"],
                      tagger_model=spec.get("tagger") or "",
                      zeroshot_model=spec.get("zeroshot") or "",
                      labels=spec.get("labels") or (),
                      voice=spec.get("voice") or (),
                      threshold=float(spec.get("threshold", 0.25)),
                      floor=float(spec.get("floor", 0.35)))


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


HANDLERS = {"transcribe": do_transcribe, "embed": do_embed, "sound": do_sound}


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


def discard(where) -> None:
    """Delete the staged copy, and only ever the staged copy.

    A job may name a file this box already had -- the archive can live on a
    filesystem both machines mount, in which case nothing was copied here and
    the path points at the original. Checking that the parent directory happens
    to be called "audio" is not enough to tell those apart: plenty of archives
    have a directory called audio, and deleting what is in it would take the
    recording itself. Resolve, and require the result to be inside this
    worker's own staging directory.
    """
    if not where:
        return
    staged = (HERE / "audio").resolve()
    try:
        audio = (HERE / str(where)).resolve()
        audio.relative_to(staged)
    except (ValueError, OSError):
        return
    if audio.is_file():
        audio.unlink(missing_ok=True)


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
            discard(spec.get("audio"))
    print("STOP seen; exiting", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
