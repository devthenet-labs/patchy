// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// OAuth error codes the relay uses (RFC 6749 §4.1.2.1 and §5.2, RFC 6750).
const (
	CodeInvalidRequest          = "invalid_request"
	CodeInvalidClient           = "invalid_client"
	CodeInvalidGrant            = "invalid_grant"
	CodeUnsupportedGrantType    = "unsupported_grant_type"
	CodeUnsupportedResponseType = "unsupported_response_type"
	CodeInvalidScope            = "invalid_scope"
	CodeInvalidToken            = "invalid_token"
	CodeTemporarilyUnavailable  = "temporarily_unavailable"
)

// Grant types the token endpoint serves.
const (
	GrantAuthorizationCode = "authorization_code"
	GrantRefreshToken      = "refresh_token"
)

// ErrDenied means the access review refused the viewer.
var ErrDenied = errors.New("previewauth: viewer may not see this project's previews")

// Error is a refused request: the HTTP status and the OAuth error code. At
// /authorize it is rendered as a relay page, never as a redirect.
type Error struct {
	Status      int
	Code        string
	Description string
	// Basic is set on an invalid_client answer to a request that tried HTTP
	// Basic authentication, which then carries WWW-Authenticate.
	Basic bool
}

func (e *Error) Error() string { return "previewauth: " + e.Code + ": " + e.Description }

func badRequest(code, desc string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: code, Description: desc}
}

// single reads a parameter that may appear at most once. A repeated parameter
// is refused outright rather than resolved, so a proxy and the relay can never
// read different values.
func single(v url.Values, key string) (string, bool, error) {
	vals, ok := v[key]
	if !ok {
		return "", false, nil
	}
	if len(vals) != 1 {
		return "", true, badRequest(CodeInvalidRequest, key+" is repeated")
	}
	return vals[0], true, nil
}

// noRepeats refuses any repeated parameter, known or not.
func noRepeats(v url.Values) error {
	for k, vals := range v {
		if len(vals) > 1 {
			return badRequest(CodeInvalidRequest, "parameter "+k+" is repeated")
		}
	}
	return nil
}

// AuthorizeRequest is a validated /authorize request.
type AuthorizeRequest struct {
	ClientID    string
	Slot        int
	RedirectURI string
	Label       string
	State       string
	Nonce       string
	Challenge   string
}

// ParseAuthorize validates an /authorize query, in this order: no repeated
// parameter; response_type is code; client_id is a slot's client; redirect_uri
// is that grammar's preview callback; scope includes openid; state is 1 to
// MaxStateBytes bytes, kept byte for byte; nonce is at most MaxNonceBytes; a
// code_challenge, when present, is S256. Other scopes and unknown parameters
// (prompt, max_age, ...) are ignored and never change the result. Every
// refusal is an *Error with status 400.
func ParseAuthorize(q url.Values, slotCount int, cb Callbacks) (AuthorizeRequest, error) {
	if err := noRepeats(q); err != nil {
		return AuthorizeRequest{}, err
	}
	if q.Get("response_type") != "code" {
		return AuthorizeRequest{}, badRequest(CodeUnsupportedResponseType, "response_type must be code")
	}
	clientID := q.Get("client_id")
	slot, ok := SlotOf(clientID, slotCount)
	if !ok {
		return AuthorizeRequest{}, badRequest(CodeInvalidRequest, "unknown client_id")
	}
	redirect := q.Get("redirect_uri")
	label, err := cb.Parse(redirect)
	if err != nil {
		return AuthorizeRequest{}, badRequest(CodeInvalidRequest, "redirect_uri is not a preview callback")
	}
	if !hasScope(q.Get("scope"), "openid") {
		return AuthorizeRequest{}, badRequest(CodeInvalidScope, "scope must include openid")
	}
	state := q.Get("state")
	if state == "" || len(state) > MaxStateBytes {
		return AuthorizeRequest{}, badRequest(CodeInvalidRequest, "state is missing or too long")
	}
	nonce := q.Get("nonce")
	if len(nonce) > MaxNonceBytes {
		return AuthorizeRequest{}, badRequest(CodeInvalidRequest, "nonce is too long")
	}
	challenge, hasChallenge := q.Get("code_challenge"), q.Has("code_challenge")
	method, hasMethod := q.Get("code_challenge_method"), q.Has("code_challenge_method")
	switch {
	case hasChallenge && (method != "S256" || !validChallenge(challenge)):
		return AuthorizeRequest{}, badRequest(CodeInvalidRequest, "code_challenge must be S256")
	case !hasChallenge && hasMethod:
		return AuthorizeRequest{}, badRequest(CodeInvalidRequest, "code_challenge_method without code_challenge")
	}
	return AuthorizeRequest{
		ClientID: clientID, Slot: slot, RedirectURI: redirect, Label: label,
		State: state, Nonce: nonce, Challenge: challenge,
	}, nil
}

