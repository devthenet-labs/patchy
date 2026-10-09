// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

func TestNormalizePlatform(t *testing.T) {
	cases := []struct {
		in, want v1.Platform
	}{
		{v1.Platform{Architecture: "x86_64"}, v1.Platform{OS: "linux", Architecture: "amd64"}},
		{v1.Platform{OS: "Linux", Architecture: "X86-64", Variant: "v1"}, v1.Platform{OS: "linux", Architecture: "amd64"}},
		{v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"},
			v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}},
		{v1.Platform{OS: "linux", Architecture: "aarch64", Variant: "8"}, v1.Platform{OS: "linux", Architecture: "arm64"}},
		{v1.Platform{OS: "linux", Architecture: "ARM64", Variant: "V8"}, v1.Platform{OS: "linux", Architecture: "arm64"}},
		{v1.Platform{OS: "linux", Architecture: "i386", Variant: "x"}, v1.Platform{OS: "linux", Architecture: "386"}},
		{v1.Platform{OS: "linux", Architecture: "armhf"}, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}},
		{v1.Platform{OS: "linux", Architecture: "armel"}, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}},
		{v1.Platform{OS: "windows", Architecture: "s390x"}, v1.Platform{OS: "windows", Architecture: "s390x"}},
	}
	for _, c := range cases {
		got := normalizePlatform(c.in)
		if got.OS != c.want.OS || got.Architecture != c.want.Architecture || got.Variant != c.want.Variant {
			t.Errorf("normalizePlatform(%+v) = %+v, want %+v", c.in, got, c.want)
		}
		// Normalising is idempotent.
		again := normalizePlatform(got)
		if again.OS != got.OS || again.Architecture != got.Architecture || again.Variant != got.Variant {
			t.Errorf("normalizePlatform not idempotent on %+v: %+v", got, again)
		}
	}
}

func TestParsePublicKeyErrors(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	good := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if got, err := ParsePublicKey(good); err != nil || !got.Equal(&ec.PublicKey) {
		t.Fatalf("ParsePublicKey(ecdsa) = %v, %v", got, err)
	}

	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rder, err := x509.MarshalPKIXPublicKey(&rk.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		in   []byte
		want string
	}{
		"no PEM":    {[]byte("not a key"), "no PEM block"},
		"bad DER":   {pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1, 2, 3}}), "cosign public key:"},
		"RSA key":   {pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rder}), "is not an ECDSA key"},
		"empty PEM": {nil, "no PEM block"},
	}
	for name, c := range cases {
		if _, err := ParsePublicKey(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ParsePublicKey = %v, want %q", name, err, c.want)
		}
	}
}

func testDigest() v1.Hash {
	return v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("ab", 32)}
}

