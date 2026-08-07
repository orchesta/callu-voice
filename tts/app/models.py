"""HTTP contract."""
from __future__ import annotations

from typing import Dict, List, Optional

from pydantic import BaseModel, Field

# An alert is a couple of sentences. These caps bound synthesis cost so a caller
# cannot request enormous audio (CPU and disk DoS); they are far above any real
# paging message.
_MAX_SEGMENT_CHARS = 2_000
_MAX_SEGMENTS = 32
_MAX_LEXICON_ENTRIES = 500


class SegmentIn(BaseModel):
    text: str = Field(..., min_length=1, max_length=_MAX_SEGMENT_CHARS)
    lang: str = Field("tr", max_length=16)


class SynthesizeRequest(BaseModel):
    segments: List[SegmentIn] = Field(..., min_length=1, max_length=_MAX_SEGMENTS)
    voice: str = Field("F1", max_length=8)
    # Merged on top of the language's default lexicon; defaults are not lost.
    lexicon: Optional[Dict[str, str]] = Field(default=None, max_length=_MAX_LEXICON_ENTRIES)
    # A render nobody is waiting on a phone for. Answered 503 rather than made to share the
    # synthesizer with a call that is going out.
    discretionary: bool = False


class SegmentOut(BaseModel):
    sound: str
    cached: bool
    normalized: str
    duration_s: float


class SynthesizeResponse(BaseModel):
    results: List[SegmentOut]
    media: List[str]  # ready to pass straight to an ARI play operation


class HealthResponse(BaseModel):
    status: str
    engine: str
    # The build and settings the cache key is derived from, so an operator can see
    # which one produced the audio on disk.
    engine_build: str
    model_loaded: bool
    load_error: Optional[str] = None
    # Set when num2words could not be imported: numbers are then spoken as written,
    # which is a degradation an operator has to be able to see.
    number_words_error: Optional[str] = None
    cached_sounds: int
    # Bytes, not just a file count: the count says nothing about how close the volume
    # is to full, and a full volume fails every page.
    cached_bytes: int
    cache_max_bytes: int
    rule_languages: List[str]
