// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
)

const intentBranch = "patchy-intent/target-1"

// refReply writes a Git ref payload pointing at sha.
func refReply(t *testing.T, w http.ResponseWriter, sha string) {
	t.Helper()
	writeJSON(t, w, `{"ref":"refs/heads/`+intentBranch+`","object":{"type":"commit","sha":"`+sha+`"}}`)
}

// unprocessableReply answers 422 with GitHub's message.
func unprocessableReply(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write([]byte(`{"message":"` + message + `","documentation_url":"https://docs.github.com/rest/git/refs"}`))
}

// TestCreateBranchRef: create-only. An existing branch is adopted only when
// it already points at the commit; anything else is refused, and no call
// ever moves a ref.
func TestCreateBranchRef(t *testing.T) {
	tests := []struct {
		name       string
		create     func(w http.ResponseWriter)
		existing   func(t *testing.T, w http.ResponseWriter) // GET of the ref; nil: must not be read
		wantErr    bool
		wantExists bool
	}{
		{
			name:   "created",
			create: func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated); _, _ = w.Write([]byte(`{}`)) },
		},
		{
			name:     "already at the commit is adopted",
			create:   func(w http.ResponseWriter) { unprocessableReply(w, "Reference already exists") },
			existing: func(t *testing.T, w http.ResponseWriter) { refReply(t, w, "c0ffee1") },
		},
		{
			name:       "already at another commit",
			create:     func(w http.ResponseWriter) { unprocessableReply(w, "Reference already exists") },
			existing:   func(t *testing.T, w http.ResponseWriter) { refReply(t, w, "b1ade00") },
			wantErr:    true,
			wantExists: true,
		},
		{
			name:   "existing branch unreadable",
			create: func(w http.ResponseWriter) { unprocessableReply(w, "Reference already exists") },
			existing: func(_ *testing.T, w http.ResponseWriter) {
				http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			},
			wantErr: true,
		},
		{
			name:    "unknown commit",
			create:  func(w http.ResponseWriter) { unprocessableReply(w, "Object does not exist") },
			wantErr: true,
		},
		{
			name: "forbidden",
			create: func(w http.ResponseWriter) {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				want := map[string]any{"ref": "refs/heads/" + intentBranch, "sha": "c0ffee1"}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("create-ref body = %v, want %v", body, want)
				}
				tt.create(w)
			})
			var reads atomic.Int32
			mux.HandleFunc("GET /repos/o/r/git/ref/heads/patchy-intent/target-1", func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				if tt.existing == nil {
					t.Error("existing branch read, want no read")
					http.NotFound(w, nil)
					return
				}
				tt.existing(t, w)
			})
			mux.HandleFunc("PATCH /repos/o/r/git/refs/", func(w http.ResponseWriter, _ *http.Request) {
				t.Error("a ref was moved; CreateBranchRef must never update one")
				http.NotFound(w, nil)
			})

			err := c.CreateBranchRef(context.Background(), testRepo, intentBranch, "c0ffee1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateBranchRef() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrBranchExists); got != tt.wantExists {
				t.Errorf("errors.Is(%v, ErrBranchExists) = %v, want %v", err, got, tt.wantExists)
			}
			if tt.existing != nil && reads.Load() != 1 {
				t.Errorf("existing branch read %d times, want 1", reads.Load())
			}
		})
	}
}

// TestFastForwardRef is the 422-message mapping table: GitHub refuses every
// bad move with a 422, and only the exact verified messages map to the
// sentinels — anything else is a plain error, so an unrecognised refusal
// can never pass for a known one.
func TestFastForwardRef(t *testing.T) {
	tests := []struct {
		name      string
		reply     func(t *testing.T, w http.ResponseWriter)
		wantErr   bool
		wantNotFF bool
		wantNoRef bool
	}{
		{
			// GitHub answers 200 both for a fast-forward and for a branch
			// already at the commit (a no-op).
			name:  "fast-forward or already there",
			reply: func(t *testing.T, w http.ResponseWriter) { refReply(t, w, "c0ffee1") },
		},
		{
			name:      "diverged or rewinding",
			reply:     func(_ *testing.T, w http.ResponseWriter) { unprocessableReply(w, "Update is not a fast forward") },
			wantErr:   true,
			wantNotFF: true,
		},
		{
			name:      "missing branch",
			reply:     func(_ *testing.T, w http.ResponseWriter) { unprocessableReply(w, "Reference does not exist") },
			wantErr:   true,
			wantNoRef: true,
		},
		{
			name:    "unknown commit",
			reply:   func(_ *testing.T, w http.ResponseWriter) { unprocessableReply(w, "Object does not exist") },
			wantErr: true,
		},
		{
			name:    "unrecognised message fails closed",
			reply:   func(_ *testing.T, w http.ResponseWriter) { unprocessableReply(w, "update is not a fast-forward") },
			wantErr: true,
		},
		{
			name: "not found status",
			reply: func(_ *testing.T, w http.ResponseWriter) {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			},
			wantErr: true,
		},
		{
			name:    "success reporting another commit",
			reply:   func(t *testing.T, w http.ResponseWriter) { refReply(t, w, "b1ade00") },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			pattern := "PATCH /repos/o/r/git/refs/heads/" + intentBranch
			mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				want := map[string]any{"sha": "c0ffee1", "force": false}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("update-ref body = %v, want %v (never forced, force stated)", body, want)
				}
				tt.reply(t, w)
			})

			err := c.FastForwardRef(context.Background(), testRepo, intentBranch, "c0ffee1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("FastForwardRef() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrNotFastForward); got != tt.wantNotFF {
				t.Errorf("errors.Is(%v, ErrNotFastForward) = %v, want %v", err, got, tt.wantNotFF)
			}
			if got := errors.Is(err, ErrRefNotFound); got != tt.wantNoRef {
				t.Errorf("errors.Is(%v, ErrRefNotFound) = %v, want %v", err, got, tt.wantNoRef)
			}
		})
	}
}