func hasScope(scope, want string) bool {
	for _, s := range strings.Split(scope, " ") {
		if s == want {
			return true
		}
	}
	return false
}

// CodeRedirect is where /authorize sends the browser with a code: the
// redirect URI (already validated, so it has no query) with code and state.
func CodeRedirect(redirectURI, code, state string) string {
	return redirectURI + "?" + url.Values{"code": {code}, "state": {state}}.Encode()
}

// ClientAuth is an authenticated ALB client at the token endpoint.
type ClientAuth struct {
	ClientID string
	Slot     int
}

func invalidClient(basic bool, desc string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeInvalidClient, Description: desc, Basic: basic}
}

// AuthenticateClient authenticates a token request by exactly one of
// client_secret_basic (authorization is the request's Authorization header;
// the id and secret are form-decoded per RFC 6749 §2.3.1) or
// client_secret_post (client_id and client_secret in form). Both, neither, an
// Authorization header of another scheme, an unknown client or a wrong secret
// is invalid_client (401). The secret is checked in constant time against the
// current and the previous generation.
func (r *KeyRing) AuthenticateClient(authorization string, form url.Values, slotCount int) (ClientAuth, error) {
	scheme, cred, _ := strings.Cut(authorization, " ")
	basic := strings.EqualFold(scheme, "Basic")
	if authorization != "" && !basic {
		return ClientAuth{}, invalidClient(false, "unsupported Authorization scheme")
	}
	formID, hasFormID, err := single(form, "client_id")
	if err != nil {
		return ClientAuth{}, err
	}
	formSecret, post, err := single(form, "client_secret")
	if err != nil {
		return ClientAuth{}, err
	}
	if basic == post {
		return ClientAuth{}, invalidClient(basic, "use exactly one client authentication method")
	}
	id, secret := formID, formSecret
	if basic {
		var ok bool
		if id, secret, ok = decodeBasic(cred); !ok {
			return ClientAuth{}, invalidClient(true, "malformed Basic credentials")
		}
		if hasFormID && formID != id {
			return ClientAuth{}, invalidClient(true, "client_id does not match the Basic credentials")
		}
	}
	slot, ok := SlotOf(id, slotCount)
	if !ok || !r.VerifyClientSecret(slot, secret) {
		return ClientAuth{}, invalidClient(basic, "client authentication failed")
	}
	return ClientAuth{ClientID: id, Slot: slot}, nil
}

func decodeBasic(cred string) (string, string, bool) {
	raw, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		return "", "", false
	}
	encID, encSecret, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", false
	}
	id, err := url.QueryUnescape(encID)
	if err != nil {
		return "", "", false
	}
	secret, err := url.QueryUnescape(encSecret)
	if err != nil {
		return "", "", false
	}
	return id, secret, true
}

// TokenRequest is a validated token request body.
type TokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	Verifier     string
	RefreshToken string
}

// ParseTokenRequest validates a token request's form: no repeated parameter,
// a supported grant_type and its required parameters. Any scope is ignored.
func ParseTokenRequest(form url.Values) (TokenRequest, error) {
	if err := noRepeats(form); err != nil {
		return TokenRequest{}, err
	}
	req := TokenRequest{
		GrantType: form.Get("grant_type"), Code: form.Get("code"), RedirectURI: form.Get("redirect_uri"),
		Verifier: form.Get("code_verifier"), RefreshToken: form.Get("refresh_token"),
	}
	switch req.GrantType {
	case GrantAuthorizationCode:
		if req.Code == "" || len(req.Code) > MaxTokenBytes || req.RedirectURI == "" {
			return TokenRequest{}, badRequest(CodeInvalidRequest, "code and redirect_uri are required")
		}
		if form.Has("code_verifier") && !validVerifier(req.Verifier) {
			return TokenRequest{}, badRequest(CodeInvalidRequest, "malformed code_verifier")
		}
	case GrantRefreshToken:
		if req.RefreshToken == "" || len(req.RefreshToken) > MaxTokenBytes {
			return TokenRequest{}, badRequest(CodeInvalidRequest, "refresh_token is required")
		}
	case "":
		return TokenRequest{}, badRequest(CodeInvalidRequest, "grant_type is required")
	default:
		return TokenRequest{}, badRequest(CodeUnsupportedGrantType, "unsupported grant_type")
	}
	return req, nil
}

func invalidGrant(desc string) *Error { return badRequest(CodeInvalidGrant, desc) }

