// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package hostprobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

const (
	suffix = "preview.example.com"
	issuer = "https://preview-auth.example.com"
)

type views []previewauth.View

func (v views) Ready(context.Context) ([]previewauth.View, error) { return v, nil }

type failing struct{}

func (failing) Ready(context.Context) ([]previewauth.View, error) { return nil, errors.New("down") }

// hosts answers each host as the test says: "auth" redirects as the ALB
// would, "app" serves the app, "other-slot" redirects for slot 0's client,
// "down" fails the connection.
type hosts map[string]string

func (h hosts) RoundTrip(r *http.Request) (*http.Response, error) {
	label := r.URL.Hostname()[:len(r.URL.Hostname())-len(suffix)-1]
	if r.Header.Get("Cookie") != "" || r.URL.Path != "/" {
		return nil, errors.New("unexpected request")
	}
	rec := httptest.NewRecorder()
	slot := "1"
	switch h[label] {
	case "down":
		return nil, errors.New("connection refused")
	case "app":
		rec.WriteHeader(http.StatusOK)
		return rec.Result(), nil
	case "other-slot":
		slot = "0"
	}
	q := url.Values{"client_id": {"patchy-preview-s" + slot}, "response_type": {"code"},
		"redirect_uri": {"https://" + label + "." + suffix + "/oauth2/idpresponse"}}
	rec.Header().Set("Location", issuer+"/authorize?"+q.Encode())
	rec.WriteHeader(http.StatusFound)
	return rec.Result(), nil
}

func TestOnce(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	v := func(label string) previewauth.View {
		return previewauth.View{UID: "u-" + label, Label: label, Project: "p", Slot: 1, Live: true}
	}
	p := &Prober{
		Lister:    views{v("a-1"), v("b-2"), v("c-3"), v("d-4")},
		Callbacks: cb, Issuer: issuer,
		Transport: hosts{"a-1": "auth", "b-2": "app", "c-3": "other-slot", "d-4": "down"},
	}
	res := p.Once(context.Background())
	slices.Sort(res.UnprotectedLabels)
	if res.Probed != 4 || res.Unprotected != 2 || res.Unreachable != 1 ||
		!slices.Equal(res.UnprotectedLabels, []string{"b-2", "c-3"}) {
		t.Fatalf("result %+v", res)
	}
	p.Lister = failing{}
	if res := p.Once(context.Background()); res.Probed != 0 {
		t.Fatalf("failed listing probed %+v", res)
	}
}

func TestStartDisabled(t *testing.T) {
	if err := (&Prober{}).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}
