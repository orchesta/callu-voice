"""TTS engine contract.

Swapping Supertonic for another engine (Azure, OpenAI, ElevenLabs, ...) means
implementing this Protocol. Nothing in the service layer changes.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import List, Protocol, runtime_checkable

import numpy as np


@dataclass(frozen=True)
class Audio:
    """Raw synthesis output, before conversion to telephony format."""

    samples: np.ndarray  # mono, float32
    sample_rate: int

    @property
    def duration_s(self) -> float:
        return float(self.samples.size) / self.sample_rate


class UnsupportedLanguage(Exception):
    """The engine cannot synthesize the requested language."""


@runtime_checkable
class TtsEngine(Protocol):
    @property
    def name(self) -> str:
        """Which engine this is. Stable across versions of it."""

    @property
    def build_id(self) -> str:
        """Identity of this engine build and configuration, for the cache key.

        Must change whenever the produced waveform could.
        """

    def supports(self, lang: str) -> bool:
        """Whether the engine has a voice for `lang`."""

    def voices(self) -> List[str]:
        """The voice names this engine offers."""

    def synthesize(self, text: str, lang: str, voice: str) -> Audio:
        """Raises UnsupportedLanguage if the engine has no voice for `lang`."""
