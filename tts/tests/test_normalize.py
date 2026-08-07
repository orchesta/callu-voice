# -*- coding: utf-8 -*-
"""Text normalization tests."""
import pytest

from app import normalize as normalize_mod
from app.normalize import (
    apply_lexicon,
    cardinal,
    digits,
    is_code_cue,
    load_rules,
    normalize,
    normalize_numbers,
    tr_cardinal,
)

# Supported European languages.
EURO = ["tr", "en", "de", "fr", "es", "it", "pt", "ru"]

TR = load_rules("tr-TR")
EN = load_rules("en-US")


class TestTurkishCardinal:
    @pytest.mark.parametrize("n,expected", [
        (0, "sıfır"), (1, "bir"), (9, "dokuz"), (10, "on"), (11, "on bir"),
        (30, "otuz"), (99, "doksan dokuz"),
        (100, "yüz"),                            # never "bir yüz"
        (101, "yüz bir"), (200, "iki yüz"), (503, "beş yüz üç"),
        (999, "dokuz yüz doksan dokuz"),
        (1000, "bin"),                           # never "bir bin"
        (1001, "bin bir"), (2000, "iki bin"),
        (5432, "beş bin dört yüz otuz iki"),
        (1_000_000, "bir milyon"),               # but "bir milyon" is correct
    ])
    def test_cardinal(self, n, expected):
        assert tr_cardinal(n) == expected

    def test_words_are_space_separated(self):
        # num2words yields 'beşbindörtyüzotuziki', which the engine reads as one token.
        assert " " in tr_cardinal(5432)


class TestEuropeanLanguages:
    @pytest.mark.parametrize("lang", EURO)
    def test_digit_table_available(self, lang):
        assert digits("503", lang) is not None

    @pytest.mark.parametrize("lang", EURO)
    def test_cardinal_available(self, lang):
        assert cardinal(30, lang) is not None

    @pytest.mark.parametrize("lang", EURO)
    def test_rules_file_exists(self, lang):
        assert load_rules(lang).known, f"no rule file for {lang}"

    def test_unsupported_language_leaves_text_raw(self):
        assert normalize("PostgreSQL 503", "zh-CN") == "PostgreSQL 503"


class TestLanguageTagResolution:
    @pytest.mark.parametrize("tag", ["tr", "tr-TR", "TR", "tr-tr"])
    def test_turkish_tags_resolve(self, tag):
        assert load_rules(tag).known
        assert "postgre es kü el" in normalize("PostgreSQL", tag)

    @pytest.mark.parametrize("tag", ["en", "en-US", "en-GB", "en-us"])
    def test_english_tags_resolve(self, tag):
        assert load_rules(tag).known
        assert "thirty" in normalize("30 seconds", tag)

    def test_short_tag_is_what_production_sends(self):
        out = normalize("payment-api servisinde HTTP 503", "tr")
        assert "ey pi ay" in out and "beş yüz üç" in out


class TestLanguageIsolation:
    def test_turkish_lexicon_applies_to_turkish(self):
        assert "postgre es kü el" in normalize("PostgreSQL çöktü", "tr-TR")

    def test_turkish_lexicon_does_not_leak_into_english(self):
        out = normalize("PostgreSQL connection pool exhausted", "en-US")
        assert "postgre es kü el" not in out
        assert "PostgreSQL" in out

    def test_turkish_lexicon_does_not_leak_into_german(self):
        assert "postgre es kü el" not in normalize("PostgreSQL nicht erreichbar", "de-DE")


class TestNumberContext:
    def test_quantity_is_cardinal(self):
        assert normalize_numbers("30 saniye sonra", "tr", TR) == "otuz saniye sonra"

    # Turkish says the code as a quantity; the cap below is what still protects real identifiers.
    def test_http_status_is_a_quantity(self):
        assert normalize_numbers("HTTP 503 hatası", "tr", TR) == "HTTP beş yüz üç hatası"

    def test_port_is_digits(self):
        assert normalize_numbers("port 5432", "tr", TR) == "port beş dört üç iki"

    def test_large_number_assumed_identifier(self):
        assert normalize_numbers("5432 numarası", "tr", TR) == "beş dört üç iki numarası"

    def test_small_number_without_cue_is_cardinal(self):
        assert normalize_numbers("3 sunucu", "tr", TR) == "üç sunucu"

    def test_english_quantity(self):
        assert normalize_numbers("after 30 seconds", "en", EN) == "after thirty seconds"

    def test_english_code(self):
        assert normalize_numbers("error 404", "en", EN) == "error four zero four"

    def test_german_code_cue(self):
        de = load_rules("de-DE")
        assert normalize_numbers("Fehler 404", "de", de) == "Fehler vier null vier"