func TestStatementNames(t *testing.T) {
	d := testDigest()
	stmt := func(typ, pred, hex string) []byte {
		raw, _ := json.Marshal(map[string]any{
			"_type": typ, "predicateType": pred,
			"subject": []map[string]any{{"digest": map[string]string{"sha256": hex}}},
		})
		return raw
	}
	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"v1 statement", stmt("https://in-toto.io/Statement/v1", cosignSignPredicate, d.Hex), ""},
		{"v0.1 statement", stmt("https://in-toto.io/Statement/v0.1", cosignSignPredicate, d.Hex), ""},
		{"not JSON", []byte("{"), "bundle: statement:"},
		{"not in-toto", stmt("https://example.com/x", cosignSignPredicate, d.Hex), "is not in-toto"},
		{"an attestation", stmt("https://in-toto.io/Statement/v1", "https://slsa.dev/provenance/v1", d.Hex),
			"is not an image signature"},
		{"another image", stmt("https://in-toto.io/Statement/v1", cosignSignPredicate, strings.Repeat("cd", 32)),
			"subject is not the image digest"},
	}
	for _, c := range cases {
		err := statementNames(c.payload, d)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: statementNames = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestVerifyLegacyPayload(t *testing.T) {
	d := testDigest()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := func(typ, digest string) []byte {
		raw, _ := json.Marshal(map[string]any{"critical": map[string]any{
			"type": typ, "image": map[string]string{"docker-manifest-digest": digest}}})
		return raw
	}
	sign := func(p []byte, k *ecdsa.PrivateKey) string {
		sum := sha256.Sum256(p)
		sig, err := ecdsa.SignASN1(rand.Reader, k, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	}
	good := payload(legacySignatureType, d.String())
	cases := []struct {
		name    string
		payload []byte
		sig     string
		want    string
	}{
		{"valid", good, sign(good, key), ""},
		{"not JSON", []byte("{"), sign(good, key), "legacy signature:"},
		{"wrong type", payload("something else", d.String()), sign(good, key), "does not name the image digest"},
		{"wrong digest", payload(legacySignatureType, "sha256:"+strings.Repeat("0", 64)), sign(good, key),
			"does not name the image digest"},
		{"signature not base64", good, "!!!", "legacy signature:"},
		{"signed by another key", good, sign(good, other), "does not verify"},
	}
	for _, c := range cases {
		err := verifyLegacyPayload(c.payload, c.sig, &key.PublicKey, d)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: verifyLegacyPayload = %v, want %q", c.name, err, c.want)
		}
	}
}

type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

var _ net.Error = timeoutNetErr{}

// TestSearchLookup: a transient failure and an access refusal are each
// remembered once (the first wins); a deterministic one is only a candidate
// that did not verify.
func TestSearchLookup(t *testing.T) {
	transient := []error{
		&transport.Error{StatusCode: http.StatusBadGateway},
		&transport.Error{StatusCode: http.StatusTooManyRequests},
		timeoutNetErr{},
		context.DeadlineExceeded,
		context.Canceled,
		fmt.Errorf("read: %w", io.ErrUnexpectedEOF),
	}
	for _, err := range transient {
		if !transientLookup(err) {
			t.Errorf("transientLookup(%v) = false", err)
		}
	}
	for _, err := range []error{&transport.Error{StatusCode: http.StatusNotFound}, errors.New("bad json")} {
		if transientLookup(err) {
			t.Errorf("transientLookup(%v) = true", err)
		}
	}

	var s search
	first := &transport.Error{StatusCode: http.StatusServiceUnavailable}
	s.lookup(first)
	s.lookup(&transport.Error{StatusCode: http.StatusBadGateway})
	if s.transient != first {
		t.Errorf("transient = %v, want the first", s.transient)
	}
	denied := &runnerimage.Rejection{Reason: "AccessDenied", Message: "403"}
	s.lookup(denied)
	s.lookup(&runnerimage.Rejection{Reason: "AccessDenied", Message: "401"})
	if s.denied != denied {
		t.Errorf("denied = %v, want the first", s.denied)
	}
	var quiet search
	quiet.lookup(&runnerimage.Rejection{Reason: "NotFound"})
	quiet.lookup(errors.New("unparseable"))
	if quiet.transient != nil || quiet.denied != nil {
		t.Errorf("deterministic failures recorded: %+v", quiet)
	}
}

// isolateAWS points the AWS SDK at empty shared files, off the instance
// metadata service, with static environment credentials and ECR's endpoint
// at url.
func isolateAWS(t *testing.T, url string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": empty, "AWS_SHARED_CREDENTIALS_FILE": empty, "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_PROFILE": "", "AWS_ACCESS_KEY_ID": "AKIATEST", "AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "", "AWS_ENDPOINT_URL": "",
		"AWS_ENDPOINT_URL_ECR": url, "AWS_MAX_ATTEMPTS": "1",
	} {
		t.Setenv(k, v)
	}
}

// TestAWSECRToken drives the SDK path against an in-process ECR endpoint:
// the token and expiry come back from a good answer; an empty answer and a
// refusal are errors.
func TestAWSECRToken(t *testing.T) {
	expires := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var target string
	answer := ""
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		target = r.Header.Get("X-Amz-Target")
		a, s := answer, status
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(s)
		_, _ = io.WriteString(w, a)
	}))
	defer srv.Close()
	isolateAWS(t, srv.URL)
	set := func(a string, s int) {
		mu.Lock()
		answer, status = a, s
		mu.Unlock()
	}

	set(fmt.Sprintf(`{"authorizationData":[{"authorizationToken":"QVdTOnB3","expiresAt":%d,`+
		`"proxyEndpoint":"https://1.dkr.ecr.eu-west-2.amazonaws.com"}]}`, expires.Unix()), http.StatusOK)
	tok, exp, err := awsECRToken(context.Background(), "eu-west-2")
	if err != nil {
		t.Fatalf("awsECRToken = %v", err)
	}
	if tok != "QVdTOnB3" || !exp.Equal(expires) {
		t.Errorf("token = %q expires %v, want QVdTOnB3 at %v", tok, exp, expires)
	}
	mu.Lock()
	gotTarget := target
	mu.Unlock()
	if !strings.HasSuffix(gotTarget, ".GetAuthorizationToken") {
		t.Errorf("X-Amz-Target = %q", gotTarget)
	}

	set(`{"authorizationData":[]}`, http.StatusOK)
	if _, _, err := awsECRToken(context.Background(), "eu-west-2"); err == nil ||
		!strings.Contains(err.Error(), "empty authorization data") {
		t.Errorf("empty answer = %v", err)
	}

	set(`{"__type":"AccessDeniedException","message":"no"}`, http.StatusBadRequest)
	if _, _, err := awsECRToken(context.Background(), "eu-west-2"); err == nil ||
		!strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("refusal = %v", err)
	}
}