// RedeemCode judges an opened code against the authenticated client and the
// request: issued to this client, for this redirect URI, and, when the code
// carries a PKCE challenge, with the verifier that hashes to it. A verifier
// for a code without a challenge is refused too. The single-use ledger and
// the Preview and access re-checks are the caller's.
func RedeemCode(g Grant, client ClientAuth, req TokenRequest) error {
	if g.IssuedTo(client.ClientID) != nil || g.Slot != client.Slot {
		return invalidGrant("code was not issued to this client")
	}
	if req.RedirectURI != g.RedirectURI {
		return invalidGrant("redirect_uri does not match")
	}
	switch {
	case g.Challenge == "" && req.Verifier != "":
		return invalidGrant("code_verifier for a code without a challenge")
	case g.Challenge != "" &&
		subtle.ConstantTimeCompare([]byte(S256(req.Verifier)), []byte(g.Challenge)) != 1:
		return invalidGrant("code_verifier does not match")
	}
	return nil
}

// CheckRefresh judges an opened refresh token against the authenticated
// client.
func CheckRefresh(g Grant, client ClientAuth) error {
	if g.IssuedTo(client.ClientID) != nil || g.Slot != client.Slot {
		return invalidGrant("refresh token was not issued to this client")
	}
	return nil
}

// CodeKey is the single-use ledger's key for an opened code: the SHA-256 of
// its JTI, which is inside the sealed payload. Keying on the payload rather
// than on the code's spelling means no second spelling of a code can redeem
// it again, and the ledger never holds anything redeemable.
func CodeKey(g Grant) [32]byte { return sha256.Sum256([]byte("code|" + g.JTI)) }

// TokenError maps any failure at the token endpoint to its answer. A token
// that does not open, has expired or is no longer bound, a Preview that is
// gone, a Project that is unknown and a refused access review all end the
// ALB session (invalid_grant). Anything else, such as a failed lookup or
// access review, is temporarily_unavailable (503), which the ALB does not
// treat as a sign-out.
func TokenError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, ErrInvalidToken), errors.Is(err, ErrExpired), errors.Is(err, ErrNotBound),
		errors.Is(err, ErrNoPreview), errors.Is(err, ErrNoProject), errors.Is(err, ErrDenied),
		errors.Is(err, ErrReplayed):
		return invalidGrant("grant is not valid")
	}
	return &Error{Status: http.StatusServiceUnavailable, Code: CodeTemporarilyUnavailable,
		Description: "try again"}
}

// ParseBearer reads a userinfo request's access token from an Authorization
// Bearer header (GET or POST) or, on POST only, the form's access_token.
// postForm must be the body's form only: a token in the query string is never
// read. Sending both is invalid_request (400); sending neither, or another
// scheme, is invalid_token (401).
func ParseBearer(method, authorization string, postForm url.Values) (string, error) {
	if method != http.MethodGet && method != http.MethodPost {
		return "", &Error{Status: http.StatusMethodNotAllowed, Code: CodeInvalidRequest, Description: "GET or POST"}
	}
	var fromHeader string
	if authorization != "" {
		scheme, tok, _ := strings.Cut(authorization, " ")
		if !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
			return "", invalidToken("malformed Authorization header")
		}
		fromHeader = tok
	}
	var fromForm string
	if method == http.MethodPost {
		v, has, err := single(postForm, "access_token")
		if err != nil {
			return "", err
		}
		if has && v == "" {
			return "", invalidToken("empty access_token")
		}
		fromForm = v
	}
	switch {
	case fromHeader != "" && fromForm != "":
		return "", badRequest(CodeInvalidRequest, "access token sent twice")
	case fromHeader != "":
		return fromHeader, nil
	case fromForm != "":
		return fromForm, nil
	}
	return "", invalidToken("no access token")
}

func invalidToken(desc string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeInvalidToken, Description: desc}
}

// UserinfoError maps any failure at userinfo to its answer: a token that
// does not open, has expired or is no longer bound to a live Preview is
// invalid_token (401); anything else is 503.
func UserinfoError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, ErrInvalidToken), errors.Is(err, ErrExpired), errors.Is(err, ErrNotBound),
		errors.Is(err, ErrNoPreview):
		return invalidToken("token is not valid")
	}
	return &Error{Status: http.StatusServiceUnavailable, Code: CodeTemporarilyUnavailable,
		Description: "try again"}
}

// ValidIssuer accepts the relay's issuer: an https origin with no port,
// path, query, fragment or trailing slash, compared byte for byte everywhere.
func ValidIssuer(issuer string) bool {
	host, ok := strings.CutPrefix(issuer, "https://")
	if !ok || !validSuffix(host) {
		return false
	}
	u, err := url.Parse(issuer)
	return err == nil && u.Host == host && u.Path == "" && u.String() == issuer
}
