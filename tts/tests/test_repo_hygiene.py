"""A comment may only cite something a reader of this repository can open."""

import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

# The image ships the tests without the source tree, and a guard that always fails inside the
# container teaches people to ignore a red suite.
pytestmark = pytest.mark.skipif(
    not (REPO / "Dockerfile").is_file(),
    reason="repository tree not present; this guard has nothing to scan",
)

SCANNED_SUFFIXES = {".go", ".py", ".md", ".yml", ".yaml", ".conf", ".sh", ".template", ".example"}
SCANNED_NAMES = {"Dockerfile", "NOTICE"}

# Directory names, not paths — matched against every path component.
SKIPPED_DIRECTORIES = {
    ".git",
    "vendor",
    "__pycache__",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    "node_modules",
    ".venv",
    "venv",
    "site-packages",
}

FOREIGN_DOC_ROOTS = ("code-notes", "docs/decisions", "docs/reviews", "mvp/", "mvp\\")

TRACKER_ID = re.compile(r"\(\s*[A-Z]\d{1,2}(?:\s*[,;/]\s*[A-Z]?\d{1,2})*\s*\)|\bFix \d\d\.[A-Z0-9]")

DOC_PATH = re.compile(r"(?<![\w/:.-])(?:\./)?(?:[\w-]+/)*[\w-]+\.md\b")


def _sources():
    for path in sorted(REPO.rglob("*")):
        if not path.is_file():
            continue
        if any(part in SKIPPED_DIRECTORIES for part in path.parts):
            continue
        if path.name == Path(__file__).name:
            continue
        if path.suffix in SCANNED_SUFFIXES or path.name in SCANNED_NAMES:
            yield path


def _lines(path):
    text = path.read_text(encoding="utf-8", errors="replace")
    return enumerate(text.splitlines(), start=1)


def _label(path, number):
    return f"{path.relative_to(REPO).as_posix()}:{number}"


def test_no_file_points_at_a_document_outside_this_repository():
    offenders = [
        f"{_label(path, number)} cites `{root}`"
        for path in _sources()
        for number, line in _lines(path)
        for root in FOREIGN_DOC_ROOTS
        if root in line
    ]

    assert not offenders, (
        "These lines name a document that is not in this repository:\n  "
        + "\n  ".join(offenders)
        + "\n\nThis repository is public and those documents are not. A pointer a reader cannot "
        "follow is worse than no pointer: say the reason in one line here, or say nothing."
    )


def test_no_file_cites_an_opaque_tracker_id():
    offenders = [
        f"{_label(path, number)} cites `{match.group().strip()}`"
        for path in _sources()
        for number, line in _lines(path)
        for match in TRACKER_ID.finditer(line)
    ]

    assert not offenders, (
        "These lines cite an id that resolves nowhere in this repository:\n  "
        + "\n  ".join(offenders)
        + "\n\nNobody outside the authors' own tracker can look one up. Describe the defect in "
        "the comment's own words instead, or drop the comment."
    )


def test_every_document_path_resolves_to_a_file_that_ships_here():
    offenders = [
        f"{_label(path, number)} -> {reference}"
        for path in _sources()
        for number, line in _lines(path)
        for reference in DOC_PATH.findall(line)
        if not (path.parent / reference).exists() and not (REPO / reference).exists()
    ]

    assert not offenders, (
        "These references do not resolve to a file in this repository:\n  "
        + "\n  ".join(offenders)
        + "\n\nEither add the file, fix the path, or remove the reference."
    )


def test_the_detector_flags_a_pointer_a_reader_cannot_follow():
    # Invented filenames and ids only. Pasting a real one here would reintroduce exactly what
    # this detector exists to keep out.
    flagged = [
        f"# Reasoning: {FOREIGN_DOC_ROOTS[0]}/example.md (Q9).",
        f"// see {FOREIGN_DOC_ROOTS[0]}/example.md for the rationale",
        "// the hostNetwork comment on the Deployment (Q7)",
        "# supervisord gives up and exits (Q8)",
        "# above the provider AttemptTimeout. Fix 99.P0-9.",
    ]
    clean = [
        "# supervisord must exit when a program dies for good, or the box reports alive.",
        "// Draining: refuse new calls, let the in-flight ones finish their callbacks.",
        "# A 2 GiB ceiling, kept under the volume so the sweep bounds it, not a failing write.",
        "// See README.md for what CI does not cover.",
    ]

    for line in flagged:
        assert any(root in line for root in FOREIGN_DOC_ROOTS) or TRACKER_ID.search(line), line

    for line in clean:
        assert not any(root in line for root in FOREIGN_DOC_ROOTS), line
        assert not TRACKER_ID.search(line), line


def test_the_path_reader_takes_relative_paths_and_leaves_urls_alone():
    assert DOC_PATH.findall("// See README.md for the manual checks.") == ["README.md"]
    assert DOC_PATH.findall("# see docs/nope.md") == ["docs/nope.md"]
    assert DOC_PATH.findall("// https://github.com/orchesta/callu-voice/blob/main/README.md") == []


def test_the_scan_reads_the_tree_it_claims_to():
    scanned = {path.relative_to(REPO).as_posix() for path in _sources()}

    assert "README.md" in scanned
    assert "supervisord.conf" in scanned
    assert "Dockerfile" in scanned
    assert "deploy/kubernetes.yaml" in scanned
    assert "voice-api/internal/call/manager.go" in scanned
    assert "tts/app/service.py" in scanned
    assert "asterisk/etc/ari.conf.template" in scanned
    assert "asterisk/etc/pjsip.conf.template" in scanned
    assert ".env.example" in scanned
    assert not any(part.startswith("vendor/") for part in scanned)
    assert Path(__file__).name not in {Path(p).name for p in scanned}