class TestInflectedCodeCues:
    """Turkish agglutinates, so a fixed cue list misses "kodu", "kodunda", "numarası"."""

    # The stemming still has to work: it is what a language whose cues DO force digits relies on.
    # Turkish no longer reads a short code digit by digit, so these assert the stemmer directly
    # rather than an outcome that magnitude now decides.
    @pytest.mark.parametrize("word", [
        "kod", "kodu", "kodunda", "kodları", "kodlarında", "kodundan",
        "hata", "hatası", "hatasında", "durum", "durumu", "durumunda",
        "numara", "numarası", "port", "portu", "portta",
    ])
    def test_an_inflected_cue_is_recognised(self, word):
        assert is_code_cue(word, TR), word

    @pytest.mark.parametrize("text,expected", [
        ("30 saniye sonra", "otuz saniye sonra"),
        ("yüzde 30 arttı", "yüzde otuz arttı"),
        ("son 5 dakika", "son beş dakika"),
        ("toplam 12 istek", "toplam on iki istek"),
        ("sonra 40 kez", "sonra kırk kez"),
    ])
    def test_a_quantity_is_still_a_quantity(self, text, expected):
        assert normalize_numbers(text, "tr", TR) == expected

    def test_suffix_stripping_needs_a_real_stem(self):
        assert is_code_cue("kod", TR)
        assert is_code_cue("kodunda", TR)
        assert not is_code_cue("kodlama", TR)
        assert not is_code_cue("sunucu", TR)
        assert not is_code_cue("", TR)

    def test_a_locale_without_suffixes_matches_exactly(self):
        assert is_code_cue("code", EN)
        assert not EN.code_cue_suffixes
        assert not is_code_cue("codes", EN)


class TestLexicon:
    def test_case_insensitive(self):
        assert apply_lexicon("POSTGRESQL", {"postgresql": "X"}) == "X"

    def test_longest_key_wins(self):
        assert apply_lexicon("PostgreSQL", {"postgres": "A", "postgresql": "B"}) == "B"

    def test_word_boundary_respected(self):
        assert apply_lexicon("apifoo", {"api": "ey pi ay"}) == "apifoo"

    def test_hyphen_is_word_boundary(self):
        assert apply_lexicon("payment-api", {"api": "ey pi ay"}) == "payment-ey pi ay"

    def test_large_lexicon_still_compiles(self):
        # One capturing group per key is what makes the branch-index lookup
        # possible; `re` dropped its 100-group ceiling in 3.5, and a caller's
        # `lexicon_override` can push a language's lexicon well past 22 keys.
        lexicon = {f"term{i:03d}": f"spelled{i:03d}" for i in range(150)}
        assert apply_lexicon("term000 and term149", lexicon) == "spelled000 and spelled149"

    def test_regex_metacharacters_in_key_are_literal(self):
        assert apply_lexicon("c++ derleyicisi", {"c++": "si plas plas"}) == "si plas plas derleyicisi"


class TestTurkishDottedCapitalI:
    """`İ` (U+0130) in a lexicon term must not raise."""

    def test_ascii_capitals_already_work(self):
        # The contrast that made the bug specific, and why test_case_insensitive missed it.
        assert "ey pi ay" in normalize("API ÇÖKTÜ", "tr-TR")

    @pytest.mark.parametrize("text,term,respelled", [
        ("APİ ÇÖKTÜ", "APİ", "ey pi ay"),
        ("NGİNX yeniden başlatıldı", "NGİNX", "encinix"),
        ("TİMEOUT oluştu", "TİMEOUT", "taymaut"),
    ])
    def test_dotted_capital_i_is_not_a_crash(self, text, term, respelled):
        out = normalize(text, "tr-TR")

        assert respelled in out, out
        assert term not in out, out

    def test_dotted_and_ascii_capitals_agree(self):
        assert normalize("APİ ÇÖKTÜ", "tr-TR") == normalize("API ÇÖKTÜ", "tr-TR")

    def test_dotted_capital_i_that_matches_no_key_is_left_alone(self):
        # A miss, not a crash — recorded so nobody "fixes" it by widening the fold.
        assert normalize("KUBERNETİS çöktü", "tr-TR") == "KUBERNETİS çöktü"

    @pytest.mark.parametrize("text", [
        "APİ", "apİ", "Apİ", "aPİ", "İ", "İİİ",
        "NGİNX", "ngİnx", "TİMEOUT", "tİmeout",
        "SSL/TLS İSTEK", "CACHE İÇİN", "K8S İLE",
    ])
    def test_no_turkish_casing_raises(self, text):
        normalize(text, "tr-TR")


