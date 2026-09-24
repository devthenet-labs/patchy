// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// writeJSONTagged answers a list the way GitHub does: a weak ETag over the
// body, GitHub's cache-control, and a bodiless 304 when the request's
// If-None-Match already names that tag. Any change to what the list would
// render changes its tag.
func writeJSONTagged(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `W/"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=60, s-maxage=60")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(body, '\n'))
}

// unprocessable answers 422 with GitHub's message — how the Git refs
// endpoints refuse every bad move, told apart only by the message.
func unprocessable(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message":           message,
		"documentation_url": "https://docs.github.com/rest",
		"status":            "422",
	})
}

// notFound answers GitHub's 404 body.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message":           "Not Found",
		"documentation_url": "https://docs.github.com/rest",
		"status":            "404",
	})
}

// now is the fake's clock at GitHub's timestamp precision (whole seconds,
// UTC), what the new records are stamped with.
func (s *Server) now() time.Time { return s.Now().UTC().Truncate(time.Second) }
