"""Supertonic 3 engine."""
from __future__ import annotations

import logging
import threading
import time
from functools import lru_cache
from typing import Dict, List, Optional

import numpy as np

from app.engines.base import Audio, TtsEngine, UnsupportedLanguage
from app.lang import primary

log = logging.getLogger(__name__)

SAMPLE_RATE = 44_100
VOICES = ["F1", "F2", "F3", "F4", "F5", "M1", "M2", "M3", "M4", "M5"]

# Attributes the library might expose for the weights it downloaded. Weights are
# fetched with auto_download and are not pinned, so this is best-effort.
_REVISION_ATTRS = ("MODEL_REVISION", "MODEL_VERSION", "REVISION", "__version__")


@lru_cache(maxsize=1)
def _package_build() -> str:
    """The installed supertonic build, as far as it can be established."""
    parts: List[str] = []
    try:
        from importlib.metadata import version

        parts.append(version("supertonic"))
    except Exception:  # noqa: BLE001 - any failure means "cannot establish"
        pass
    try:
        from supertonic import config as supertonic_config

        for attr in _REVISION_ATTRS:
            revision = getattr(supertonic_config, attr, None)
            if revision:
                parts.append(str(revision))
                break
    except Exception:  # noqa: BLE001 - the library need not expose a revision
        pass

    if not parts:
        log.warning(
            "cannot establish the supertonic build; an upgrade will not invalidate cached audio"
        )
        return "unknown"
    return "+".join(parts)


class SupertonicEngine(TtsEngine):
    def __init__(self, steps: int = 8, speed: float = 1.0) -> None:
        self._steps = steps
        self._speed = speed
        self._tts = None
        self._styles: Dict[str, object] = {}
        self._languages: Optional[frozenset] = None
        # The last load failure, surfaced through /health.
        self._load_error: Optional[str] = None
        # The ONNX session is not thread-safe; synthesis is serialized.
        self._lock = threading.Lock()

    @property
    def name(self) -> str:
        return "supertonic3"

    @property
    def build_id(self) -> str:
        return f"{self.name}@{_package_build()}/steps={self._steps}/speed={self._speed}"

    @property
    def loaded(self) -> bool:
        return self._tts is not None

    @property
    def load_error(self) -> Optional[str]:
        return self._load_error

    def load(self) -> None:
        """Load the model. Skipping this delays the first synthesis by ~25s.

        A failure is recorded (for /health) and re-raised, so a warmer can retry
        and a request gets a clear error instead of a torn-down process.
        """
        if self._tts is not None:
            return
        with self._lock:
            if self._tts is not None:
                return
            try:
                from supertonic import TTS
                from supertonic.config import SUPPORTED_LANGUAGES

                started = time.time()
                log.info("loading model")
                tts = TTS(auto_download=True)
                self._languages = frozenset(SUPPORTED_LANGUAGES)
                self._tts = tts
                self._load_error = None
                log.info("model ready in %.1fs", time.time() - started)
            except Exception as exc:  # noqa: BLE001 - report any load failure
                self._load_error = f"{type(exc).__name__}: {exc}"
                log.error("model load failed: %s", self._load_error)
                raise

    def supports(self, lang: str) -> bool:
        self.load()
        return primary(lang) in (self._languages or frozenset())

    def voices(self) -> List[str]:
        return list(VOICES)

    def synthesize(self, text: str, lang: str, voice: str) -> Audio:
        self.load()
        if not self.supports(lang):
            raise UnsupportedLanguage(f"{self.name} does not support language {lang!r}")

        with self._lock:
            if voice not in self._styles:
                self._styles[voice] = self._tts.get_voice_style(voice_name=voice)
            samples, _ = self._tts.synthesize(
                text=text,
                lang=primary(lang),
                voice_style=self._styles[voice],
                total_steps=self._steps,
                speed=self._speed,
            )

        return Audio(
            samples=np.asarray(samples, dtype=np.float32).reshape(-1),
            sample_rate=SAMPLE_RATE,
        )
