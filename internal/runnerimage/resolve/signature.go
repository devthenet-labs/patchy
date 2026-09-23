// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

const (
	// bundleMediaTypePrefix is the sigstore bundle media type family; the
	// referrer's artifactType and the bundle's own mediaType both carry it
	// (cosign v3 writes application/vnd.dev.sigstore.bundle.v0.3+json).
	bundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"
	// legacySignatureAnnotation carries the base64 signature on each layer of
	// a legacy sha256-<digest>.sig manifest.
	legacySignatureAnnotation = "dev.cosignproject.cosign/signature"
	// legacySignatureType is the simple-signing payload's critical.type.
	legacySignatureType = "cosign container image signature"
	// cosignSignPredicate is the in-toto predicateType cosign v3 puts in the
	// DSSE statement of an image signature (as opposed to an attestation).
	cosignSignPredicate = "https://sigstore.dev/cosign/sign/v1"
	// emptyConfigMediaType is the OCI empty descriptor cosign uses as the
	// config of a bundle artifact.
	emptyConfigMediaType = "application/vnd.oci.empty.v1+json"
	// maxSignatureBytes caps a bundle or payload blob read from the registry.
	maxSignatureBytes = 1 << 20
	// maxReferrers bounds how many bundle candidates among the referrers are
	// examined.
	maxReferrers = 64
	// maxBundleLayers bounds the bundle layers read from one referrer;
	// cosign writes one.
	maxBundleLayers = 4
	// maxLegacySignatures bounds the signature layers read from a legacy
	// sha256-<digest>.sig manifest; cosign appends one per signing.
	maxLegacySignatures = 32
)

// ParsePublicKey reads the operator's cosign public key: a PEM-encoded PKIX
// ECDSA key, which is what cosign generate-key-pair writes.
func ParsePublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("cosign public key: no PEM block found")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cosign public key: %w", err)
	}
	ec, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cosign public key: %T is not an ECDSA key", key)
	}
	return ec, nil
}

// search accumulates what one signature verification saw across both
// forms.
type search struct {
	// examined counts the signature blobs read and judged.
	examined int
	// transient is the first lookup the registry or the network failed.
	transient error
	// denied is the first lookup the registry refused (401/403).
	denied error
}

// lookup records one failed lookup. A candidate is anything anyone with push
// access attaches, so a deterministic failure (not found, denied, oversized,
// unparseable) only means that candidate does not verify, never a verdict
// on the image; a transient one is remembered, since the candidate might
// have been the valid signature.
func (s *search) lookup(err error) {
	var rej *runnerimage.Rejection
	switch {
	case transientLookup(err):
		if s.transient == nil {
			s.transient = err
		}
	case errors.As(err, &rej) && rej.Reason == "AccessDenied":
		if s.denied == nil {
			s.denied = err
		}
	}
}

