# SPDX-License-Identifier: GPL-3.0-only
"""Robust audio decoding and transcription, shared by queue jobs."""

import re

def container_duration(path):
    """The real length of the file, off the container.

    info.duration is post-VAD here, so it collapses along with the transcript when
    the gate misfires -- a file cut to 1% of its length then looks perfectly dense
    and the fallback below never fires. Ask the container instead.
    """
    try:
        import av
        # AV_TIME_BASE is 1e6 by definition; av.time_base was removed in PyAV 18,
        # and reading it through getattr would have made this return 0.0 in silence
        # -- which is the very failure it exists to prevent.
        with av.open(str(path)) as c:
            if c.duration:
                return float(c.duration) / 1_000_000
            st = next((s for s in c.streams if s.type == "audio"), None)
            if st is not None and st.duration and st.time_base:
                return float(st.duration * st.time_base)
    except Exception:
        pass
    return 0.0

def resilient_decode(path, rate=16000):
    """Decode the whole file, stepping over corrupt packets.

    A single bad frame partway through an mp3 raises InvalidDataError out of
    avcodec_send_packet, and faster-whisper's own decode_audio gives up there and
    returns what it had. 112 files in this archive did that -- Tangled Brain
    decoded 30s of its 45 minutes off one bad packet. Demuxing by hand and
    skipping the packet that will not decode recovers the rest.
    """
    import av
    import numpy as np
    chunks, bad = [], 0
    try:
        with av.open(str(path)) as c:
            if not c.streams.audio:
                return np.zeros(0, dtype="float32"), 0
            st = c.streams.audio[0]
            st.thread_type = "AUTO"
            # A damaged stream throws out the odd frame at a different sample rate
            # or layout, and a resampler is bound to the format it first saw -- so
            # one resampler raises on that frame and takes the whole decode with
            # it. Blackmail.mp3 has 71 such frames among 95,000 and stopped at the
            # first, 71 seconds into 42 minutes. Keep one resampler per format.
            resamplers = {}
            packets = c.demux(st)
            while True:
                try:
                    packet = next(packets)
                except StopIteration:
                    break
                except Exception:
                    bad += 1
                    continue
                try:
                    frames = packet.decode()
                except Exception:
                    bad += 1
                    continue
                for frame in frames:
                    key = (frame.format.name, frame.layout.name, frame.sample_rate)
                    resampler = resamplers.get(key)
                    if resampler is None:
                        resampler = resamplers[key] = av.AudioResampler(
                            format="s16", layout="mono", rate=rate)
                    try:
                        for out in resampler.resample(frame):
                            chunks.append(out.to_ndarray().reshape(-1))
                    except Exception:
                        bad += 1
    except Exception:
        pass
    if not chunks:
        return np.zeros(0, dtype="float32"), bad
    return np.concatenate(chunks).astype("float32") / 32768.0, bad

# What a speech model says when handed audio with no speech in it.
#
# These are not mishearings. They are the stock phrases of the training data --
# caption boilerplate and conversational filler -- emitted because the decoder
# must emit something, and they arrive looking exactly like a transcript. A
# larger model produces fewer of them and still produces them, so the answer is
# not a better model but refusing to count them as words.
#
# Interjections are deliberately absent. "Oh", "Mm" and "Ah" are real sounds
# that real recordings are made of, and counting them here would mistake a
# wordless recording for a broken one.
RESIDUE = re.compile(r"""^\W*(?:
 thank\s+you(\s+(?:so|very)\s+much)?|thank\s+you\s+for\s+(?:watching|listening|your\s+attention)|
 thanks(?:\s+for\s+(?:watching|listening))?|you\s+know|
 i\s+don'?t\s+know(?:\s+what\s+(?:to\s+do|you'?re\s+talking\s+about))?|
 i'?m\s+going\s+to(?:\s+be\s+able\s+to)?|we'?re\s+going\s+to(?:\s+be(?:\s+able\s+to\s+be)?)?|
 i'?ll\s+be\s+right\s+back|see\s+you\s+(?:next\s+time|in\s+the\s+next\s+video)|
 (?:please\s+)?subscribe(?:\s+to\s+my\s+channel)?|like\s+and\s+subscribe|
 bye(?:\s*bye)?|goodbye|the\s+end|for\s+more\s+information.*|www\..*|
 \u00a1?gracias.*|\u3053\u3093\u306b\u3061\u306f.*|\u0e02\u0e2d\u0e1a\u0e04\u0e38\u0e13.*
 )\W*$""", re.I | re.X)


