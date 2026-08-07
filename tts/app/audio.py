"""Audio format conversion: engine output -> telephony WAV.

Written in-process rather than shelling out to ffmpeg. ffmpeg would drag a few
hundred megabytes of codecs into the image to do one resample, and piping a WAV
out of it silently produces placeholder RIFF sizes that Asterisk refuses to play.
"""
from __future__ import annotations

import io
import wave

import numpy as np
import soxr

from app.engines.base import Audio

_SAMPLE_WIDTH = 2  # 16-bit
_INT16_MAX = 32767


def to_telephony_wav(audio: Audio, sample_rate: int = 8000) -> bytes:
    """Convert to mono 16-bit PCM WAV bytes."""
    samples = np.asarray(audio.samples, dtype=np.float32).reshape(-1)

    if audio.sample_rate != sample_rate:
        # Downsampling without a low-pass filter aliases; soxr is a polyphase
        # resampler and does it properly.
        samples = soxr.resample(samples, audio.sample_rate, sample_rate, quality="HQ")

    pcm = (np.clip(samples, -1.0, 1.0) * _INT16_MAX).astype("<i2")

    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as out:
        out.setnchannels(1)
        out.setsampwidth(_SAMPLE_WIDTH)
        out.setframerate(sample_rate)
        out.writeframes(pcm.tobytes())
    return buffer.getvalue()
