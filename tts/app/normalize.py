"""Text normalization applied before synthesis.

Two passes, both driven by per-language rule files (``rules/<locale>.json``):

1. Pronunciation lexicon — respells technical terms phonetically for the target
   language. This is not translation: meaning and identifiers are preserved.
2. Number expansion — spells digits out, because TTS engines mis-read long
   numbers. Context decides how: a quantity becomes a cardinal ("30" -> "thirty"),
   an identifier is read digit by digit ("HTTP 503" -> "five zero three"), since
   "five hundred three" would send an on-call engineer to the wrong place.

Rules are per-language on purpose. A single global lexicon leaks: Turkish
phonetics applied to an English segment make an English voice unintelligible.
When a language has no rule file, text passes through untouched — leaving a digit
for the engine to read is safer than applying another language's rules.
"""
from __future__ import annotations

import json
import logging
import os
import re
from dataclasses import dataclass, field
from functools import lru_cache
from pathlib import Path
from typing import Dict, Iterator, List, Optional, Sequence, Set, Tuple

from app.lang import primary

log = logging.getLogger(__name__)

RULES_DIR = Path(os.getenv("TTS_RULES_DIR", str(Path(__file__).parent.parent / "rules")))

# Resolved once, at import, so a missing install is one ERROR line and a /health
# field rather than a silent per-request degradation.
_num2words = None
_NUMBER_WORDS_ERROR: Optional[str] = None
try:
    from num2words import num2words as _num2words
except Exception as _exc:  # noqa: BLE001 - any import failure means "unavailable"
    _NUMBER_WORDS_ERROR = f"{type(_exc).__name__}: {_exc}"
    log.error(
        "num2words unavailable (%s); numbers will be spoken as written, so an "
        "identifier can be read as a quantity",
        _NUMBER_WORDS_ERROR,
    )


def number_words_error() -> Optional[str]:
    """Why digit-by-digit reading is unavailable, or None when it works."""
    return _NUMBER_WORDS_ERROR


_TR_ONES = ["", "bir", "iki", "üç", "dört", "beş", "altı", "yedi", "sekiz", "dokuz"]
_TR_TENS = ["", "on", "yirmi", "otuz", "kırk", "elli", "altmış", "yetmiş", "seksen", "doksan"]


def _tr_below_1000(n: int) -> List[str]:
    out: List[str] = []
    hundreds, rest = divmod(n, 100)
    if hundreds:
        out.append("yüz" if hundreds == 1 else f"{_TR_ONES[hundreds]} yüz")
    tens, ones = divmod(rest, 10)
    if tens:
        out.append(_TR_TENS[tens])
    if ones:
        out.append(_TR_ONES[ones])
    return out


def tr_cardinal(n: int) -> str:
    """Turkish cardinal number.

    Hand-written because num2words joins the words together
    ("beşbindörtyüzotuziki"), which a character-level TTS engine reads as a
    single meaningless token. Turkish orthography separates them.
    """
    if n == 0:
        return "sıfır"
    if n < 0:
        return "eksi " + tr_cardinal(-n)

    parts: List[str] = []
    for value, name in ((1_000_000, "milyon"), (1_000, "bin")):
        chunk, n = divmod(n, value)
        if not chunk:
            continue
        if chunk == 1 and name == "bin":
            parts.append(name)  # "bin", never "bir bin"
        else:
            parts.extend(_tr_below_1000(chunk))
            parts.append(name)  # but "bir milyon" is correct
    parts.extend(_tr_below_1000(n))
    return " ".join(p for p in parts if p)


@lru_cache(maxsize=64)
def _digit_table(lang: str) -> Optional[List[str]]:
    """Digit words 0-9, derived from num2words.

    Single digits are one word in every language, so the word-joining behaviour
    that rules num2words out for Turkish cardinals is harmless here.
    """
    if _num2words is None:
        return None
    try:
        return [_num2words(d, lang=primary(lang)) for d in range(10)]
    except Exception:
        return None


def digits(number: str, lang: str) -> Optional[str]:
    """Read digit by digit: "503" -> "five zero three"."""
    table = _digit_table(lang)
    if table is None:
        return None
    return " ".join(table[int(c)] for c in number if c.isdigit())


def cardinal(n: int, lang: str) -> Optional[str]:
    """Read as a quantity: 30 -> "thirty". None when the language is unsupported."""
    if primary(lang) == "tr":
        return tr_cardinal(n)
    if _num2words is None:
        return None
    try:
        return _num2words(n, lang=primary(lang))
    except Exception:
        return None