// transientLookup reports whether a failed lookup is the registry or the
// network failing, worth a retry: a 5xx or 429, a network error, a deadline
// or a truncated body. Anything else is about the content and deterministic.
func transientLookup(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode >= http.StatusInternalServerError || te.StatusCode == http.StatusTooManyRequests
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// verifySignature checks that the recorded digest carries a signature by
// the operator's key, bundle form first, legacy tag second; a candidate that
// cannot be fetched or read is skipped, so one junk or dangling referrer
// never hides a valid signature. It returns a *runnerimage.Rejection when
// nothing verifies, and a plain error when a lookup failed transiently and
// nothing else verified (the failed candidate might have been the one).
func (r *Resolver) verifySignature(ctx context.Context, repo name.Repository, digest v1.Hash) error {
	pinned := repo.Digest(digest.String())
	var s search
	if r.verifyBundles(ctx, pinned, digest, &s) || r.verifyLegacy(ctx, repo, digest, &s) {
		return nil
	}
	switch {
	case s.transient != nil:
		return s.transient
	case s.examined == 0 && s.denied != nil:
		return s.denied
	case s.examined == 0:
		return &runnerimage.Rejection{Reason: "Unsigned",
			Message: fmt.Sprintf("image `%s` carries no signature by the operator's cosign key", pinned)}
	}
	return &runnerimage.Rejection{Reason: "SignatureInvalid",
		Message: fmt.Sprintf("image `%s` carries %d signature(s), none of which verifies with the operator's "+
			"cosign key", pinned, s.examined)}
}

// verifyBundles walks the referrers of the pinned digest (the Referrers API,
// or the sha256-<digest> tag when the registry lacks it) and verifies the
// sigstore bundles of up to maxReferrers candidates, reporting whether one
// verified.
func (r *Resolver) verifyBundles(ctx context.Context, pinned name.Digest, digest v1.Hash, s *search) bool {
	idx, err := remote.Referrers(pinned, r.options(ctx)...)
	if err != nil {
		s.lookup(classify(pinned.String(), err))
		return false
	}
	im, err := idx.IndexManifest()
	if err != nil {
		s.lookup(fmt.Errorf("referrers of %s: %w", pinned, err))
		return false
	}
	candidates := 0
	for _, d := range im.Manifests {
		if !bundleCandidate(d) {
			continue
		}
		if candidates++; candidates > maxReferrers {
			break
		}
		if r.verifyReferrer(ctx, pinned.Context().Digest(d.Digest.String()), digest, s) {
			return true
		}
	}
	return false
}

// verifyReferrer reads up to maxBundleLayers sigstore bundle layers of one
// referrer, judging each as it is read so at most one blob is held at a
// time, and reports whether one verified.
func (r *Resolver) verifyReferrer(ctx context.Context, ref name.Digest, digest v1.Hash, s *search) bool {
	img, err := remote.Image(ref, r.options(ctx)...)
	if err != nil {
		s.lookup(classify(ref.String(), err))
		return false
	}
	m, err := img.Manifest()
	if err != nil {
		s.lookup(classify(ref.String(), err))
		return false
	}
	read := 0
	for _, desc := range m.Layers {
		if !strings.HasPrefix(string(desc.MediaType), bundleMediaTypePrefix) {
			continue
		}
		if read++; read > maxBundleLayers {
			break
		}
		data, err := r.blob(ctx, ref.Context().Digest(desc.Digest.String()))
		if err != nil {
			s.lookup(err)
			continue
		}
		s.examined++
		if verifyBundle(data, r.cfg.PublicKey, digest) == nil {
			return true
		}
	}
	return false
}

// bundleCandidate reports whether a referrer might hold a bundle. The
// listing's artifactType is the manifest's own artifactType per the
// distribution spec, but a registry may report the config media type
// instead (cosign writes the OCI empty config, and ggcr's registry does
// exactly that), so those and an absent type are examined too; the layer
// media types decide.
func bundleCandidate(d v1.Descriptor) bool {
	switch {
	case strings.HasPrefix(d.ArtifactType, bundleMediaTypePrefix):
		return true
	case d.ArtifactType == "", d.ArtifactType == emptyConfigMediaType:
		return d.MediaType.IsImage()
	}
	return false
}

// verifyLegacy checks the sha256-<digest>.sig tag: each layer's blob is a
// simple-signing payload naming the digest, its annotation the signature.
// Up to maxLegacySignatures layers are read, one at a time.
func (r *Resolver) verifyLegacy(ctx context.Context, repo name.Repository, digest v1.Hash, s *search) bool {
	tag := repo.Tag(strings.Replace(digest.String(), ":", "-", 1) + ".sig")
	img, err := remote.Image(tag, r.options(ctx)...)
	if err != nil {
		if !isStatus(err, http.StatusNotFound) {
			s.lookup(classify(tag.String(), err))
		}
		return false
	}
	m, err := img.Manifest()
	if err != nil {
		if !isStatus(err, http.StatusNotFound) {
			s.lookup(classify(tag.String(), err))
		}
		return false
	}
	read := 0
	for _, desc := range m.Layers {
		sig, ok := desc.Annotations[legacySignatureAnnotation]
		if !ok {
			continue
		}
		if read++; read > maxLegacySignatures {
			break
		}
		payload, err := r.blob(ctx, repo.Digest(desc.Digest.String()))
		if err != nil {
			s.lookup(err)
			continue
		}
		s.examined++
		if verifyLegacyPayload(payload, sig, r.cfg.PublicKey, digest) == nil {
			return true
		}
	}
	return false
}

// blob reads one blob by digest, capped at maxSignatureBytes.
func (r *Resolver) blob(ctx context.Context, ref name.Digest) ([]byte, error) {
	layer, err := remote.Layer(ref, r.options(ctx)...)
	if err != nil {
		return nil, classify(ref.String(), err)
	}
	rc, err := layer.Compressed()
	if err != nil {
		return nil, classify(ref.String(), err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxSignatureBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref, err)
	}
	if len(data) > maxSignatureBytes {
		return nil, fmt.Errorf("read %s: blob exceeds %d bytes", ref, maxSignatureBytes)
	}
	return data, nil
}

// bundle is the slice of a sigstore bundle (protojson encoding) verification
// reads: the DSSE envelope cosign v3 writes for image signatures, or the
// message-signature form of the bundle v0.3 specification.
type bundle struct {
	MediaType        string `json:"mediaType"`
	MessageSignature *struct {
		MessageDigest struct {
			Algorithm string `json:"algorithm"`
			Digest    []byte `json:"digest"`
		} `json:"messageDigest"`
		Signature []byte `json:"signature"`
	} `json:"messageSignature"`
	DSSEEnvelope *struct {
		Payload     []byte `json:"payload"`
		PayloadType string `json:"payloadType"`
		Signatures  []struct {
			Sig []byte `json:"sig"`
		} `json:"signatures"`
	} `json:"dsseEnvelope"`
}

// statement is the slice of an in-toto statement verification reads.
type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
}

