"""SynthesisService tests.

These run against a fake engine. That is the point: if the service can be
exercised without Supertonic, the TtsEngine seam is real and swapping engines
is genuinely a matter of writing one class.
"""
from __future__ import annotations

import os
import threading
import time
from pathlib import Path
from typing import List

import numpy as np
import pytest

from app.cache import AudioCache
from app.engines.base import Audio, TtsEngine, UnsupportedLanguage
from app.service import EngineBusy, Segment, SingleFlightTimeout, SynthesisService


class FakeEngine(TtsEngine):
    """Emits one second of silence and records what it was asked to say."""

    def __init__(self, supported: List[str] | None = None, build: str = "1") -> None:
        self.calls: List[tuple[str, str, str]] = []
        self._supported = supported or ["tr", "en"]
        self._build = build

    @property
    def name(self) -> str:
        return "fake"

    @property
    def build_id(self) -> str:
        return f"fake@{self._build}"

    def supports(self, lang: str) -> bool:
        return lang.split("-")[0].lower() in self._supported

    def voices(self) -> List[str]:
        return ["F1"]

    def synthesize(self, text: str, lang: str, voice: str) -> Audio:
        if not self.supports(lang):
            raise UnsupportedLanguage(lang)
        self.calls.append((text, lang, voice))
        return Audio(samples=np.zeros(8000, dtype=np.float32), sample_rate=8000)


@pytest.fixture
def service(tmp_path: Path) -> SynthesisService:
    cache = AudioCache(root=tmp_path, prefix="callu")
    return SynthesisService(FakeEngine(), cache)


def test_protocol_is_satisfied():
    assert isinstance(FakeEngine(), TtsEngine)


def test_renders_a_segment(service: SynthesisService):
    [result] = service.render([Segment("30 saniye", "tr")], voice="F1")

    assert result.sound.startswith("callu/")
    assert result.cached is False
    assert result.normalized == "otuz saniye"


def test_engine_receives_normalized_text(tmp_path: Path):
    engine = FakeEngine()
    service = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"))

    service.render([Segment("HTTP 503", "tr")], voice="F1")

    spoken, _, _ = engine.calls[0]
    assert spoken == "HTTP beş yüz üç"


def test_second_render_hits_the_cache(tmp_path: Path):
    engine = FakeEngine()
    service = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"))
    segments = [Segment("30 saniye", "tr")]

    first = service.render(segments, voice="F1")
    second = service.render(segments, voice="F1")

    assert first[0].cached is False
    assert second[0].cached is True
    assert len(engine.calls) == 1, "cached segment must not reach the engine"


def test_changing_voice_misses_the_cache(tmp_path: Path):
    engine = FakeEngine()
    service = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"))
    segments = [Segment("30 saniye", "tr")]

    service.render(segments, voice="F1")
    result = service.render(segments, voice="M1")

    assert result[0].cached is False
    assert len(engine.calls) == 2


def test_lexicon_override_changes_the_cache_key(tmp_path: Path):
    engine = FakeEngine()
    service = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"))
    segments = [Segment("PostgreSQL", "tr")]

    default = service.render(segments, voice="F1")
    overridden = service.render(segments, voice="F1", lexicon={"postgresql": "postgres"})

    assert overridden[0].cached is False, "a changed lexicon must re-synthesize"
    assert default[0].sound != overridden[0].sound


def test_unsupported_language_is_reported(tmp_path: Path):
    engine = FakeEngine(supported=["en"])
    service = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"))

    with pytest.raises(UnsupportedLanguage):
        service.render([Segment("merhaba", "tr")], voice="F1")


def test_empty_segment_after_normalization_is_rejected(service: SynthesisService):
    with pytest.raises(ValueError):
        service.render([Segment("   ", "tr")], voice="F1")


def test_audio_written_to_cache_is_playable_wav(tmp_path: Path):
    cache = AudioCache(root=tmp_path, prefix="callu")
    service = SynthesisService(FakeEngine(), cache)

    [result] = service.render([Segment("30 saniye", "tr")], voice="F1")

    key = result.sound.split("/", 1)[1]
    written = cache.get(key)
    assert written is not None
    assert written.read_bytes()[:4] == b"RIFF"


# ── one render per key, however many callers ask for it ───────────────────────────


class BlockingEngine(FakeEngine):
    """Holds the first synthesize until released, so a fan-out can be observed."""

    def __init__(self) -> None:
        super().__init__()
        self.entered = threading.Event()
        self.release = threading.Event()

    def synthesize(self, text: str, lang: str, voice: str) -> Audio:
        self.entered.set()
        self.release.wait(5)
        return super().synthesize(text, lang, voice)


