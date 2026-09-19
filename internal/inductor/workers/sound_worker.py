#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-only
"""Identify audio by what it sounds like rather than by what it says.

Two models, kept apart on purpose because they fail in opposite directions.

The ontology tagger is multi-label: every class gets its own probability, so
"somebody is speaking" is an absolute reading that does not compete with
"there are mouth sounds". That is what makes it usable as a gate.

The zero-shot model scores audio against whatever phrases it is handed, so it
describes far better and can be given a vocabulary that suits the collection --
but it cannot decline. Handed a list with no word for what it is hearing it
returns the nearest thing on the list, confidently: offered no tone label, it
called sixty silent tone tracks "whispering close to the microphone", and
offered a list of household noises it called sixty-five of them "a car engine".
What tells those apart from a real answer is the *absolute* similarity, not the
margin over the runner-up -- the wrong-vocabulary run had the larger margins of
the two. Its answers are descriptions, and only above the floor.

Shipped alongside queue_worker.py and imports nothing from Inductor.
"""
from __future__ import annotations

import os
import pathlib

import numpy as np

HERE = pathlib.Path(__file__).resolve().parent
DEVICE = os.environ.get("INDUCTOR_DEVICE", "cuda")

# What each model wants. The tagger was trained on 10.24s at 16 kHz and the
# zero-shot model on 48 kHz; feeding either the other's rate quietly costs
# accuracy rather than raising anything.
TAG_RATE, ZS_RATE = 16000, 48000
WINDOW = 20.0
OFFSETS = (0.2, 0.5, 0.8)

_models: dict = {}


def load(path, rate, start, seconds):
    """One window, mono, at the rate asked for.

    Seeking rather than decoding the whole file is what keeps a four-hour
    recording from being read into memory to answer a question three twenty
    second windows can settle.
    """
    import av

    chunks = []
    with av.open(str(path)) as c:
        if not c.streams.audio:
            return np.zeros(0, dtype="float32")
        st = c.streams.audio[0]
        st.thread_type = "AUTO"
        if start > 0 and st.time_base:
            try:
                c.seek(int(start / float(st.time_base)), stream=st)
            except Exception:
                pass
        resampler = av.AudioResampler(format="s16", layout="mono", rate=rate)
        want = int(seconds * rate)
        got = 0
        for frame in c.decode(st):
            if frame.time is not None and frame.time < start - 1:
                continue
            try:
                for out in resampler.resample(frame):
                    a = out.to_ndarray().reshape(-1)
                    chunks.append(a)
                    got += len(a)
            except Exception:
                continue
            if got >= want:
                break
    if not chunks:
        return np.zeros(0, dtype="float32")
    return (np.concatenate(chunks)[: int(seconds * rate)]).astype("float32") / 32768.0


def windows(path, rate, seconds):
    """Three windows through the recording, or the whole of a short one."""
    import av

    total = 0.0
    try:
        with av.open(str(path)) as c:
            if c.duration:
                total = float(c.duration) / 1_000_000
    except Exception:
        pass
    if total <= WINDOW * 1.5:
        w = load(path, rate, 0, max(total, WINDOW))
        return [w] if w.size else []
    out = []
    for frac in OFFSETS:
        w = load(path, rate, max(0.0, total * frac - WINDOW / 2), WINDOW)
        if w.size > rate:
            out.append(w)
    return out


def tagger(name):
    if "tagger" not in _models:
        import torch
        from transformers import AutoProcessor, ASTForAudioClassification

        proc = AutoProcessor.from_pretrained(name)
        model = ASTForAudioClassification.from_pretrained(name).to(DEVICE).eval()
        _models["tagger"] = (proc, model, model.config.id2label, torch)
    return _models["tagger"]


def zeroshot(name):
    if "zeroshot" not in _models:
        import torch
        from transformers import ClapModel, ClapProcessor

        proc = ClapProcessor.from_pretrained(name)
        model = ClapModel.from_pretrained(name).to(DEVICE).eval()
        _models["zeroshot"] = (proc, model, torch)
    return _models["zeroshot"]


def _features(out, torch):
    """transformers 5 returns an output object here, earlier versions a tensor."""
    t = out if torch.is_tensor(out) else (
        out.pooler_output if getattr(out, "pooler_output", None) is not None
        else out.last_hidden_state.mean(1))
    return t / t.norm(dim=-1, keepdim=True)


def run_tagger(name, path, limit=12):
    proc, model, id2label, torch = tagger(name)
    scores = []
    for w in windows(path, TAG_RATE, WINDOW):
        # The tagger's own window is 10.24s; averaging its verdicts over the
        # pieces beats handing it one long stretch it will crop anyway.
        step = int(10.24 * TAG_RATE)
        pieces = [w[i:i + step] for i in range(0, len(w), step)]
        for piece in pieces:
            if len(piece) < TAG_RATE:
                continue
            inputs = proc(piece, sampling_rate=TAG_RATE, return_tensors="pt").to(DEVICE)
            with torch.no_grad():
                scores.append(torch.sigmoid(model(**inputs).logits)[0].float().cpu().numpy())
    if not scores:
        return []
    mean = np.mean(scores, axis=0)
    order = np.argsort(mean)[::-1][:limit]
    return [{"label": id2label[int(i)], "score": round(float(mean[i]), 3)} for i in order]


def run_zeroshot(name, path, labels, floor, limit=6):
    if not labels:
        return [], 0.0, False
    proc, model, torch = zeroshot(name)
    with torch.no_grad():
        text = proc(text=list(labels), return_tensors="pt", padding=True).to(DEVICE)
        temb = _features(model.get_text_features(**text), torch)
    sims = []
    for w in windows(path, ZS_RATE, WINDOW):
        audio = proc(audio=[w], sampling_rate=ZS_RATE, return_tensors="pt").to(DEVICE)
        with torch.no_grad():
            aemb = _features(model.get_audio_features(**audio), torch)
        sims.append((aemb @ temb.T)[0].float().cpu().numpy())
    if not sims:
        return [], 0.0, False
    mean = np.mean(sims, axis=0)
    order = np.argsort(mean)[::-1]
    out = [{"label": labels[int(i)], "score": round(float(mean[i]), 4)} for i in order[:limit]]
    top = float(mean[order[0]])
    margin = float(mean[order[0]] - mean[order[1]]) if len(order) > 1 else 0.0
    # Kept for the record, not for the decision: measured over this collection,
    # every margin threshold admitted more nonsense than it kept real answers,
    # while a floor on the similarity itself admitted none.
    return out, round(margin, 4), top >= floor


def analyse(path, *, tagger_model, zeroshot_model, labels, voice, threshold, floor):
    result = {}
    if tagger_model:
        result["tags"] = run_tagger(tagger_model, path)
    if zeroshot_model:
        sounds, margin, fits = run_zeroshot(zeroshot_model, path, list(labels or ()), floor)
        result["sounds"] = sounds
        result["margin"] = margin
        # Whether the vocabulary had a word for this at all. False means the
        # labels below are the nearest miss, not an identification.
        result["fits"] = fits
        result["floor"] = floor
    best = 0.0
    for row in result.get("tags", ()):
        if row["label"] in (voice or ()):
            best = max(best, row["score"])
    result["voice_confidence"] = round(best, 3)
    result["speech"] = "present" if best >= threshold else "none"
    result["tagger_model"] = tagger_model or None
    result["zeroshot_model"] = zeroshot_model or None
    return result
