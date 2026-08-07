#!/bin/sh
# Render Asterisk's configuration from templates, then hand over to supervisord.
# Credentials arrive as environment variables, so no secret is baked into the image.
set -eu

TEMPLATE_DIR=/etc/asterisk.template
CONFIG_DIR=/etc/asterisk
# The trunk half of pjsip.conf, which #includes it by absolute path. Nothing here renders a
# carrier: the container starts without one and PUT /trunk supplies it.
TRUNK_FILE="$CONFIG_DIR/pjsip_trunk.conf"

require() {
    eval "value=\${$1:-}"
    if [ -z "$value" ]; then
        echo "entrypoint: $1 is required" >&2
        exit 1
    fi
}

require ARI_PASSWORD

export ARI_USERNAME="${ARI_USERNAME:-callu}"
export ARI_PASSWORD
export RTP_START="${RTP_START:-10000}"
export RTP_END="${RTP_END:-10099}"

# --- process isolation ----------------------------------------------------
# Synthesis is left at least two cores short of the machine, so it can never
# starve the media threads no matter how many pages are rendered at once.
if [ -z "${TTS_THREADS:-}" ]; then
    cores="$(nproc)"
    if [ "$cores" -gt 3 ]; then
        TTS_THREADS=$((cores - 2))
    else
        TTS_THREADS=1
    fi
fi
export TTS_THREADS

# --- everything below is Asterisk's config --------------------------------
# Only needed when Asterisk sits behind NAT, which includes bridged Docker.
# Without it Asterisk advertises its private address in SDP and audio goes nowhere.
NAT_CONFIG=""
if [ -n "${EXTERNAL_MEDIA_ADDRESS:-}" ]; then
    NAT_CONFIG="external_media_address=${EXTERNAL_MEDIA_ADDRESS}
external_signaling_address=${EXTERNAL_SIGNALING_ADDRESS:-${EXTERNAL_MEDIA_ADDRESS}}
local_net=10.0.0.0/8
local_net=172.16.0.0/12
local_net=192.168.0.0/16
local_net=127.0.0.0/8"
fi
export NAT_CONFIG

# Development only: lets a softphone stand in for the PSTN.
TEST_CONFIG="; test endpoints disabled"
if [ "${TEST_ENDPOINTS:-false}" = "true" ]; then
    require TEST_PASSWORD
    TEST_CONFIG="[6001]
type=endpoint
transport=transport-udp
context=from-internal
disallow=all
allow=ulaw,alaw
dtmf_mode=auto
auth=6001
aors=6001
direct_media=no
rtp_symmetric=yes
force_rport=yes
rewrite_contact=yes

[6001]
type=auth
auth_type=userpass
username=6001
password=${TEST_PASSWORD}

[6001]
type=aor
max_contacts=1"
fi
export TEST_CONFIG

mkdir -p "$CONFIG_DIR"
for template in "$TEMPLATE_DIR"/*; do
    name=$(basename "$template")
    case "$name" in
        *.template) envsubst < "$template" > "$CONFIG_DIR/${name%.template}" ;;
        *)          cp "$template" "$CONFIG_DIR/$name" ;;
    esac
done

# Empty on every boot: no carrier is this container's normal state, and Callu sends one over
# PUT /trunk. The file still has to exist, because pjsip.conf #includes it unconditionally.
printf '%s\n' "; no trunk configured" > "$TRUNK_FILE"

# Rendered config holds trunk and ARI passwords.
chmod 600 "$CONFIG_DIR"/*.conf

exec "$@"