def test_concurrent_callers_render_the_same_text_once(tmp_path: Path):
    """An escalation fan-out asks for the same announcement for several responders at once."""
    engine = BlockingEngine()
    svc = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"))

    segment = Segment("payment-api coktu", "tr")
    results: List[object] = []
    errors: List[Exception] = []

    def call() -> None:
        try:
            results.append(svc.render([segment], voice="F1")[0])
        except Exception as exc:  # noqa: BLE001 - recorded for the assertion
            errors.append(exc)

    threads = [threading.Thread(target=call) for _ in range(4)]
    threads[0].start()
    assert engine.entered.wait(5), "the leader never reached the engine"
    for t in threads[1:]:
        t.start()

    # Give the followers time to queue on the leader rather than start their own render.
    time.sleep(0.2)
    engine.release.set()
    for t in threads:
        t.join(10)

    assert not errors, errors
    assert len(results) == 4
    assert len(engine.calls) == 1, f"synthesized {len(engine.calls)} times, want 1"


def test_a_follower_gives_up_rather_than_hanging_forever(tmp_path: Path):
    """The wait is bounded: a 503 the caller can retry beats a socket that times out."""
    engine = BlockingEngine()
    svc = SynthesisService(
        engine, AudioCache(root=tmp_path, prefix="callu"), single_flight_timeout_s=0.1
    )
    segment = Segment("payment-api coktu", "tr")

    leader = threading.Thread(target=lambda: svc.render([segment], voice="F1"))
    leader.start()
    assert engine.entered.wait(5)

    with pytest.raises(SingleFlightTimeout):
        svc.render([segment], voice="F1")

    engine.release.set()
    leader.join(10)


def test_a_failed_leader_does_not_park_the_followers(tmp_path: Path):
    """Releasing the slot in a finally is what keeps a failed render from blocking the next."""

    class FailingOnce(FakeEngine):
        def __init__(self) -> None:
            super().__init__()
            self.attempts = 0

        def synthesize(self, text: str, lang: str, voice: str) -> Audio:
            self.attempts += 1
            if self.attempts == 1:
                raise RuntimeError("engine blew up")
            return super().synthesize(text, lang, voice)

    engine = FailingOnce()
    svc = SynthesisService(engine, AudioCache(root=tmp_path, prefix="callu"), single_flight_timeout_s=0.1)
    segment = Segment("payment-api coktu", "tr")

    with pytest.raises(RuntimeError):
        svc.render([segment], voice="F1")

    # The second caller renders instead of waiting out the timeout for a dead leader.
    [result] = svc.render([segment], voice="F1")
    assert result.cached is False


# ── the cache is bounded ──────────────────────────────────────────────────────────


def test_the_cache_evicts_least_recently_used_when_over_its_bound(tmp_path: Path):
    """Every incident's own title is a new key kept forever, so the directory needs a bound."""
    engine = FakeEngine()
    # One second of 8 kHz 16-bit audio is ~16 kB, so this holds a couple of entries.
    cache = AudioCache(root=tmp_path, prefix="callu", max_bytes=40_000)
    svc = SynthesisService(engine, cache)

    for i in range(6):
        svc.render([Segment(f"incident number {i}", "en")], voice="F1")

    assert cache.total_bytes() <= 40_000, cache.total_bytes()
    assert cache.count() >= 1, "the sweep must not empty the directory"


def test_an_unbounded_cache_is_left_alone(tmp_path: Path):
    """0 disables the sweep: an operator who mounted a big volume gets what they asked for."""
    cache = AudioCache(root=tmp_path, prefix="callu", max_bytes=0)
    svc = SynthesisService(FakeEngine(), cache)

    for i in range(6):
        svc.render([Segment(f"incident number {i}", "en")], voice="F1")

    assert cache.count() == 6


# ── the key carries the engine build and the settings that shape the waveform ─────


def test_an_engine_upgrade_invalidates_cached_audio(tmp_path: Path):
    cache = AudioCache(root=tmp_path, prefix="callu")
    segments = [Segment("30 saniye", "tr")]

    before = SynthesisService(FakeEngine(build="1"), cache).render(segments, voice="F1")
    after = SynthesisService(FakeEngine(build="2"), cache).render(segments, voice="F1")

    assert before[0].sound != after[0].sound
    assert after[0].cached is False


@pytest.mark.parametrize("first,second", [(8000, 16000), (16000, 8000)])
def test_a_sample_rate_change_invalidates_cached_audio(tmp_path: Path, first: int, second: int):
    cache = AudioCache(root=tmp_path, prefix="callu")
    segments = [Segment("30 saniye", "tr")]

    before = SynthesisService(FakeEngine(), cache, sample_rate=first).render(segments, voice="F1")
    after = SynthesisService(FakeEngine(), cache, sample_rate=second).render(segments, voice="F1")

    assert before[0].sound != after[0].sound
    assert after[0].cached is False


