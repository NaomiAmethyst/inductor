#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-only
"""Speaker embeddings, on whatever box has the GPU.

Reads audio from `audio/`, writes one JSON per file to `out/`, and leaves the
model resident between files -- loading ECAPA costs more than embedding a
recording does.

Two vectors come back for each file. The **voice** is one embedding of a long
slice, which is what identifies the speaker. The **windows** are embeddings of
short consecutive slices, which is what a second speaker shows up in: a cameo
does not move the average of a forty-minute recording, but it owns the ninety
seconds it is in.

Shipped by Inductor and run there; it imports nothing from the package.
"""
from __future__ import annotations

import json
import pathlib
import sys
import warnings

warnings.filterwarnings("ignore")

import av
import numpy as np
import torch
from speechbrain.inference.speaker import EncoderClassifier

HERE = pathlib.Path(__file__).resolve().parent
RATE = 16000
# Long enough to be the speaker rather than the sentence; short enough that a
# forty-minute file is not decoded in full for a single vector.
VOICE_SECONDS = 180
WINDOW_SECONDS = 20
MAX_WINDOWS = 90


def load(path: pathlib.Path) -> torch.Tensor:
    """Decode anything, through the same library Inductor decodes with.

    Not `torchaudio.load`: it falls back to libsndfile, which cannot read AAC,
    and a large part of a library like this is `.m4a` -- every track demuxed out
    of a video, and whole creators' collections besides. They came back as
    "Format not recognised" and an empty embedding, which looks like a quiet
    recording rather than a failure.
    """
    with av.open(str(path)) as container:
        stream = container.streams.audio[0]
        stream.thread_type = "NONE"
        resampler = av.AudioResampler(format="flt", layout="mono", rate=RATE)
        chunks = []
        for frame in container.decode(stream):
            for out in resampler.resample(frame):
                chunks.append(out.to_ndarray().reshape(-1))
    if not chunks:
        raise ValueError("no audio decoded")
    samples = np.concatenate(chunks).astype("float32")
    return torch.from_numpy(samples).unsqueeze(0)


def embed(enc, chunks: list[torch.Tensor], device: str) -> list[list[float]]:
    out = []
    for chunk in chunks:
        if chunk.shape[1] < RATE:
            continue
        with torch.no_grad():
            v = enc.encode_batch(chunk.to(device)).squeeze()
        out.append(torch.nn.functional.normalize(v, dim=0).cpu().tolist())
    return out


def main() -> int:
    device = "cuda" if torch.cuda.is_available() else "cpu"
    enc = EncoderClassifier.from_hparams(
        source="speechbrain/spkrec-ecapa-voxceleb",
        savedir=str(HERE / "model"),
        run_opts={"device": device})
    audio, out = HERE / "audio", HERE / "out"
    out.mkdir(parents=True, exist_ok=True)
    done = 0
    for path in sorted(audio.iterdir()):
        if not path.is_file() or path.suffix.lower() == ".json":
            continue
        target = out / f"{path.stem}.json"
        if target.exists():
            continue
        try:
            wav = load(path)
        except Exception as exc:
            target.write_text(json.dumps({"error": f"{type(exc).__name__}: {exc}"}))
            continue
        total = wav.shape[1]
        # The speaker, from a slice a third of the way in: openings are
        # routinely music, a logo sting, or a warning read by somebody else.
        start = total // 3
        voice = embed(enc, [wav[:, start:start + RATE * VOICE_SECONDS]], device)
        step = RATE * WINDOW_SECONDS
        cuts = [wav[:, i:i + step] for i in range(0, total, step)][:MAX_WINDOWS]
        windows = embed(enc, cuts, device)
        target.write_text(json.dumps({
            "apiVersion": "inductor/v1", "kind": "VoicePrint",
            "seconds": round(total / RATE, 1),
            "window_seconds": WINDOW_SECONDS,
            "voice": voice[0] if voice else None,
            "windows": windows,
        }))
        done += 1
        print(f"  embedded {path.name} ({len(windows)} windows)", flush=True)
    print(f"done: {done} file(s)", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