// verifyBundle verifies one sigstore bundle against the key and the
// recorded digest. A DSSE envelope must carry an in-toto statement whose
// subject names the digest and whose predicate is cosign's image-signature
// predicate, with a signature over the DSSE pre-authentication encoding; a
// message signature must carry the digest itself as the message digest, the
// signature being over that digest.
func verifyBundle(data []byte, pub *ecdsa.PublicKey, digest v1.Hash) error {
	var b bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if !strings.HasPrefix(b.MediaType, bundleMediaTypePrefix) {
		return fmt.Errorf("bundle: media type %q is not a sigstore bundle", b.MediaType)
	}
	switch {
	case b.DSSEEnvelope != nil:
		env := b.DSSEEnvelope
		pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(env.PayloadType), env.PayloadType, len(env.Payload), env.Payload)
		sum := sha256.Sum256([]byte(pae))
		signed := false
		for _, s := range env.Signatures {
			if ecdsa.VerifyASN1(pub, sum[:], s.Sig) {
				signed = true
				break
			}
		}
		if !signed {
			return errors.New("bundle: DSSE signature does not verify")
		}
		return statementNames(env.Payload, digest)
	case b.MessageSignature != nil:
		ms := b.MessageSignature
		if ms.MessageDigest.Algorithm != "SHA2_256" {
			return fmt.Errorf("bundle: message digest algorithm %q is not SHA2_256", ms.MessageDigest.Algorithm)
		}
		want, err := hex.DecodeString(digest.Hex)
		if err != nil || digest.Algorithm != "sha256" || !bytes.Equal(ms.MessageDigest.Digest, want) {
			return errors.New("bundle: message digest is not the image digest")
		}
		if !ecdsa.VerifyASN1(pub, ms.MessageDigest.Digest, ms.Signature) {
			return errors.New("bundle: message signature does not verify")
		}
		return nil
	default:
		return errors.New("bundle: neither dsseEnvelope nor messageSignature")
	}
}

// statementNames checks that a signed in-toto statement is cosign's image
// signature for digest.
func statementNames(payload []byte, digest v1.Hash) error {
	var st statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return fmt.Errorf("bundle: statement: %w", err)
	}
	if st.Type != "https://in-toto.io/Statement/v1" && st.Type != "https://in-toto.io/Statement/v0.1" {
		return fmt.Errorf("bundle: statement type %q is not in-toto", st.Type)
	}
	if st.PredicateType != cosignSignPredicate {
		return fmt.Errorf("bundle: predicate %q is not an image signature", st.PredicateType)
	}
	for _, s := range st.Subject {
		if s.Digest[digest.Algorithm] == digest.Hex {
			return nil
		}
	}
	return errors.New("bundle: statement subject is not the image digest")
}

// simpleSigning is the slice of the legacy payload verification reads.
type simpleSigning struct {
	Critical struct {
		Type  string `json:"type"`
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
	} `json:"critical"`
}

// verifyLegacyPayload verifies one legacy signature: the payload must be a
// simple-signing document naming digest, and the base64 DER signature must
// verify over the payload's SHA-256.
func verifyLegacyPayload(payload []byte, sig string, pub *ecdsa.PublicKey, digest v1.Hash) error {
	var ss simpleSigning
	if err := json.Unmarshal(payload, &ss); err != nil {
		return fmt.Errorf("legacy signature: %w", err)
	}
	if ss.Critical.Type != legacySignatureType || ss.Critical.Image.DockerManifestDigest != digest.String() {
		return errors.New("legacy signature: payload does not name the image digest")
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("legacy signature: %w", err)
	}
	sum := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(pub, sum[:], raw) {
		return errors.New("legacy signature: does not verify")
	}
	return nil
}
