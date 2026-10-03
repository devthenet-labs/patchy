#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Check the trees `patchy init app` generates the way their own CI and a
# reviewer would: actionlint over the workflows (with shellcheck on their run
# blocks), shellcheck over the publish scripts, hadolint over the
# Dockerfiles, the guard's node tests and the publishers' python tests, and
# gofmt, vet and race-enabled tests of a generated Go application. With
# --docker it also builds every Dockerfile of a tree that has an application
# (the agent image needs its go.mod), twice for the agent image with the
# generated workflow's reproducibility settings, and fails unless both
# builds have the same digest.
#
# Usage: hack/scaffold-check.sh [--docker] [tree...]
# With no tree, the golden trees under cmd/patchy/internal/scaffold/testdata.
set -euo pipefail

docker_build=false
if [[ "${1:-}" == "--docker" ]]; then
  docker_build=true
  shift
fi
if [[ $# -eq 0 ]]; then
  set -- cmd/patchy/internal/scaffold/testdata/golden/*/
fi

# The digest of the one image in an OCI layout tarball.
oci_digest() {
  tar -xOf "$1" index.json | python3 -c 'import json, sys; print(json.load(sys.stdin)["manifests"][0]["digest"])'
}

for tree in "$@"; do
  tree=${tree%/}
  echo "scaffold-check: $tree"
  (
    cd "$tree"
    publish=.github/actions/publish
    actionlint .github/workflows/*.yml
    shellcheck "$publish"/*.sh
    for df in Dockerfile .patchy/Dockerfile; do
      if [[ -f "$df" ]]; then hadolint "$df"; fi
    done
    node --test "$publish/guard.test.cjs"
    python3 -m unittest discover -s "$publish" -p 'test_*.py'
    if [[ -f go.mod ]]; then
      unformatted=$(gofmt -l .)
      [[ -z "$unformatted" ]] || { echo "scaffold-check: not gofmt'd: $unformatted" >&2; exit 1; }
      GOWORK=off go vet ./...
      GOWORK=off go test -race ./...
    fi
    if $docker_build && [[ -f go.mod ]]; then
      out=$(mktemp -d)
      trap 'rm -rf "$out"' EXIT
      docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
        --output "type=oci,dest=$out/runtime.oci.tar" .
      for n in 1 2; do
        SOURCE_DATE_EPOCH=0 docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
          --no-cache -f .patchy/Dockerfile --output "type=oci,dest=$out/agent-$n.oci.tar,rewrite-timestamp=true" .
      done
      first=$(oci_digest "$out/agent-1.oci.tar")
      second=$(oci_digest "$out/agent-2.oci.tar")
      [[ "$first" == "$second" ]] ||
        { echo "scaffold-check: agent image is not reproducible: $first != $second" >&2; exit 1; }
      echo "scaffold-check: agent image rebuilds reproducibly as $first"
    fi
  )
done
