"""Where the trunk config lives, and that nothing but the caller can put a carrier in it.

Nothing here runs Asterisk. These are cross-file agreements that break silently — a carrier
that is never dialled through looks exactly like a quiet night.
"""

import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

pytestmark = pytest.mark.skipif(
    not (REPO / "entrypoint.sh").is_file(),
    reason="repository tree not present; this guard has nothing to scan",
)

ENTRYPOINT = REPO / "entrypoint.sh"
PJSIP_TEMPLATE = REPO / "asterisk" / "etc" / "pjsip.conf.template"
MAIN_GO = REPO / "voice-api" / "cmd" / "voice-api" / "main.go"
CONFIG_GO = REPO / "voice-api" / "internal" / "config" / "config.go"
TRUNK_GO = REPO / "voice-api" / "internal" / "trunk" / "trunk.go"
COMPOSE = REPO / "docker-compose.yml"
ENV_EXAMPLE = REPO / ".env.example"
KUBERNETES = REPO / "deploy" / "kubernetes.yaml"

TRUNK_PATH = "/etc/asterisk/pjsip_trunk.conf"

# TRUNK_ENDPOINT is in this list too: it named the endpoint /health watches, which the dial
# format already decides, and two ways to say one thing is one more way to get it wrong.
TRUNK_ENV_VARS = [
    "TRUNK_ENABLED",
    "TRUNK_HOST",
    "TRUNK_PORT",
    "TRUNK_USERNAME",
    "TRUNK_PASSWORD",
    "TRUNK_AUTH_USERNAME",
    "TRUNK_CALLER_ID",
    "TRUNK_REGISTER",
    "TRUNK_DTMF_MODE",
    "TRUNK_CODECS",
    "TRUNK_ENDPOINT",
]


@pytest.mark.parametrize(
    "path",
    [ENTRYPOINT, COMPOSE, ENV_EXAMPLE, KUBERNETES, CONFIG_GO, TRUNK_GO],
    ids=lambda p: p.name,
)
def test_the_carrier_cannot_be_set_from_the_environment(path):
    """Two ways to configure one carrier is two things that can disagree.

    The environment won at boot and the caller's push won afterwards, so which carrier was
    live depended on when the container was last recreated, and nothing said so.
    """
    body = path.read_text(encoding="utf-8")

    found = [name for name in TRUNK_ENV_VARS if name in body]
    assert not found, f"{path.name} still reads or documents {', '.join(found)}"


def test_boot_leaves_the_container_with_no_carrier():
    """No carrier is this container's normal state, so boot writes the file empty every time.

    It still has to write it: pjsip.conf #includes the path unconditionally.
    """
    body = ENTRYPOINT.read_text(encoding="utf-8")

    assert re.search(
        r"printf\s+'%s\\n'\s+\"; no trunk configured\"\s+>\s+\"\$TRUNK_FILE\"", body
    ), "boot no longer writes an empty trunk config"
    assert "[trunk-auth]" not in body, "the entrypoint renders a carrier again"


def test_every_side_agrees_on_where_the_trunk_file_lives():
    """Three files name this path independently; a disagreement is a trunk nobody loads."""
    entrypoint = ENTRYPOINT.read_text(encoding="utf-8")
    assert 'TRUNK_FILE="$CONFIG_DIR/pjsip_trunk.conf"' in entrypoint
    assert "CONFIG_DIR=/etc/asterisk" in entrypoint
    assert f"#include {TRUNK_PATH}" in PJSIP_TEMPLATE.read_text(encoding="utf-8")
    assert f'ConfigPath = "{TRUNK_PATH}"' in TRUNK_GO.read_text(encoding="utf-8")


def test_the_health_surface_is_told_when_the_carrier_changes():
    """Removing the carrier deletes the endpoint /health reads, and no event ever arrives for an
    object that no longer exists.

    Unwired, an unlinked carrier reads as one that stopped answering and the service reports
    unhealthy for the life of the process — including after a carrier is put back.
    """
    body = MAIN_GO.read_text(encoding="utf-8")

    assert "trunks.Watch(ari.NoteCarrier)" in body, (
        "nothing tells the health surface that the carrier changed"
    )
    assert "trunk.NewManager(trunk.ConfigPath)" in body, (
        "the watched trunk manager is not the one PUT /trunk applies carriers through"
    )


def test_no_volume_keeps_the_carrier_across_a_recreate():
    """A volume holding a rendered config is a second source of truth nobody can read.

    The carrier is the caller's to hold. This container forgetting it on a recreate is what
    makes the caller's reconciliation the only thing deciding which carrier is live.
    """
    for path in (COMPOSE, KUBERNETES):
        body = path.read_text(encoding="utf-8")
        assert "/var/lib/callu-voice" not in body, (
            f"{path.name} still mounts storage for the trunk config"
        )


def test_the_readable_trunk_state_is_not_written_to_disk():
    """Same reason: a file describing the carrier outlives the reload that made it true."""
    body = TRUNK_GO.read_text(encoding="utf-8")

    assert ".pjsip_trunk.state.json" not in body
    assert "func (m *Manager) writeState" not in body


def test_the_trunk_reports_a_fingerprint_the_caller_can_compare():
    """Without one the caller cannot tell "still there" from "gone", so it either re-pushes
    the carrier on every sweep or never notices it went missing."""
    body = TRUNK_GO.read_text(encoding="utf-8")

    assert 'Fingerprint string    `json:"fingerprint"`' in body
    assert "func (s Settings) Fingerprint() string" in body


def test_a_tcp_transport_exists_for_a_trunk_that_asks_for_one():
    """A transport's protocol and bind are not reloadable, so it has to exist from boot.

    PUT /trunk accepts transport=tcp; pointing an endpoint at a transport that was never
    declared gives a trunk that loads and never connects.
    """
    template = PJSIP_TEMPLATE.read_text(encoding="utf-8")

    assert "[transport-tcp]" in template
    assert "protocol=tcp" in template
