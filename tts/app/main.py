"""callu-tts — HTTP API.

Routes stay thin: validation and DTO mapping only. Behaviour lives in
SynthesisService. Dependencies are built once during startup and injected.
"""
from __future__ import annotations

import logging
import os
import re
import signal
import threading
import time
from contextlib import asynccontextmanager
from typing import Annotated

from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.responses import FileResponse, JSONResponse

from app.cache import AudioCache
from app.config import Settings, get_settings
from app.engines.base import UnsupportedLanguage
from app.engines.supertonic import SupertonicEngine
from app.models import HealthResponse, SynthesizeRequest, SynthesizeResponse
from app.normalize import number_words_error
from app.service import EngineBusy, Segment, SingleFlightTimeout, SynthesisService

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
log = logging.getLogger("callu-tts")

# Cache keys are hex digests. Anything else is a path traversal attempt.
_KEY_RE = re.compile(r"[0-9a-f]{32}")

# After this many consecutive load failures, stop retrying and terminate so the
# supervisor restarts with a clean slate rather than looping forever in the dark.
_MAX_LOAD_ATTEMPTS = 5


def _warm(engine: SupertonicEngine) -> None:
    """Load the model with backoff; give up and exit if it cannot be loaded.

    A silent forever-retry would leave the container up but unable to synthesize;
    exiting lets the supervisor restart and makes the failure visible.
    """
    for attempt in range(1, _MAX_LOAD_ATTEMPTS + 1):
        try:
            engine.load()
            return
        except Exception:  # noqa: BLE001 - already recorded on the engine
            if attempt == _MAX_LOAD_ATTEMPTS:
                log.error("model load failed %d times; terminating for restart", attempt)
                os.kill(os.getpid(), signal.SIGTERM)
                return
            time.sleep(min(2 * attempt, 15))


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = get_settings()
    engine = SupertonicEngine(steps=settings.synthesis_steps, speed=settings.synthesis_speed)
    cache = AudioCache(
        root=settings.sounds_dir,
        prefix=settings.sound_prefix,
        max_bytes=settings.cache_max_bytes,
    )
    # A .tmp from a process that died mid-write is never published and never read.
    if stale := cache.clear_stale_temp_files():
        log.warning("removed %d stale temp file(s) from the audio cache", stale)

    app.state.settings = settings
    app.state.engine = engine
    app.state.cache = cache
    app.state.service = SynthesisService(
        engine,
        cache,
        sample_rate=settings.sample_rate,
        single_flight_timeout_s=settings.single_flight_timeout_s,
    )

    # Warm the model in the background so the first real call does not pay for it.
    threading.Thread(target=_warm, args=(engine,), daemon=True).start()
    yield


app = FastAPI(title="callu-tts", version="1.0.0", lifespan=lifespan)


def get_service(request: Request) -> SynthesisService:
    return request.app.state.service


ServiceDep = Annotated[SynthesisService, Depends(get_service)]


@app.post("/synthesize", response_model=SynthesizeResponse)
def synthesize(request: SynthesizeRequest, service: ServiceDep) -> SynthesizeResponse:
    engine = app.state.engine
    if not engine.loaded:
        # Still warming, or the model could not load. 503 (retryable), not 500.
        detail = engine.load_error or "model not ready"
        raise HTTPException(status_code=503, detail=detail)
    if request.voice not in engine.voices():
        # Guard the unbounded per-voice style cache and give a clear 400.
        raise HTTPException(status_code=400, detail=f"unknown voice {request.voice!r}")
    try:
        rendered = service.render(
            [Segment(text=s.text, lang=s.lang) for s in request.segments],
            voice=request.voice,
            lexicon=request.lexicon,
            discretionary=request.discretionary,
        )
    except UnsupportedLanguage as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except EngineBusy as exc:
        # 503: the text is fine, the moment is not. Only a discretionary render can get this.
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    except SingleFlightTimeout as exc:
        # 503, not 500: another request is rendering this exact text, so retrying is
        # the right move and the caller should be told so rather than shown a fault.
        raise HTTPException(status_code=503, detail=str(exc)) from exc

    return SynthesizeResponse(
        results=[r.as_dict() for r in rendered],
        media=[f"sound:{r.sound}" for r in rendered],
    )


@app.get("/audio/{key}", response_class=FileResponse)
def audio(key: str, request: Request) -> FileResponse:
    """Return a rendered segment as a WAV file.

    Asterisk reads the shared volume directly and never calls this. It exists so
    a caller can preview a voice, or hear what a segment actually sounded like
    when an announcement went wrong.
    """
    if not _KEY_RE.fullmatch(key):
        raise HTTPException(status_code=400, detail="invalid key")

    path = request.app.state.cache.get(key)
    if path is None:
        raise HTTPException(status_code=404, detail="unknown key")

    return FileResponse(path, media_type="audio/wav", filename=f"{key}.wav")


@app.get("/voices")
def voices(request: Request) -> dict:
    return {"voices": request.app.state.engine.voices()}


@app.get("/health")
def health(request: Request) -> JSONResponse:
    settings: Settings = request.app.state.settings
    engine = request.app.state.engine
    loaded = engine.loaded

    body = HealthResponse(
        status="ok" if loaded else "unavailable",
        engine=engine.name,
        engine_build=engine.build_id,
        model_loaded=loaded,
        load_error=engine.load_error,
        number_words_error=number_words_error(),
        cached_sounds=request.app.state.cache.count(),
        cached_bytes=request.app.state.cache.total_bytes(),
        cache_max_bytes=settings.cache_max_bytes,
        rule_languages=sorted(p.stem for p in settings.rules_dir.glob("*.json")),
    )
    # Not loaded means synthesis would fail, so report unavailable — voice-api's
    # readiness proxies this and stops advertising itself as able to page.
    return JSONResponse(
        content=body.model_dump(),
        status_code=200 if loaded else 503,
    )