class TestTurkishDotlessI:
    """Dotless `ı` (U+0131) where a key has `i` respells rather than raising."""

    def test_dotless_i_respells(self):
        assert apply_lexicon("apı çöktü", {"api": "ey pi ay"}) == "ey pi ay çöktü"

    @pytest.mark.parametrize("text,respelled", [
        ("apı yanıt vermiyor", "ey pi ay"),
        ("tımeout oluştu", "taymaut"),
    ])
    def test_dotless_i_respells_through_normalize(self, text, respelled):
        assert respelled in normalize(text, "tr-TR")


class TestNumberWordsUnavailable:
    """A missing num2words degrades the reading but must never stop a page."""

    def test_it_is_available_here(self):
        assert normalize_mod.number_words_error() is None

    @pytest.fixture
    def without_num2words(self, monkeypatch):
        monkeypatch.setattr(normalize_mod, "_num2words", None)
        monkeypatch.setattr(normalize_mod, "_NUMBER_WORDS_ERROR", "ImportError: no num2words")
        normalize_mod._digit_table.cache_clear()
        yield
        normalize_mod._digit_table.cache_clear()

    def test_the_reason_is_reportable(self, without_num2words):
        assert normalize_mod.number_words_error() == "ImportError: no num2words"

    def test_digits_and_cardinals_degrade_rather_than_raise(self, without_num2words):
        assert digits("503", "en") is None
        assert cardinal(30, "en") is None

    def test_the_number_is_left_as_written(self, without_num2words):
        assert normalize_numbers("error 503 after 30 seconds", "en", EN) == \
            "error 503 after 30 seconds"

    def test_turkish_cardinals_are_ours_and_survive(self, without_num2words):
        assert cardinal(30, "tr") == "otuz"
        assert normalize_numbers("30 saniye sonra", "tr", TR) == "otuz saniye sonra"


class TestOverride:
    def test_override_adds_to_defaults(self):
        out = normalize("PostgreSQL ve Grafana", "tr-TR", {"grafana": "grafana"})
        assert "postgre es kü el" in out

    def test_override_wins_over_default(self):
        out = normalize("PostgreSQL çöktü", "tr-TR", {"postgresql": "postgre"})
        assert "postgre çöktü" in out and "es kü el" not in out


class TestEndToEnd:
    def test_turkish_announcement(self):
        out = normalize("payment-api servisinde HTTP 503, port 5432 kapandı", "tr-TR")
        assert "ey pi ay" in out
        assert "beş yüz üç" in out
        assert "beş dört üç iki" in out
        assert "503" not in out

    def test_english_body(self):
        out = normalize("PostgreSQL connection pool exhausted after 30 seconds", "en-US")
        assert "thirty seconds" in out
        assert "PostgreSQL" in out
        assert "30" not in out


class TestTurkishReadsShortCodesAsQuantities:
    """A Turkish speaker says "bes yuz uc hatasi" for 503, never "bes sifir uc hatasi"."""

    # Magnitude alone decides here: a cue cannot force digits below max_cardinal, so the same
    # number reads the same whether the cue comes before it or after it.
    @pytest.mark.parametrize("text", [
        "Sunucu 503 hatası veriyor",
        "503 hatası aldık",
        "hata kodu 503",
        "hata kodunda 503",
        "durum kodu 503",
        "503 HATASI",
    ])
    def test_a_short_code_is_read_as_a_quantity(self, text):
        out = normalize_numbers(text, "tr", TR)

        assert "beş yüz üç" in out, out
        assert "beş sıfır üç" not in out, out

    # The cap is what protects the identifiers a quantity reading would mangle.
    @pytest.mark.parametrize("text,expected", [
        ("port 8080", "sekiz sıfır sekiz sıfır"),
        ("8080 portu", "sekiz sıfır sekiz sıfır"),
        ("sipariş 184729", "bir sekiz dört yedi iki dokuz"),
    ])
    def test_anything_at_or_above_the_cap_is_still_read_digit_by_digit(self, text, expected):
        assert expected in normalize_numbers(text, "tr", TR)

    @pytest.mark.parametrize("text,expected", [
        ("30 saniye bekleyin", "otuz saniye"),
        ("15 dakika sonra", "on beş dakika"),
    ])
    def test_an_ordinary_quantity_is_unaffected(self, text, expected):
        assert expected in normalize_numbers(text, "tr", TR)

    # English keeps the cue: "HTTP 503" is an identifier there, and "503 errors" is a count.
    def test_english_still_uses_its_cues(self):
        assert "five zero three" in normalize_numbers("HTTP 503 from upstream", "en", EN)
        assert "five hundred" in normalize_numbers("503 errors were logged", "en", EN)