def substantive(segments):
    """Words that are not the decoder talking to itself.

    This is the number every keep-or-discard decision below is made on. Raw word
    count cannot make them: on audio with nothing to transcribe, the gate is
    right to return nothing and any retry that invents filler beats it, every
    time, for ever.
    """
    kept = [s for s in segments if s["text"] and not RESIDUE.match(s["text"])]
    return sum(len(s["text"].split()) for s in kept), kept


def _run(model, pipe, source, *, vad, batch_size, language):
    if vad:
        segments, info = pipe.transcribe(
            source, batch_size=batch_size, language=language, vad_filter=True,
            vad_parameters={"min_silence_duration_ms": 700},
            condition_on_previous_text=False)
    else:
        segments, info = model.transcribe(
            source, language=language, vad_filter=False,
            condition_on_previous_text=False)
    segs = [{"start": round(s.start, 2), "end": round(s.end, 2), "text": s.text.strip()}
            for s in segments]
    return segs, info


def transcribe(path, load, *, model_name, fallback=(), batch_size, language):
    """Transcribe, and be willing to say there was nothing to transcribe.

    Three things can be true of a recording that comes back nearly empty: the
    delivery is quiet and slow and the voice gate swallowed it, the model is not
    good enough, or there are no words in it. The first is worth opening the
    gate for, the second is worth a stronger model, and the third is worth
    recording as a fact rather than papering over. They are told apart by
    whether anything *substantive* appears, never by whether more text does.
    """
    real = container_duration(path)
    audio, bad = resilient_decode(path)
    source = audio if audio.size else str(path)
    decoded = audio.size / 16000 if audio.size else 0.0

    model, pipe = load(model_name)
    segs, info = _run(model, pipe, source, vad=True, batch_size=batch_size, language=language)
    span = max((real, decoded, info.duration or 0))
    best = {"segs": segs, "model": model_name, "gate": "vad"}
    best["words"], _ = substantive(segs)

    # Quiet, slow delivery can be swallowed whole by the gate; a transcript that
    # simply stops early is the other shape of the same failure. 112 files were
    # cut to their first minutes because only the first test ran, and against a
    # duration the gate had already shrunk.
    def wanting(state):
        covered = state["segs"][-1]["end"] if state["segs"] else 0.0
        return (state["words"] / max(span / 60, 1) < 25) or (span > 120 and covered < span * 0.75)

    attempts = [(model_name, False)]
    for name in fallback:
        attempts += [(name, True), (name, False)]
    for name, vad in attempts:
        if not wanting(best):
            break
        try:
            m, p = load(name)
        except Exception:
            continue
        try:
            segs, _ = _run(m, p, source, vad=vad, batch_size=batch_size, language=language)
        except Exception:
            continue
        words, _ = substantive(segs)
        # Strictly more, so an alternative that merely reshuffles the same words
        # does not displace a result that has already been accepted.
        if words > best["words"]:
            best = {"segs": segs, "words": words, "model": name,
                    "gate": "vad" if vad else "open"}

    segs = best["segs"]
    words, kept = substantive(segs)
    covered = segs[-1]["end"] if segs else 0.0
    # What the transcript is, said plainly, so that nothing downstream has to
    # infer it from a word count and a duration and reach its own conclusion.
    if words == 0:
        speech = "none"
    elif words / max(span / 60, 1) < 25:
        speech = "sparse"
    else:
        speech = "present"
    if speech == "none":
        # Keeping the invention would hand every later stage a recording that
        # says "thank you" four hundred times, which is how a tone track ends up
        # described as a whispered mantra.
        segs, covered = [], 0.0
    name = best["model"]
    if best["gate"] == "open":
        name += " (no VAD)"
    text = "\n".join(s["text"] for s in segs if s["text"])
    return {"apiVersion": "hypnotica/v1", "kind": "Transcript",
            "model": name, "text": text, "segments": segs, "speech": speech,
            "substantive_words": words,
            "duration": max(span, covered), "covered": round(covered, 2),
            "decoded": round(decoded, 2), "bad_packets": bad}
