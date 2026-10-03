// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testKeyPEM is a throwaway RSA key in the PEM form GitHub returns.
func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// conversionBody is a conversion response as GitHub documents it, OAuth
// client credentials included.
func conversionBody(keyPEM string, webhookSecret any) map[string]any {
	return map[string]any{
		"id": 123456, "slug": "patchy-acme", "node_id": "A_1", "name": "patchy-acme",
		"html_url": "https://github.com/apps/patchy-acme",
		"owner":    map[string]any{"login": "acme", "id": 9},
		"permissions": map[string]string{
			"contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write",
		},
		"events":         []string{},
		"client_id":      "Iv1.oauthclientid",
		"client_secret":  "oauth-client-secret-never-kept",
		"webhook_secret": webhookSecret,
		"pem":            keyPEM,
	}
}

// fakeConversions serves POST /app-manifests/{code}/conversions: code
// converts once, to body; every other request is recorded as a failure.
func fakeConversions(t *testing.T, code string, status int, body any) *httptest.Server {
	t.Helper()
	used := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodPost:
			t.Errorf("conversion method %s", r.Method)
		case r.Header.Get("Authorization") != "":
			t.Error("the conversion sent credentials: the endpoint needs none")
		case r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") == "":
			t.Errorf("conversion headers %v", r.Header)
		}
		if r.URL.Path != "/app-manifests/"+code+"/conversions" || used {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		used = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConvert(t *testing.T) {
	key := testKeyPEM(t)
	srv := fakeConversions(t, "c0de", http.StatusCreated, conversionBody(key, "whsec"))
	app, err := Convert(context.Background(), srv.Client(), srv.URL+"/", "c0de")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if app.ID != 123456 || app.Slug != "patchy-acme" || app.Owner != "acme" ||
		app.HTMLURL != "https://github.com/apps/patchy-acme" || app.Permissions["issues"] != "write" {
		t.Errorf("app = %+v", app)
	}
	if string(app.Credentials.privateKey) != key || app.Credentials.webhookSecret != "whsec" {
		t.Error("the credentials were not kept")
	}
	if _, err := Convert(context.Background(), srv.Client(), srv.URL, "c0de"); !errors.Is(err, ErrCodeRejected) {
		t.Errorf("a used code = %v, want ErrCodeRejected", err)
	}
}

// TestConvertRefuses: a code GitHub refuses, an answer that is not an App
// with a key, and a code that is not shaped like one never become an App.
func TestConvertRefuses(t *testing.T) {
	key := testKeyPEM(t)
	noKey := conversionBody("not a key", nil)
	badSlug := conversionBody(key, nil)
	badSlug["slug"] = "../admin"
	noID := conversionBody(key, nil)
	noID["id"] = 0
	tests := []struct {
		name   string
		status int
		body   any
		code   string
		want   string
	}{
		{"expired", http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"}, "c0de",
			"single-use"},
		{"server error", http.StatusBadGateway, map[string]string{"message": "oops"}, "c0de", "HTTP 502"},
		{"no key", http.StatusCreated, noKey, "c0de", "no PEM private key"},
		{"bad slug", http.StatusCreated, badSlug, "c0de", "slug"},
		{"no ID", http.StatusCreated, noID, "c0de", "no App ID"},
		{"code shaped like a path", http.StatusCreated, conversionBody(key, nil), "../x", "not shaped"},
		{"oversized", http.StatusCreated, map[string]string{"pem": strings.Repeat("a", maxResponseBytes)}, "c0de",
			"larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeConversions(t, "c0de", tt.status, tt.body)
			app, err := Convert(context.Background(), srv.Client(), srv.URL, tt.code)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Convert() = %+v, %v; want an error containing %q", app, err, tt.want)
			}
		})
	}
}

// TestCredentialsNeverFormat: printing an App with any verb shows neither
// the private key nor the webhook secret.
func TestCredentialsNeverFormat(t *testing.T) {
	key := testKeyPEM(t)
	srv := fakeConversions(t, "c0de", http.StatusCreated, conversionBody(key, "whsec-value"))
	app, err := Convert(context.Background(), srv.Client(), srv.URL, "c0de")
	if err != nil {
		t.Fatal(err)
	}
	out := fmt.Sprintf("%v %+v %#v %s %v %+v %#v", app, app, app, app.Credentials, *app, *app, *app)
	for _, secret := range []string{"PRIVATE KEY", key[40:80], "whsec-value"} {
		if strings.Contains(out, secret) {
			t.Fatalf("formatting an App leaked %q:\n%s", secret, out)
		}
	}
}

func TestDrift(t *testing.T) {
	m, err := Build(Config{Features: Features{Security: true}, Name: "patchy-acme",
		HomepageURL: DefaultHomepageURL, WebhookURL: testWebhook})
	if err != nil {
		t.Fatal(err)
	}
	same := &App{Permissions: map[string]string{}, Events: m.DefaultEvents}
	for p, a := range m.DefaultPermissions {
		same.Permissions[p] = a
	}
	if d := Drift(m, same); len(d) != 0 {
		t.Errorf("Drift(same) = %v", d)
	}
	edited := &App{
		Permissions: map[string]string{"metadata": "read", "issues": "read", "contents": "write",
			"pull_requests": "write", "security_events": "write", "administration": "write"},
		Events: []string{"code_scanning_alert", "issues", "pull_request", "push"},
	}
	want := []string{
		"event issue_comment is not subscribed, which the manifest asked for",
		"event push is subscribed, which the manifest did not ask for",
		`permission administration is "write", which the manifest did not ask for`,
		`permission issues is "read", the manifest asked for "write"`,
	}
	if got := Drift(m, edited); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Drift(edited) =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
