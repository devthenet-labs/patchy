#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Check the trees `patchy init app` generates the way their own CI and a
# reviewer would: actionlint over the workflows (with shellcheck on their run
# blocks), shellcheck over the publish scripts, hadolint over the
# Dockerfiles, the guard's node tests and the publishers' python tests, and
# gofmt, vet and race-enabled tests of a generated Go application.
#
# With --docker it also builds what a tree's CI builds, for a tree generated
# with real images (the golden trees pin documentation digests no registry
# holds, so --docker needs trees named): the runtime image, validated as the
# publisher validates it and then run the way a preview runs it (uid 65532,
# a read-only root filesystem, no capabilities) until its readiness path
# answers; and the agent image, twice with the agent workflow's
# reproducibility settings, failing unless both builds have one digest.
#
# Usage: hack/scaffold-check.sh [--docker] [tree...]
# With no tree, the golden trees under cmd/patchy/internal/scaffold/testdata.
# READINESS_PATH (default /healthz) is the path the runtime image must answer.
set -euo pipefail

docker_build=false
if [[ "${1:-}" == "--docker" ]]; then
  docker_build=true
  shift
  if [[ $# -eq 0 ]]; then
    echo "scaffold-check: --docker needs trees generated with real images; the golden trees pin documentation digests" >&2
    exit 2
  fi
fi
if [[ $# -eq 0 ]]; then
  set -- cmd/patchy/internal/scaffold/testdata/golden/*/
fi
readiness=${READINESS_PATH:-/healthz}
# The digest of the one image in an OCI layout tarball.
oci_digest() {
  tar -xOf "$1" index.json | python3 -c 'import json, sys; print(json.load(sys.stdin)["manifests"][0]["digest"])'
}

# Run the runtime image in tree . as a preview runs it, and wait for its
# readiness path to answer.
run_runtime() {
  local tag=$1 port container host code="" ok=false
  port=$(awk 'toupper($1) == "EXPOSE" { sub("/.*", "", $2); print $2; exit }' Dockerfile)
  [[ "$port" =~ ^[0-9]+$ ]] || { echo "scaffold-check: Dockerfile EXPOSEs no port" >&2; return 1; }
  container=$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges \
    --user 65532:65532 -p "127.0.0.1::$port" "$tag")
  host=$(docker port "$container" "$port/tcp" | head -n 1)
  for _ in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://$host$readiness" || true)
    if [[ "$code" == 200 ]]; then
      ok=true
      break
    fi
    sleep 1
  done
  if ! $ok; then
    echo "scaffold-check: $readiness answered ${code:-nothing}, not 200" >&2
    docker logs "$container" >&2 || true
  fi
  docker rm -f "$container" > /dev/null
  $ok && echo "scaffold-check: runtime image answers $readiness read-only as uid 65532"
}

for tree in "$@"; do
  tree=${tree%/}
  echo "scaffold-check: $tree"
  # Each tree is checked in a copy that is a git repository of its own:
  # actionlint resolves ./.github/workflows/ calls against the enclosing
  # repository's root, and nothing the checks write lands in the tree.
  work=$(mktemp -d)
  out=$(mktemp -d)
  cp -R "$tree"/. "$work"/
  git -C "$work" init -q
  (
    cd "$work"
    trap 'rm -rf "$work" "$out"' EXIT
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
    if $docker_build; then
      if [[ -f Dockerfile ]]; then
        docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
          --output "type=oci,dest=$out/runtime.oci.tar" .
        python3 "$publish/validate_oci.py" "$out/runtime.oci.tar" "$out/runtime-validated" runtime
        tag="scaffold-check-runtime:$$"
        docker buildx build --platform linux/amd64 --provenance=false --sbom=false --load -t "$tag" .
        run_runtime "$tag"
        docker image rm "$tag" > /dev/null
      fi
      for n in 1 2; do
        SOURCE_DATE_EPOCH=0 docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
          --no-cache -f .patchy/Dockerfile --output "type=oci,dest=$out/agent-$n.oci.tar,rewrite-timestamp=true" .
      done
      python3 "$publish/validate_oci.py" "$out/agent-1.oci.tar" "$out/agent-validated" agent
      first=$(oci_digest "$out/agent-1.oci.tar")
      second=$(oci_digest "$out/agent-2.oci.tar")
      [[ "$first" == "$second" ]] ||
        { echo "scaffold-check: agent image is not reproducible: $first != $second" >&2; exit 1; }
      echo "scaffold-check: agent image rebuilds reproducibly as $first"
    fi
  )
done