@dataclass(frozen=True)
class Rules:
    lexicon: Dict[str, str] = field(default_factory=dict)
    code_cues: Set[str] = field(default_factory=set)
    # Suffixes an inflected cue may carry, stackable ("kod" -> "kodunda").
    code_cue_suffixes: Tuple[str, ...] = ()
    # Whether a cue makes a number below max_cardinal an identifier. English reads "HTTP 503" as
    # digits; Turkish says "bes yuz uc hatasi" for the same code, so there magnitude alone decides.
    cue_forces_digits: bool = True
    max_cardinal: int = 1000
    known: bool = False


_EMPTY = Rules()


@lru_cache(maxsize=1)
def _rule_index() -> Dict[str, Path]:
    """Index rule files under both the full tag and the primary subtag.

    Callers use either form ("tr-TR" from templates, "tr" for the engine); a
    one-way lookup would silently disable normalization for the other.
    """
    index: Dict[str, Path] = {}
    for path in sorted(RULES_DIR.glob("*.json")):
        tag = path.stem.lower()
        index[tag] = path
        index.setdefault(tag.split("-")[0], path)
    return index


@lru_cache(maxsize=32)
def load_rules(lang: str) -> Rules:
    path = _rule_index().get(lang.lower()) or _rule_index().get(primary(lang))
    if path is None:
        log.warning("no rule file for %r; skipping normalization", lang)
        return _EMPTY

    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        # A broken rule file must not silence the call.
        log.exception("cannot read %s; continuing without normalization", path)
        return _EMPTY

    return Rules(
        lexicon={k.lower(): v for k, v in (data.get("lexicon") or {}).items()},
        code_cues={c.lower() for c in (data.get("code_cues") or [])},
        code_cue_suffixes=tuple(
            sorted((s.lower() for s in (data.get("code_cue_suffixes") or [])), key=len, reverse=True)
        ),
        cue_forces_digits=bool(data.get("cue_forces_digits", True)),
        max_cardinal=int(data.get("max_cardinal", 1000)),
        known=True,
    )


_NUMBER_RE = re.compile(r"\b(\d+)\b")
_WORD_RE = re.compile(r"[^\W\d_]+", re.UNICODE)


def _cue_stems(word: str, suffixes: Sequence[str]) -> Iterator[str]:
    """Yield `word` and every stem reachable by stripping listed suffixes."""
    seen = {word}
    pending = [word]
    while pending:
        current = pending.pop()
        yield current
        for suffix in suffixes:
            if len(current) > len(suffix) and current.endswith(suffix):
                stem = current[: -len(suffix)]
                if stem not in seen:
                    seen.add(stem)
                    pending.append(stem)


def is_code_cue(word: str, rules: Rules) -> bool:
    return any(stem in rules.code_cues for stem in _cue_stems(word, rules.code_cue_suffixes))


def normalize_numbers(text: str, lang: str, rules: Rules) -> str:
    tokens = _NUMBER_RE.split(text)
    out: List[str] = []
    for i, token in enumerate(tokens):
        if i % 2 == 0:
            out.append(token)
            continue

        preceding = _WORD_RE.findall(tokens[i - 1])
        cue = preceding[-1].lower() if preceding else ""
        value = int(token)

        cued = rules.cue_forces_digits and is_code_cue(cue, rules)
        is_identifier = cued or value >= rules.max_cardinal
        spoken = digits(token, lang) if is_identifier else cardinal(value, lang)
        out.append(spoken if spoken is not None else token)
    return "".join(out)


def apply_lexicon(text: str, lexicon: Dict[str, str]) -> str:
    """Case-insensitive, whole-word replacement. Longest key wins.

    One branch per key, so the replacement follows `m.lastindex` and the matched text
    is never case-folded back into a key.
    """
    if not lexicon:
        return text
    keys = sorted(lexicon, key=len, reverse=True)
    pattern = re.compile(
        r"(?<![A-Za-z0-9])(?:"
        + "|".join(f"({re.escape(k)})" for k in keys)
        + r")(?![A-Za-z0-9])",
        re.IGNORECASE,
    )

    def replace(m: re.Match[str]) -> str:
        # Only reachable for a degenerate empty key; leave the term as written.
        if m.lastindex is None:
            return m.group(0)
        return lexicon.get(keys[m.lastindex - 1], m.group(0))

    return pattern.sub(replace, text)


def normalize(text: str, lang: str, lexicon_override: Optional[Dict[str, str]] = None) -> str:
    """Normalize `text` for synthesis in `lang`.

    `lexicon_override` is merged on top of the language's default lexicon, so a
    caller can add its own terms without losing the defaults.
    """
    rules = load_rules(lang)
    if not rules.known and not lexicon_override:
        return text.strip()

    lexicon = rules.lexicon
    if lexicon_override:
        lexicon = {**rules.lexicon, **{k.lower(): v for k, v in lexicon_override.items()}}

    text = apply_lexicon(text, lexicon)
    if rules.known:
        text = normalize_numbers(text, lang, rules)
    return re.sub(r"\s{2,}", " ", text).strip()
