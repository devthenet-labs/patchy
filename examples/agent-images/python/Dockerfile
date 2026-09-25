# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Python agent image: patchy's agent-base plus Python and a virtualenv with
# the project's dependencies installed, so the agent can run the tests with
# no network. Build it with the repository root as the context (it needs
# requirements.txt; pin versions there, or use a lock such as
# `pip-compile --generate-hashes` output):
#
#   docker build -f examples/agent-images/python/Dockerfile -t <registry>/<repo>-agent:<tag> .
#
# Rebuild whenever requirements.txt changes: pip cannot fetch in the pod.

# The base, pinned to a patchy release (release-please stamps the version).
# Pin its digest as well once you have built against it.
# x-release-please-start-version
ARG AGENT_BASE=ghcr.io/devthenet-labs/patchy/agent-base:v0.12.3
# x-release-please-end
FROM ${AGENT_BASE}

# apk needs root (uid 0); the image drops back to 65532 at the end (the pod
# runs as 65532 whatever the image says).
USER 0:0

# Wolfi's glibc CPython, which ships venv and ensurepip.
# hadolint ignore=DL3018
RUN apk add --no-cache python-3.13

# The virtualenv sits outside HOME and /tmp, which are emptyDirs in the pod,
# so it survives into it (read-only there). Putting its bin first on PATH is
# what activating it means; source-controller keeps the entry.
ENV VIRTUAL_ENV=/opt/venv
ENV PATH=${VIRTUAL_ENV}/bin:${PATH}
RUN python3.13 -m venv "${VIRTUAL_ENV}"

# Install the dependencies (and nothing else from the repository) into it.
WORKDIR /tmp/deps
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt \
  && rm -rf /tmp/deps

# Offline from here on: pip never contacts an index and skips its version
# check. No bytecode is written, so the agent's changeset never picks up
# __pycache__ directories next to the repository's sources (the venv's own
# bytecode was compiled at install time and is still used).
ENV PIP_NO_INDEX=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1

# Describe your image, not the base (labels are inherited).
LABEL org.opencontainers.image.source="https://github.com/your-org/your-repo" \
      org.opencontainers.image.description="patchy agent image: Python 3.13 with the project's dependencies in /opt/venv"

WORKDIR /workspace
USER 65532:65532
