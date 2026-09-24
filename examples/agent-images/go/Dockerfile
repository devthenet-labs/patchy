# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Go agent image: patchy's agent-base plus a Go toolchain and the module's
# dependencies, so the agent can build and test with no network. Build it
# with the repository root as the context (it needs go.mod and go.sum):
#
#   docker build -f examples/agent-images/go/Dockerfile -t <registry>/<repo>-agent:<tag> .
#
# Rebuild whenever go.mod changes: a module missing from the baked cache fails
# the agent's build with "module lookup disabled by GOPROXY=off".

# The base, pinned to a patchy release (release-please stamps the version).
# Pin its digest as well once you have built against it.
# x-release-please-start-version
ARG AGENT_BASE=ghcr.io/devthenet-labs/patchy/agent-base:v0.12.1
# x-release-please-end
FROM ${AGENT_BASE}

# apk needs root (uid 0); the image drops back to 65532 at the end (the pod
# runs as 65532 whatever the image says).
USER 0:0

# The toolchain go.mod's go directive asks for (1.24 for patchy-target). Wolfi
# builds it against glibc, puts go on /usr/bin, and pulls in build-base, so
# cgo works too.
# hadolint ignore=DL3018
RUN apk add --no-cache go-1.24

# GOTOOLCHAIN=local: never fetch a newer toolchain for a go or toolchain line
# the one above cannot satisfy; fail fast instead of trying the network.
# GOMODCACHE sits outside HOME and /tmp, which are emptyDirs in the pod, so
# the baked cache survives into it (read-only there, as Go leaves it anyway).
ENV GOTOOLCHAIN=local \
    GOMODCACHE=/opt/go/pkg/mod

# Bake the module cache from go.mod and go.sum alone: the layer is rebuilt only
# when dependencies change, and no source code enters the image. The build
# cache this creates is dropped; in the pod Go's build cache lives under the
# base's XDG_CACHE_HOME (/tmp/.cache/go-build), which is writable.
WORKDIR /tmp/deps
COPY go.mod go.sum* ./
RUN go mod download \
  && rm -rf /tmp/deps "${XDG_CACHE_HOME}"

# Offline from here on. GOPROXY=off resolves modules from the baked cache
# only; -mod=mod lets go build and go test record a requirement the agent's
# change adds, provided the cache already holds that module.
ENV GOPROXY=off \
    GOFLAGS=-mod=mod

# Describe your image, not the base (labels are inherited).
LABEL org.opencontainers.image.source="https://github.com/your-org/your-repo" \
      org.opencontainers.image.description="patchy agent image: Go 1.24 with the module cache baked in"

WORKDIR /workspace
USER 65532:65532
