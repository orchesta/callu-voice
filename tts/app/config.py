"""Settings. Single source — no scattered os.getenv calls."""
from __future__ import annotations

from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="TTS_", env_file=".env")

    # Shared volume, mounted read-only into Asterisk's sounds directory.
    sounds_dir: Path = Path("/sounds")
    rules_dir: Path = Path(__file__).parent.parent / "rules"

    # Resolved by Asterisk as "sound:<prefix>/<hash>".
    sound_prefix: str = "callu"

    # Telephony band. Generated directly in this format so Asterisk never
    # transcodes at call time.
    sample_rate: int = 8000

    default_voice: str = "F1"
    synthesis_steps: int = 8
    synthesis_speed: float = 1.0

    # Ceiling for the rendered-audio directory. Every incident is a new key kept
    # forever, so without a bound the volume fills and every page then fails on the
    # write. 0 disables the sweep.
    cache_max_bytes: int = 2 * 1024 * 1024 * 1024

    # How long a caller may wait for another request that is already rendering the
    # same text before giving up.
    single_flight_timeout_s: float = 55.0


def get_settings() -> Settings:
    return Settings()
