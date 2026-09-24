// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAppSlug: the slug is read once with the App's own JWT and cached; the
// bot login is "<slug>[bot]", as GitHub records it on events.
func TestAppSlug(t *testing.T) {
	mux, app := newFakeApp(t)
	var gets atomic.Int32
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("GET /app Authorization = %q, want the app JWT bearer", auth)
		}
		writeJSON(t, w, `{"id":7,"slug":"patchy-devthenet","name":"patchy devthenet"}`)
	})

	ctx := context.Background()
	for range 2 {
		slug, err := app.Slug(ctx)
		if err != nil {
			t.Fatalf("Slug() error = %v", err)
		}
		if slug != "patchy-devthenet" {
			t.Errorf("Slug() = %q, want patchy-devthenet", slug)
		}
	}
	login, err := app.BotLogin(ctx)
	if err != nil {
		t.Fatalf("BotLogin() error = %v", err)
	}
	if login != "patchy-devthenet[bot]" {
		t.Errorf("BotLogin() = %q, want patchy-devthenet[bot]", login)
	}
	if got := gets.Load(); got != 1 {
		t.Errorf("GET /app hit %d times, want 1 (cached)", got)
	}
}

// TestAppSlugErrors: a failed or slug-less read is an error and is not
// cached, so the next call asks again.
func TestAppSlugErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		payload string
	}{
		{name: "api error", status: http.StatusInternalServerError, payload: `{"message":"boom"}`},
		{name: "no slug", status: http.StatusOK, payload: `{"id":7}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, app := newFakeApp(t)
			var gets atomic.Int32
			mux.HandleFunc("GET /app", func(w http.ResponseWriter, _ *http.Request) {
				gets.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.payload))
			})
			for range 2 {
				if _, err := app.BotLogin(context.Background()); err == nil {
					t.Fatal("BotLogin() error = nil, want non-nil")
				}
			}
			if got := gets.Load(); got != 2 {
				t.Errorf("GET /app hit %d times, want 2 (failures never cached)", got)
			}
		})
	}
}
