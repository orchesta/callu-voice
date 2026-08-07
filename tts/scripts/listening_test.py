#!/usr/bin/env python3
"""Render sample announcements for a human listening test.

Drives the running callu-tts service over HTTP — the same path production uses,
including normalization and caching. It deliberately does not import the engine:
a listening test that bypasses the service proves nothing about the service.

Audio is written as 8 kHz mono WAV, the band a phone actually carries. Judging a
voice at 44.1 kHz flatters it.

Usage:
    python scripts/listening_test.py --url http://localhost:8080 --out samples/
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Dict, List

# A realistic page: a Turkish announcement wrapped around English alert content,
# with the identifiers and status codes that break naive number handling.
SERVICE = "payment-api"
SEVERITY = "acil"
TITLE = "PostgreSQL connection pool exhausted on port 5432 after 30 seconds. HTTP 503."

# Carriers clip the first seconds of a call while the media path settles, so the
# announcement opens with audio it can afford to lose. Everything after it is
# safe, which is why the service name can lead the message.
PREAMBLE_SILENCE_MS = 1000
PREAMBLE = {"text": "Callu bildirimi.", "lang": "tr"}

MESSAGE = {
    "text": f"{SERVICE} servisinden {SEVERITY} seviyeli bildirim. {TITLE}",
    "lang": "tr",
}
PROMPT = {
    "text": "Onaylamak için bire, yükseltmek için ikiye, tekrar dinlemek için "
            "yıldıza, video konferans için dokuza basın.",
    "lang": "tr",
}

CASES: Dict[str, List[Dict[str, str]]] = {
    # The English body is read with Turkish phonetics.
    "single_language": [PREAMBLE, MESSAGE, PROMPT],
    # The English body is synthesized as English and played back to back with the
    # Turkish segments, which is what Asterisk does with a list of media URIs.
    "per_segment_language": [
        PREAMBLE,
        {"text": f"{SERVICE} servisinden {SEVERITY} seviyeli bildirim.", "lang": "tr"},
        {"text": TITLE, "lang": "en"},
        PROMPT,
    ],
    # No preamble: demonstrates what clipping costs when the service name leads.
    "no_preamble": [MESSAGE, PROMPT],
}


def post_json(url: str, payload: dict) -> dict:
    request = urllib.request.Request(
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request) as response:
        return json.load(response)


def fetch(url: str) -> bytes:
    with urllib.request.urlopen(url) as response:
        return response.read()


def concat_wavs(chunks: List[bytes], lead_silence_ms: int = 0) -> bytes:
    """Join WAV payloads, mimicking Asterisk playing several sounds in sequence.

    In production the leading silence is Asterisk's own `sound:silence/1`; here it
    is generated so the sample plays back exactly as the callee would hear it.
    """
    import io
    import wave

    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as out:
        configured = False
        for chunk in chunks:
            with wave.open(io.BytesIO(chunk), "rb") as source:
                if not configured:
                    # Copy the format but not nframes: that is written on close,
                    # once the total frame count is known.
                    out.setnchannels(source.getnchannels())
                    out.setsampwidth(source.getsampwidth())
                    out.setframerate(source.getframerate())
                    configured = True
                    if lead_silence_ms:
                        frames = source.getframerate() * lead_silence_ms // 1000
                        out.writeframes(b"\x00" * frames * source.getsampwidth())
                out.writeframes(source.readframes(source.getnframes()))
    return buffer.getvalue()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://localhost:8080", help="callu-tts base URL")
    parser.add_argument("--out", type=Path, default=Path("samples"), help="output directory")
    parser.add_argument("--voices", nargs="+", default=["F1", "F2", "M1", "M2"])
    args = parser.parse_args()

    args.out.mkdir(parents=True, exist_ok=True)

    try:
        health = json.loads(fetch(f"{args.url}/health"))
    except urllib.error.URLError as exc:
        print(f"cannot reach callu-tts at {args.url}: {exc}", file=sys.stderr)
        return 1
    print(f"engine={health['engine']} languages={health['rule_languages']}\n")

    for voice in args.voices:
        for case, segments in CASES.items():
            result = post_json(
                f"{args.url}/synthesize",
                {"voice": voice, "segments": segments},
            )

            audio = [fetch(f"{args.url}/audio/{r['sound'].split('/', 1)[1]}")
                     for r in result["results"]]
            silence = 0 if case == "no_preamble" else PREAMBLE_SILENCE_MS
            target = args.out / f"{case}__{voice}.wav"
            target.write_bytes(concat_wavs(audio, lead_silence_ms=silence))

            cached = sum(r["cached"] for r in result["results"])
            print(f"{target.name:34s} {cached}/{len(result['results'])} cached")

    print(f"\nWritten to {args.out.resolve()}")
    print("Listen for:")
    print("  - is the English body intelligible?")
    print("  - does the voice stay the same person when the language changes?")
    print("  - imagine the first ~1.5s cut off: no_preamble loses the service name,")
    print("    the others lose only throwaway audio.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
