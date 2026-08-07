"""Language tag helpers.

Callers use either "tr-TR" (template locale) or "tr" (engine code). Both must
resolve to the same rules, or normalization silently turns itself off.
"""
from __future__ import annotations


def primary(lang: str) -> str:
    """Return the primary subtag: "tr-TR" -> "tr"."""
    return lang.split("-")[0].lower()
