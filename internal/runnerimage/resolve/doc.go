// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package resolve is the registry-backed runnerimage.Resolver: it turns a
// declared, allowlisted reference into the digest-pinned, checked image a
// Job may run, over go-containerregistry.
//
// The order is what makes pin-once hold. A tag costs exactly one HEAD; from
// then on every call names the repository by digest, so nothing after that
// HEAD can observe a tag move, and the recorded reference is the object
// that was checked. For an image index every child a linux/amd64 or
// linux/arm64 node could run, judged the way containerd picks one (other
// spellings of those architectures, an empty OS), is enumerated by its own
// digest and checked (compressed layer size, a config blob of at most
// 4 MiB, os/arch, VOLUME, reserved ENV, PATH); every runnable child must
// pass with the same sanitized PATH, and the index digest is what is
// recorded, because that is what cosign signs and the kubelet pulls. An
// index that leaves a node an unchecked fallback (linux/386, linux/arm/*,
// an entry with no platform) is rejected. A single-platform manifest is
// checked the same way, with its os/arch read from the config.
//
// Signature verification is in-process with stdlib crypto: the sigstore
// bundle cosign v3 attaches through the OCI referrers API (with the
// sha256-<digest> tag fallback) is checked first, then the legacy
// sha256-<digest>.sig tag with its simple-signing payload. Both must name
// the recorded digest and verify with the operator's ECDSA key. Anyone who
// can push can attach a referrer, so one that cannot be fetched or read is
// a candidate that does not verify, never a verdict on the image; the
// candidates, the bundle layers of each and the legacy signature layers are
// capped and read one blob at a time.
//
// Registry credentials come from a host-selected keychain (NewKeychain):
// ECR through the AWS SDK's default credential chain, Artifact Registry and
// GCR through Application Default Credentials, everything else through the
// docker config under DOCKER_CONFIG, anonymous last. A cloud credential
// failure is transient, never anonymous. A 401, 403 or 404 is a
// *runnerimage.Rejection; anything else is transient and left to the
// caller's backoff.
//
// A verdict is cached by pinned reference for a bounded time so per-Finding
// Repositories on the same image do not repeat the checks; the HEAD that
// resolves a tag is never cached.
package resolve
