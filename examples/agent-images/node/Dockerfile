# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Node agent image: patchy's agent-base plus Node.js and the project's
# node_modules, so the agent can build and test with no network. Build it
# with the repository root as the context (it needs package.json and
# package-lock.json):
#
#   docker build -f examples/agent-images/node/Dockerfile -t <registry>/<repo>-agent:<tag> .
#
# Rebuild whenever the lockfile changes: npm cannot fetch in the pod, so the
# agent runs `npm test` (or the tools directly), never `npm ci`.

# The base, pinned to a patchy release (release-please stamps the version).
# Pin its digest as well once you have built against it.
# x-release-please-start-version
ARG AGENT_BASE=ghcr.io/devthenet-labs/patchy/agent-base:v0.12.5
# x-release-please-end
FROM ${AGENT_BASE}

# apk needs root (uid 0); the image drops back to 65532 at the end (the pod
# runs as 65532 whatever the image says).
USER 0:0

# Node.js (the current LTS line) and npm, glibc builds from wolfi.
# hadolint ignore=DL3018
RUN apk add --no-cache nodejs-24 npm

# Install exactly what the lockfile pins, then move node_modules to /: Node
# resolves a package by walking up from the importing file's directory, and /
# is the one ancestor of /workspace/repo that the pod does not cover with an
# emptyDir. So require(), import, TypeScript's @types lookup and npm's
# node_modules/.bin all find the baked tree without NODE_PATH (which ES
# modules ignore) or NODE_OPTIONS (which the pod blanks).
WORKDIR /tmp/deps
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund \
  && mv node_modules /node_modules \
  && rm -rf /tmp/deps /root/.npm

# The dependencies' CLIs (jest, tsc, eslint, ...) callable by name.
ENV PATH=/node_modules/.bin:${PATH}

# Offline from here on: npm uses its cache only (and so fails fast rather than
# hanging on a fetch), makes no audit, funding or update-check calls, and
# keeps that cache in the writable /tmp.
ENV npm_config_offline=true \
    npm_config_audit=false \
    npm_config_fund=false \
    npm_config_update_notifier=false \
    npm_config_cache=/tmp/.cache/npm

# Describe your image, not the base (labels are inherited).
LABEL org.opencontainers.image.source="https://github.com/your-org/your-repo" \
      org.opencontainers.image.description="patchy agent image: Node.js 24 with the project's node_modules in /node_modules"

WORKDIR /workspace
USER 65532:65532
