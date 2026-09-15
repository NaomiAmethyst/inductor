# SPDX-License-Identifier: GPL-3.0-only
"""Robust audio decoding and transcription, shared by queue jobs."""

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

def transcribe(path, model, pipe, *, model_name, batch_size, language):
    real = container_duration(path)
    audio, bad = resilient_decode(path)
    source = audio if audio.size else str(path)
    decoded = audio.size / 16000 if audio.size else 0.0
    segments, info = pipe.transcribe(
        source, batch_size=batch_size, language=language, vad_filter=True,
        vad_parameters={"min_silence_duration_ms": 700},
        condition_on_previous_text=False)
    segs = [{"start": round(s.start, 2), "end": round(s.end, 2), "text": s.text.strip()}
            for s in segments]
    text = "\n".join(s["text"] for s in segs if s["text"])
    name = model_name
    span = max((real, decoded, info.duration or 0))
    covered = segs[-1]["end"] if segs else 0.0
    # Quiet, slow delivery can be swallowed whole by the VAD gate; retry open.
    # Two ways that shows up: too few words for the running time, or a transcript
    # that simply stops early -- 112 files were cut to their first minutes because
    # only the first test ran, and against a duration the gate had already shrunk.
    sparse = len(text.split()) / max(span / 60, 1) < 25
    short = span > 120 and covered < span * 0.75
    if sparse or short:
        segments, info = model.transcribe(
            source, language=language, vad_filter=False, condition_on_previous_text=False)
        alt = [{"start": round(s.start, 2), "end": round(s.end, 2), "text": s.text.strip()}
               for s in segments]
        alt_text = "\n".join(s["text"] for s in alt if s["text"])
        if len(alt_text.split()) > len(text.split()):
            segs, text, name = alt, alt_text, model_name + " (no VAD)"
            covered = segs[-1]["end"] if segs else 0.0
    return {"apiVersion": "hypnotica/v1", "kind": "Transcript",
            "model": name, "text": text, "segments": segs,
            "duration": max(span, covered), "covered": round(covered, 2),
            "decoded": round(decoded, 2), "bad_packets": bad}