def test_the_supertonic_build_id_covers_the_waveform_settings():
    from app.engines.supertonic import SupertonicEngine

    base = SupertonicEngine(steps=8, speed=1.0).build_id

    assert base != SupertonicEngine(steps=16, speed=1.0).build_id
    assert base != SupertonicEngine(steps=8, speed=1.2).build_id
    assert base == SupertonicEngine(steps=8, speed=1.0).build_id


@pytest.mark.parametrize("sample_rate", [8000, 16000, 44100])
def test_duration_is_read_from_the_header_not_assumed(tmp_path: Path, sample_rate: int):
    """duration_s follows the configured sample rate rather than assuming 8 kHz."""
    cache = AudioCache(root=tmp_path, prefix="callu")
    engine = FakeEngine()  # emits exactly one second of audio
    svc = SynthesisService(engine, cache, sample_rate=sample_rate)

    [fresh] = svc.render([Segment("30 saniye", "tr")], voice="F1")
    [hit] = svc.render([Segment("30 saniye", "tr")], voice="F1")

    assert hit.cached is True
    assert hit.duration_s == pytest.approx(1.0, abs=0.01)
    assert hit.duration_s == pytest.approx(fresh.duration_s, abs=0.01)


def test_stale_temp_files_are_removed_but_fresh_ones_are_kept(tmp_path: Path):
    cache = AudioCache(root=tmp_path, prefix="callu")
    stale = tmp_path / ".abc.123.tmp"
    fresh = tmp_path / ".def.456.tmp"
    stale.write_bytes(b"x")
    fresh.write_bytes(b"x")
    os.utime(stale, (time.time() - 3600, time.time() - 3600))

    assert cache.clear_stale_temp_files() == 1
    assert not stale.exists()
    assert fresh.exists()


class TestDiscretionaryRenders:
    """A preview is optional; a call going out is not.

    Synthesis is serialized behind one lock, so a render nobody is waiting on a phone for can push a
    live page past the timeout its caller allows. A preview is turned away instead.
    """

    def test_a_preview_is_turned_away_while_a_call_renders(self, tmp_path: Path):
        cache = AudioCache(root=tmp_path, prefix="callu")
        started = threading.Event()
        release = threading.Event()

        class BlockingEngine(FakeEngine):
            def synthesize(self, text: str, lang: str, voice: str) -> Audio:
                started.set()
                release.wait(5)
                return super().synthesize(text, lang, voice)

        svc = SynthesisService(BlockingEngine(), cache)

        call = threading.Thread(
            target=lambda: svc.render([Segment("bir çağrı", "tr")], voice="F1"))
        call.start()
        assert started.wait(5)

        try:
            with pytest.raises(EngineBusy):
                svc.render([Segment("bir önizleme", "tr")], voice="F1", discretionary=True)
        finally:
            release.set()
            call.join(5)

    def test_a_call_is_never_turned_away(self, tmp_path: Path):
        """Two calls at once still both render; only a preview stands aside."""
        cache = AudioCache(root=tmp_path, prefix="callu")
        started = threading.Event()
        release = threading.Event()

        class BlockingEngine(FakeEngine):
            def synthesize(self, text: str, lang: str, voice: str) -> Audio:
                started.set()
                release.wait(5)
                return super().synthesize(text, lang, voice)

        svc = SynthesisService(BlockingEngine(), cache)

        first = threading.Thread(target=lambda: svc.render([Segment("ilk", "tr")], voice="F1"))
        first.start()
        assert started.wait(5)
        release.set()

        [rendered] = svc.render([Segment("ikinci", "tr")], voice="F1")
        first.join(5)

        assert rendered.sound.startswith("callu/")

    def test_already_rendered_text_costs_the_engine_nothing(self, tmp_path: Path):
        """A cache hit never reaches the engine, so it cannot be in anybody's way."""
        cache = AudioCache(root=tmp_path, prefix="callu")
        engine = FakeEngine()
        svc = SynthesisService(engine, cache)

        svc.render([Segment("aynı metin", "tr")], voice="F1")

        with svc._counted(discretionary=False):  # a call is rendering
            [hit] = svc.render([Segment("aynı metin", "tr")], voice="F1", discretionary=True)

        assert hit.cached is True
        assert len(engine.calls) == 1

    def test_a_failed_call_render_does_not_leave_previews_refused(self, tmp_path: Path):
        """The count is released on the way out, error or not, or every later preview is a 503."""
        cache = AudioCache(root=tmp_path, prefix="callu")

        class FailingEngine(FakeEngine):
            def synthesize(self, text: str, lang: str, voice: str) -> Audio:
                raise RuntimeError("model gone")

        svc = SynthesisService(FailingEngine(), cache)

        with pytest.raises(RuntimeError):
            svc.render([Segment("bir çağrı", "tr")], voice="F1")

        assert svc._calls_rendering == 0
