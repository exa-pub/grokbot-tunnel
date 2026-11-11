# syntax=docker/dockerfile:1
# The environment of the comparison tests (see ../README.md).
#
#   base     bash, zsh, ripgrep, git and the user "tester": what both clients
#            run in. No desktop code; CI builds this target.
#   desktop  base plus the desktop client's bundle and Node, to run the
#            original next to grokbot-tunnel. Local only: the bundle is not
#            ours to publish.
#
# The desktop target takes the AppImage from the build context "appimage";
# run.sh passes it.

# Pinned by digest so that recordings and their checks see the same programs.
ARG UBUNTU=ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852
# The Node of the desktop's Electron runtime (process.versions.node).
ARG NODE=node:24.22.1-bookworm-slim@sha256:be8204d4bd327363357f532187b752ff62759c31bedf98e9e18fc08c9cbd605c

FROM ${UBUNTU} AS base
ARG UBUNTU
RUN apt-get update \
	&& apt-get install -y --no-install-recommends bash zsh ripgrep git procps ca-certificates \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd -m -s /bin/bash tester \
	&& mkdir /work && chown tester:tester /work
# Tests refuse to start a client outside this image.
ENV GROKBOT_COMPARE_IMAGE=base GROKBOT_COMPARE_BASE=${UBUNTU} COMPARE_OUT=/work/out LANG=C.UTF-8
USER tester
WORKDIR /work

FROM ${NODE} AS node

# Unpacks the AppImage without running it.
FROM ${UBUNTU} AS bundle
RUN apt-get update \
	&& apt-get install -y --no-install-recommends squashfs-tools python3 \
	&& rm -rf /var/lib/apt/lists/*
ARG DESKTOP_VERSION
ARG DESKTOP_SHA256
COPY unpack.py /unpack.py
RUN --mount=type=bind,from=appimage,target=/appimage \
	python3 /unpack.py "/appimage/Grok_Bot_${DESKTOP_VERSION}.AppImage" "${DESKTOP_SHA256}" /desktop

FROM base AS desktop
USER root
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=bundle /desktop /opt/desktop
ARG DESKTOP_VERSION
ENV GROKBOT_COMPARE_IMAGE=desktop GROKBOT_COMPARE_DESKTOP=/opt/desktop/app/dist GROKBOT_COMPARE_DESKTOP_VERSION=${DESKTOP_VERSION}
USER tester
