# One image: Asterisk, the synthesizer and the call flow, kept apart by priority
# and thread count rather than by container boundaries (supervisord.conf).

FROM golang:1.26 AS voice-api-build

WORKDIR /src
COPY voice-api/go.mod voice-api/go.sum ./
RUN go mod download

COPY voice-api/cmd/ ./cmd/
COPY voice-api/internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/voice-api ./cmd/voice-api


# Pinned to a Debian release, not just a Python one: Asterisk is compiled from source here against
# whatever the base image ships, and the floating tag has already moved from bookworm to trixie
# once. A move like that is a rebuild that behaves differently for no reason in this repo.
FROM python:3.14.7-slim-trixie

ARG AST_VERSION=22.10.1
ARG AST_SHA256=0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353

ENV DEBIAN_FRONTEND=noninteractive

COPY tts/requirements.txt /tmp/requirements.txt

# Build, install and teardown share one layer. Docker layers are additive:
# purging the toolchain later leaves it in the image and only records a deletion.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        build-essential wget ca-certificates gettext-base libcap2-bin; \
    \
    cd /usr/src; \
    # Upstream keeps only the branch's current release at the top level and moves the rest into
    # releases/, so both are tried. The checksum below is what makes that safe.
    AST_BASE=https://downloads.asterisk.org/pub/telephony/asterisk; \
    wget -O asterisk.tar.gz "${AST_BASE}/asterisk-${AST_VERSION}.tar.gz" \
      || wget -O asterisk.tar.gz "${AST_BASE}/releases/asterisk-${AST_VERSION}.tar.gz"; \
    echo "${AST_SHA256}  asterisk.tar.gz" | sha256sum -c -; \
    tar xzf asterisk.tar.gz; \
    rm asterisk.tar.gz; \
    \
    cd "/usr/src/asterisk-${AST_VERSION}"; \
    yes | contrib/scripts/install_prereq install; \
    ./configure --with-jansson-bundled --with-pjproject-bundled; \
    make menuselect.makeopts; \
    \
    # Build only what an outbound paging call touches. menuselect exits non-zero on a name it does
    # not know, without saying which, so each target is applied on its own and a miss is reported.
    for category in MENUSELECT_CDR MENUSELECT_CEL MENUSELECT_ADDONS MENUSELECT_TESTS; do \
        menuselect/menuselect --disable-category "$category" menuselect.makeopts \
            || echo "menuselect: no category $category"; \
    done; \
    # Inbound SIP parsers this outbound-only service never uses, sitting behind a port the carrier
    # can reach. res_pjsip_pubsub is deliberately NOT here: chan_pjsip hard-depends on it and
    # disabling it takes the PJSIP channel driver with it.
    for module in \
        res_pjsip_refer res_pjsip_mwi res_pjsip_exten_state \
        res_pjsip_publish_asterisk res_pjsip_outbound_publish \
        chan_iax2 chan_motif chan_unistim chan_console \
        res_xmpp res_snmp res_phoneprov res_pjsip_phoneprov_provider \
        res_odbc res_odbc_transaction res_config_odbc func_odbc \
        res_config_pgsql res_config_ldap \
        res_hep res_hep_rtcp res_hep_pjsip \
        app_adsiprog app_getcpeid res_adsi \
        res_fax res_fax_spandsp \
        res_calendar res_calendar_caldav res_calendar_ews \
        res_calendar_exchange res_calendar_icalendar \
        app_voicemail app_directory app_minivm app_meetme app_festival \
        pbx_ael pbx_lua \
    ; do \
        menuselect/menuselect --disable "$module" menuselect.makeopts \
            || echo "menuselect: no module $module"; \
    done; \
    \
    # One job per 3GB, capped at the core count. pjproject's translation units are large enough that a
    # high -j gets the compiler killed, and Asterisk's third-party wrapper hides the sub-compiler's
    # output — so it arrives as a bare "Error 2" with nothing to go on. The ratio is measured, not
    # derived: on a 13GB builder -j16 and -j6 both died there and -j4 completed.
    # The cgroup limit first: inside a memory-capped container /proc/meminfo still reports the whole
    # host, so a builder capped at 2GB would compute a -j sized for 64GB and be killed anyway.
    BUILD_MEM_BYTES=""; \
    for f in /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory/memory.limit_in_bytes; do \
        [ -r "$f" ] || continue; \
        v="$(cat "$f")"; \
        case "$v" in ''|max|*[!0-9]*) continue;; esac; \
        # An unset v1 limit is a sentinel near 2^63, not a real reading.
        if [ "$v" -ge 4611686018427387904 ]; then continue; fi; \
        BUILD_MEM_BYTES="$v"; break; \
    done; \
    if [ -z "$BUILD_MEM_BYTES" ]; then \
        BUILD_MEM_BYTES="$(awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo)"; \
    fi; \
    BUILD_JOBS="$(nproc)"; \
    BUILD_MEM_JOBS="$((BUILD_MEM_BYTES / 3221225472))"; \
    if [ "${BUILD_MEM_JOBS:-0}" -lt "$BUILD_JOBS" ]; then BUILD_JOBS="$BUILD_MEM_JOBS"; fi; \
    if [ "${BUILD_JOBS:-0}" -lt 1 ]; then BUILD_JOBS=1; fi; \
    echo "asterisk build: -j${BUILD_JOBS} ($(nproc) cores, $((BUILD_MEM_BYTES / 1073741824))GB available, room for ${BUILD_MEM_JOBS})"; \
    make -j"$BUILD_JOBS"; \
    make install; \
    make samples; \
    # The sample is deprecated upstream and unused here (endpoints live in pjsip.conf);
    # left in place it warns on every start.
    rm -f /etc/asterisk/users.conf; \
    \
    # Real-time scheduling as a file capability — no root, no container-wide cap; the xattr
    # survives the purge below. Silently ignored under no-new-privileges (see docker-compose.yml).
    setcap cap_sys_nice+ep /usr/sbin/asterisk; \
    \
    # The synthesizer, installed while the compiler is still around in case a
    # wheel has to be built.
    pip install --no-cache-dir -r /tmp/requirements.txt supervisor; \
    \
    cd /; \
    \
    # Everything loaded at runtime, defined once, so the keep list below and the post-purge
    # check cannot drift apart.
    SITE_PACKAGES="$(python -c 'import sysconfig; print(sysconfig.get_path("platlib"))')"; \
    linked_libraries() { \
        ldd /usr/sbin/asterisk; \
        find /usr/lib/asterisk/modules -name '*.so' -exec ldd {} \; ; \
        find "$SITE_PACKAGES" -name '*.so' -exec ldd {} \; ; \
    }; \
    \
    # Reclaim the libraries nothing links any more. dpkg records paths under /usr/lib while ldd
    # reports some as /lib, so they are normalised or the purge takes libraries still in use.
    KEEP="$(linked_libraries 2>/dev/null \
            | awk '/=> \//{print $3}' \
            | sed 's|^/lib/|/usr/lib/|' \
            | sort -u \
            | xargs -r dpkg -S 2>/dev/null | cut -d: -f1 | sort -u)"; \
    apt-mark auto '.*' > /dev/null; \
    apt-mark manual $KEEP gettext-base ca-certificates > /dev/null; \
    apt-get purge -y build-essential wget; \
    apt-get autoremove -y --purge; \
    rm -rf /usr/src /usr/include /tmp/requirements.txt \
           /var/lib/apt/lists/* /var/cache/apt /root/.cache; \
    ldconfig; \
    \
    # Fail the build, not the first call, if the purge took something in use.
    if linked_libraries 2>/dev/null | grep -q 'not found'; then \
        echo "purge removed a library still in use" >&2; \
        exit 1; \
    fi; \
    \
    # Assert what must SURVIVE the trimming above: menuselect resolves dependencies silently, so
    # disabling one module can take an unrelated one — including the channel driver — with it.
    for module in \
        chan_pjsip res_pjsip res_pjsip_session res_pjsip_sdp_rtp res_pjsip_pubsub \
        res_pjsip_outbound_registration res_pjsip_authenticator_digest \
        res_pjsip_outbound_authenticator_digest res_pjsip_endpoint_identifier_ip \
        res_pjsip_dtmf_info res_pjsip_nat res_rtp_asterisk \
        res_ari res_ari_channels res_ari_events res_stasis app_stasis \
        app_amd app_playback codec_ulaw codec_alaw format_wav \
    ; do \
        if [ ! -e "/usr/lib/asterisk/modules/${module}.so" ]; then \
            echo "${module}.so is missing; the image cannot place a call" >&2; \
            exit 1; \
        fi; \
    done; \
    \
    # And what must NOT: menuselect is not fatal on a name it does not know, so a rename upstream
    # would leave a module we meant to drop quietly in the image.
    for module in res_pjsip_mwi res_pjsip_exten_state \
                  res_pjsip_publish_asterisk res_pjsip_outbound_publish; do \
        if [ -e "/usr/lib/asterisk/modules/${module}.so" ]; then \
            echo "${module} was not disabled; it parses inbound SIP this service never uses" >&2; \
            exit 1; \
        fi; \
    done; \
    echo "asterisk modules: $(ls /usr/lib/asterisk/modules/*.so | wc -l), all libraries present"

# The synthesizer writes here and Asterisk reads it. Same filesystem, so nothing
# has to be kept in sync. Mount a volume over it: see docker-compose.yml.
RUN mkdir -p /var/lib/asterisk/sounds/callu

COPY --from=voice-api-build /out/voice-api /usr/local/bin/voice-api

COPY tts/app/     /srv/tts/app/
COPY tts/tests/   /srv/tts/tests/
COPY tts/scripts/ /srv/tts/scripts/
COPY tts/rules/ /srv/tts/rules/

# The image contains GPLv2 software, so the licence and the written offer for its source have to
# travel inside the artifact — not only in the repository someone may never visit.
COPY LICENSE NOTICE /usr/share/doc/callu-voice/

# Rendered at start-up, so no credential is baked into the image.
COPY asterisk/etc/ /etc/asterisk.template/
COPY entrypoint.sh /entrypoint.sh
COPY supervisord.conf /etc/supervisord.conf

# supervisorctl needs a config to find its socket; point it at the same file.
ENV SUPERVISOR_CONF=/etc/supervisord.conf
RUN chmod +x /entrypoint.sh

# No .pyc at runtime either. Bytecode compiled on a build host names that host's paths in every
# traceback, and .dockerignore only keeps the build's copies out.
ENV PYTHONDONTWRITEBYTECODE=1

# Model weights (~400MB), downloaded on first run; mount a volume to keep them. Both variables
# are needed: the synthesizer resolves its own cache through SUPERTONIC_CACHE_DIR, not HF_HOME.
ENV HF_HOME=/cache
ENV SUPERTONIC_CACHE_DIR=/cache/supertonic3
ENV TTS_SOUNDS_DIR=/var/lib/asterisk/sounds/callu
ENV TTS_RULES_DIR=/srv/tts/rules

# Unprivileged: Asterisk parses untrusted SIP/RTP off the network, so it must not be uid 0. Its
# one privilege is the file capability set above; a mounted models volume inherits this ownership.
RUN groupadd -g 10001 callu \
 && useradd -u 10001 -g callu -m -d /home/callu -s /usr/sbin/nologin callu \
 && mkdir -p /cache /run/callu \
 && chown -R callu:callu \
      /etc/asterisk /var/lib/asterisk /var/log/asterisk /var/spool/asterisk \
      /var/run/asterisk /var/cache/asterisk /srv /cache /run/callu /home/callu

# HuggingFace/ONNX resolve ~ for their caches; without a real home they fail.
ENV HOME=/home/callu

USER callu:callu

# SIP, RTP, and the call API. ARI (8088) stays on localhost.
EXPOSE 5060/udp 5060/tcp 8090/tcp 10000-10099/udp

ENTRYPOINT ["/entrypoint.sh"]
CMD ["supervisord", "-c", "/etc/supervisord.conf"]
