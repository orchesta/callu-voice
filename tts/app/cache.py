"""Audio cache: hash -> WAV file -> Asterisk "sound:" id.

Synthesis is far too slow to sit on the call path, so every rendered segment is
cached. Static segments (prompts, severity labels) are produced once and reused
for the lifetime of the deployment.
"""
from __future__ import annotations

import hashlib
import logging
import os
import time
import wave
from pathlib import Path
from typing import Optional

log = logging.getLogger(__name__)

# Bump whenever the rendered audio changes shape (container, sample rate, header
# layout). Cached files are otherwise served forever, including broken ones.
AUDIO_FORMAT_VERSION = 3


# A temp file older than this was left by a process that died mid-write.
_STALE_TMP_SECONDS = 300


class AudioCache:
    def __init__(self, root: Path, prefix: str, max_bytes: int = 0) -> None:
        self._root = root
        self._prefix = prefix
        self._max_bytes = max_bytes
        self._root.mkdir(parents=True, exist_ok=True)

    def key(
        self,
        engine_build: str,
        voice: str,
        lang: str,
        normalized_text: str,
        sample_rate: int,
    ) -> str:
        """Everything that shapes the waveform is in the key, engine build included."""
        raw = (
            f"v{AUDIO_FORMAT_VERSION}|{engine_build}|{sample_rate}"
            f"|{voice}|{lang}|{normalized_text}"
        ).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()[:32]

    def sound_id(self, key: str) -> str:
        return f"{self._prefix}/{key}"

    def get(self, key: str) -> Optional[Path]:
        path = self._path(key)
        return path if path.exists() else None

    def store(self, key: str, wav: bytes) -> Path:
        """Publish atomically, so Asterisk can never read a half-written file."""
        path = self._path(key)
        tmp = path.with_name(f".{key}.{os.getpid()}.tmp")
        tmp.write_bytes(wav)
        tmp.replace(path)
        self._sweep()
        return path

    def count(self) -> int:
        return sum(1 for _ in self._root.glob("*.wav"))

    def total_bytes(self) -> int:
        """Reported on /health: the growth has to be visible before it is fatal."""
        total = 0
        for path in self._root.glob("*.wav"):
            try:
                total += path.stat().st_size
            except OSError:
                continue  # deleted under us by a concurrent sweep
        return total

    def clear_stale_temp_files(self) -> int:
        removed = 0
        cutoff = time.time() - _STALE_TMP_SECONDS
        for path in self._root.glob(".*.tmp"):
            try:
                if path.stat().st_mtime < cutoff:
                    path.unlink()
                    removed += 1
            except OSError:
                continue
        return removed

    def _sweep(self) -> None:
        """
        Bounded LRU. Runs after a store rather than on a timer: the only thing that
        grows the directory is a store, so that is the only moment the bound can be
        crossed, and it keeps the whole mechanism inside one process with no scheduler.

        Least-recently-USED, by atime where the filesystem keeps it: Asterisk reads
        these files without touching mtime, so mtime would evict a prompt that is
        played on every call in favour of an incident text nobody will hear again.
        """
        if self._max_bytes <= 0:
            return

        try:
            entries = []
            total = 0
            for path in self._root.glob("*.wav"):
                try:
                    st = path.stat()
                except OSError:
                    continue
                entries.append((max(st.st_atime, st.st_mtime), st.st_size, path))
                total += st.st_size

            if total <= self._max_bytes:
                return

            # Evict down to 80% so a directory sitting exactly on the bound does not
            # re-sweep on every single store.
            target = int(self._max_bytes * 0.8)
            entries.sort(key=lambda e: e[0])

            freed = 0
            removed = 0
            for _, size, path in entries:
                if total - freed <= target:
                    break
                try:
                    path.unlink()
                except OSError:
                    continue
                freed += size
                removed += 1

            log.warning(
                "audio cache swept: removed=%d freed=%.1fMB now=%.1fMB bound=%.1fMB",
                removed, freed / 1e6, (total - freed) / 1e6, self._max_bytes / 1e6,
            )
        except OSError as exc:
            # A sweep that fails must never fail the page it was triggered by.
            log.error("audio cache sweep failed: %s", exc)

    @staticmethod
    def duration_s(path: Path) -> float:
        """Read from the WAV header rather than assumed: the rate is configurable."""
        try:
            with wave.open(str(path), "rb") as wav:
                rate = wav.getframerate()
                return wav.getnframes() / rate if rate else 0.0
        except (OSError, wave.Error) as exc:
            log.warning("cannot read the duration of %s: %s", path.name, exc)
            return 0.0

    def _path(self, key: str) -> Path:
        return self._root / f"{key}.wav"
