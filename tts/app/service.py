"""Synthesis orchestration: normalize -> cache -> engine -> store.

The engine and cache are injected; this class does not know which engine it is
driving.
"""
from __future__ import annotations

import logging
import threading
import time
from contextlib import contextmanager
from dataclasses import asdict, dataclass
from typing import Dict, List, Optional, Sequence

from app.audio import to_telephony_wav
from app.cache import AudioCache
from app.engines.base import TtsEngine
from app.normalize import normalize

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Segment:
    text: str
    lang: str


@dataclass(frozen=True)
class RenderedSegment:
    sound: str
    cached: bool
    normalized: str  # what was actually spoken, for diagnosis without listening
    duration_s: float

    def as_dict(self) -> dict:
        return asdict(self)


class SingleFlightTimeout(Exception):
    """Another request is still rendering this text and we ran out of patience."""


class EngineBusy(Exception):
    """A call is being rendered, and this render was not worth making it wait."""


class SynthesisService:
    def __init__(
        self,
        engine: TtsEngine,
        cache: AudioCache,
        sample_rate: int = 8000,
        single_flight_timeout_s: float = 55.0,
    ) -> None:
        self._engine = engine
        self._cache = cache
        self._sample_rate = sample_rate
        self._single_flight_timeout_s = single_flight_timeout_s

        # One render per key, however many callers ask for it.
        self._inflight: Dict[str, threading.Event] = {}
        self._inflight_lock = threading.Lock()

        # The engine serializes synthesis, so a discretionary render delays a page by however long
        # it takes. Counted rather than read off the engine's lock: the service does not know which
        # engine it is driving.
        self._calls_rendering = 0
        self._calls_lock = threading.Lock()

    def render(
        self,
        segments: Sequence[Segment],
        voice: str,
        lexicon: Optional[Dict[str, str]] = None,
        discretionary: bool = False,
    ) -> List[RenderedSegment]:
        return [self._render_one(segment, voice, lexicon, discretionary) for segment in segments]

    @contextmanager
    def _counted(self, discretionary: bool):
        """Marks a call render as in flight, so a preview knows to stand aside."""
        if discretionary:
            yield
            return
        with self._calls_lock:
            self._calls_rendering += 1
        try:
            yield
        finally:
            with self._calls_lock:
                self._calls_rendering -= 1

    def _render_one(
        self,
        segment: Segment,
        voice: str,
        lexicon: Optional[Dict[str, str]],
        discretionary: bool = False,
    ) -> RenderedSegment:
        text = normalize(segment.text, segment.lang, lexicon)
        if not text:
            # Do not echo the incident text back to the caller or into logs.
            raise ValueError("a segment is empty after normalization")

        key = self._cache.key(
            self._engine.build_id, voice, segment.lang, text, self._sample_rate
        )

        hit = self._cache.get(key)
        if hit is not None:
            return self._from_cache(key, text, hit)

        # Only text nobody has rendered before reaches the engine, so this is the whole cost a
        # preview can impose on a page that is going out right now.
        if discretionary and self._calls_rendering > 0:
            raise EngineBusy("a call is being rendered; try again shortly")

        with self._inflight_lock:
            existing = self._inflight.get(key)
            if existing is None:
                leader = threading.Event()
                self._inflight[key] = leader
            else:
                leader = None

        if leader is None:
            if not existing.wait(self._single_flight_timeout_s):
                raise SingleFlightTimeout(
                    "another request is still rendering this text; retry shortly"
                )
            hit = self._cache.get(key)
            if hit is not None:
                return self._from_cache(key, text, hit)

            # The leader failed. Render here, and touch no bookkeeping: the map entry
            # now belongs to whoever became leader after us.
            log.warning("single-flight leader produced nothing; rendering locally")
            with self._counted(discretionary):
                return self._render_now(segment, text, voice, key)

        try:
            with self._counted(discretionary):
                return self._render_now(segment, text, voice, key)
        finally:
            # Released on failure too, or every later caller for this key waits out the
            # full timeout for a render that already gave up.
            with self._inflight_lock:
                self._inflight.pop(key, None)
            leader.set()

    def _render_now(self, segment: Segment, text: str, voice: str, key: str) -> RenderedSegment:
        started = time.time()
        audio = self._engine.synthesize(text, segment.lang, voice)
        self._cache.store(key, to_telephony_wav(audio, self._sample_rate))

        log.info(
            "synthesized engine=%s voice=%s lang=%s audio=%.2fs took=%.2fs",
            self._engine.name, voice, segment.lang, audio.duration_s, time.time() - started,
        )

        return RenderedSegment(
            sound=self._cache.sound_id(key),
            cached=False,
            normalized=text,
            duration_s=audio.duration_s,
        )

    def _from_cache(self, key: str, text: str, path) -> RenderedSegment:
        return RenderedSegment(
            sound=self._cache.sound_id(key),
            cached=True,
            normalized=text,
            duration_s=AudioCache.duration_s(path),
        )
