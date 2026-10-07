// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/signer"
)

const (
	relayHost = "preview-auth.example.com"
	issuer    = "https://" + relayHost
	suffix    = "preview.example.com"
	master    = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ01"
	label     = "demo-7"
	callback  = "https://" + label + "." + suffix + "/oauth2/idpresponse"
	clientIP4 = "203.0.113.7"
)

// signingKey is one RSA key for the package's tests (generation is slow).
var signingKey = sync.OnceValue(func() []byte {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
})

// clock is a settable clock on top of the wall clock, so ID tokens Dex
// mints stay valid while relay tokens can be aged.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// previews is a fake PreviewLookup.
type previews struct {
	mu    sync.Mutex
	views map[string]previewauth.View
	err   error
}

func (p *previews) ByLabel(_ context.Context, l string) (previewauth.View, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return previewauth.View{}, p.err
	}
	if v, ok := p.views[l]; ok {
		return v, nil
	}
	return previewauth.View{Slot: -1}, nil
}

func (p *previews) set(v previewauth.View) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.views[v.Label] = v
}

func (p *previews) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// viewers is a fake Authorizer: the users who may view every Project.
type viewers struct {
	mu    sync.Mutex
	users map[string]bool
	err   error
	asked []previewauth.AccessReview
}

func (v *viewers) Allowed(_ context.Context, r previewauth.AccessReview) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asked = append(v.asked, r)
	if v.err != nil {
		return false, v.err
	}
	return v.users[r.Username], nil
}

func (v *viewers) setErr(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.err = err
}

func (v *viewers) revoke(user string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.users, user)
}

// memLedger is a fake CodeLedger.
type memLedger struct {
	mu    sync.Mutex
	spent map[[32]byte]bool
	err   error
}

func (l *memLedger) Consume(_ context.Context, key [32]byte, _ time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.spent[key] {
		return previewauth.ErrReplayed
	}
	l.spent[key] = true
	return nil
}

// stubUpstream is a fake Upstream that signs in whoever it names: its
// "Dex" is a URL on the relay's own callback, carrying the state and the
// code "ok".
type stubUpstream struct {
	mu   sync.Mutex
	id   previewauth.Identity
	err  error
	urlE error
}

func (u *stubUpstream) AuthURL(_ context.Context, state, nonce, challenge string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.urlE != nil {
		return "", u.urlE
	}
	return "https://dex.example.com/auth?" + url.Values{"state": {state}, "nonce": {nonce},
		"code_challenge": {challenge}}.Encode(), nil
}

func (u *stubUpstream) Exchange(_ context.Context, code, _, _ string) (previewauth.Identity, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil || code != "ok" {
		return previewauth.Identity{}, errors.New("exchange refused")
	}
	return u.id, nil
}

// relay is a relay under test with its fakes.
type relay struct {
	t        *testing.T
	srv      *Server
	handler  http.Handler
	ring     *previewauth.KeyRing
	signer   *signer.Signer
	clock    *clock
	previews *previews
	viewers  *viewers
	ledger   *memLedger
	upstream previewauth.Upstream
	stub     *stubUpstream
}

func newRelay(t *testing.T, edit func(*Config)) *relay {
	t.Helper()
	ring, err := previewauth.NewKeyRing(previewauth.Generation{Number: 1, Master: []byte(master)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := signer.New(signingKey(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{
		t: t, ring: ring, signer: sg, clock: &clock{},
		previews: &previews{views: map[string]previewauth.View{
			label: {UID: "uid-7", Label: label, Project: "demo", Slot: 1, Live: true},
		}},
		viewers: &viewers{users: map[string]bool{"github:alice": true}},
		ledger:  &memLedger{spent: map[[32]byte]bool{}},
		stub:    &stubUpstream{id: previewauth.Identity{Username: "github:alice", Groups: []string{"github:o:t"}}},
	}
	cfg := Config{
		Issuer: issuer, SlotCount: 2, Callbacks: cb, Lifetimes: previewauth.DefaultLifetimes(), Ring: ring,
		Lookup: r.previews, Authorizer: r.viewers, Ledger: r.ledger, Upstream: r.stub, Signer: sg,
		Now: r.clock.now,
	}
	if edit != nil {
		edit(&cfg)
	}
	r.upstream = cfg.Upstream
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.srv, r.handler = srv, srv.Handler()
	return r
}

// transport serves the relay host in process and sends everything else
// to the network (the fake Dex).
type transport struct {
	relay http.Handler
	ip    string
}

func (tr transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != relayHost {
		if strings.HasSuffix(req.URL.Hostname(), suffix) {
			return nil, errors.New("the test followed a redirect to a preview host")
		}
		return http.DefaultTransport.RoundTrip(req)
	}
	in := req.Clone(req.Context())
	if in.Body == nil {
		in.Body = http.NoBody
	}
	in.RemoteAddr = tr.ip + ":40000"
	in.RequestURI = req.URL.RequestURI()
	rec := httptest.NewRecorder()
	tr.relay.ServeHTTP(rec, in)
	return rec.Result(), nil
}

// browser is a client with a cookie jar that does not follow redirects.
func (r *relay) browser() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return &http.Client{
		Jar: jar, Transport: transport{relay: r.handler, ip: clientIP4},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// backchannel is the ALB's client to the token and userinfo endpoints.
func (r *relay) backchannel() *http.Client {
	return &http.Client{Transport: transport{relay: r.handler, ip: "198.51.100.9"},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func authorizeURL(edit func(url.Values)) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {previewauth.ClientID(1)}, "redirect_uri": {callback},
		"scope": {"openid"}, "state": {"alb-state/with+odd=chars"},
	}
	if edit != nil {
		edit(q)
	}
	return issuer + "/authorize?" + q.Encode()
}

func get(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tokenRequest posts form to /token with client authentication: "basic",
// "post", "both" or "none".
func (r *relay) tokenRequest(how string, slot int, secret string, form url.Values) *http.Response {
	r.t.Helper()
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	req, err := http.NewRequest(http.MethodPost, issuer+"/token", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if how == "basic" || how == "both" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
			[]byte(url.QueryEscape(previewauth.ClientID(slot))+":"+url.QueryEscape(secret))))
	}
	if how == "post" || how == "both" {
		f.Set("client_id", previewauth.ClientID(slot))
		f.Set("client_secret", secret)
	}
	req.Body = io.NopCloser(strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.backchannel().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp
}

type tokenJSON struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
}

func decodeToken(t *testing.T, resp *http.Response) tokenJSON {
	t.Helper()
	var out tokenJSON
	if err := json.Unmarshal([]byte(body(t, resp)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// codeFrom reads the code and state off a redirect to the preview callback.
func codeFrom(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d, want a redirect with a code", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != callback {
		t.Fatalf("redirect to %q", resp.Header.Get("Location"))
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}
