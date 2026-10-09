// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// The seam-less backfill lister: a PAT credential walks the named
// repositories' open alerts through the API, and the Findings land.
func TestReconcileBackfillThroughPATLister(t *testing.T) {
	mux, base := newGitHubAPI(t)
	mux.HandleFunc("GET /repos/acme/orders/code-scanning/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pat" || r.URL.Query().Get("state") != "open" {
			respondJSON(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
			return
		}
		respondJSON(w, http.StatusOK, "["+alertJSON+"]")
	})

	tests := []struct {
		name         string
		repos        []string
		withSecret   bool
		wantIngested int32
		wantErr      string
		wantDone     bool
	}{
		{
			name: "an exact repository is listed and ingested", repos: []string{"acme/orders"}, withSecret: true,
			wantIngested: 1, wantDone: true,
		},
		{
			name: "a PAT cannot expand an owner prefix", repos: []string{"acme/"}, withSecret: true,
			wantErr: "cannot expand prefix",
		},
		{name: "a missing credential holds the backfill", repos: []string{"acme/orders"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := metav1.NewTime(testClock)
			integ := ghIntegrationAt("gh", "creds", base)
			integ.Spec.Backfill = &v1alpha1.BackfillRequest{By: "dev", At: at, Repositories: tt.repos}
			c := receiverClient(nil, integ)
			if tt.withSecret {
				c = receiverClient(nil, integ, patSecret("creds", "pat", "hmac"))
			}
			r := &IntegrationReconciler{
				Client: c, Creds: NewCreds(c), Now: func() time.Time { return testClock },
				Ingest: &Ingestor{Client: c, Namespace: "patchy", Window: time.Hour, Now: func() time.Time { return testClock }},
			}
			_, got := reconcileGH(t, r)
			st := got.Status.Backfill
			if !tt.withSecret {
				// Without a credential the Integration is not Ready, so the
				// backfill waits rather than running (and is not consumed).
				if st != nil || len(listFindings(t, c)) != 0 {
					t.Errorf("status.backfill = %+v, want the request left pending", st)
				}
				return
			}
			if st == nil {
				t.Fatal("status.backfill = nil, want a run report")
			}
			if st.Ingested != tt.wantIngested {
				t.Errorf("ingested = %d, want %d", st.Ingested, tt.wantIngested)
			}
			if tt.wantErr != "" && !strings.Contains(st.Error, tt.wantErr) || tt.wantErr == "" && st.Error != "" {
				t.Errorf("error = %q, want %q", st.Error, tt.wantErr)
			}
			if (st.BackfilledAt != nil) != tt.wantDone {
				t.Errorf("backfilledAt = %v, want done=%v", st.BackfilledAt, tt.wantDone)
			}
			if tt.wantIngested > 0 {
				items := listFindings(t, c)
				if len(items) != 1 || items[0].Spec.RuleID != "go/reflected-xss" {
					t.Errorf("findings = %+v, want the listed alert's finding", items)
				}
			}
		})
	}
}

// listerFor reads the credential to choose the enumerator: an App fans out
// over its installations, a PAT walks exact repositories, and an unreadable
// Secret is an error.
func TestListerForCredentials(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{name: "app credential", secret: "app"},
		{name: "pat credential", secret: "pat"},
		{name: "missing secret", secret: "absent", wantErr: true},
	}
	c := receiverClient(nil, appSecret("app"), patSecret("pat", "tok", "hmac"))
	r := &IntegrationReconciler{Client: c, Creds: NewCreds(c)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, ok, err := r.listerFor(t.Context(), ghIntegrationAt("gh", tt.secret, ""))
			if !ok {
				t.Fatalf("listerFor() ok = false, want a github integration to support listing")
			}
			if tt.wantErr != (err != nil) || (err == nil) != (l != nil) {
				t.Errorf("listerFor() = %v, %v; wantErr %v", l, err, tt.wantErr)
			}
		})
	}
}

func TestGenerationOf(t *testing.T) {
	tests := map[string]int{
		"finding-1678e4a376-2":  2,
		"finding-1678e4a376-17": 17,
		"finding-1678e4a376-x":  0,
		"nodash":                0,
		"trailing-":             0,
	}
	for in, want := range tests {
		if got := generationOf(in); got != want {
			t.Errorf("generationOf(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestKeyHashIndexer(t *testing.T) {
	labelled := &v1alpha1.Finding{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelKeyHash: "abc"}}}
	if got := KeyHashIndexer(labelled); len(got) != 1 || got[0] != "abc" {
		t.Errorf("KeyHashIndexer(labelled) = %v, want [abc]", got)
	}
	if got := KeyHashIndexer(&v1alpha1.Finding{}); got != nil {
		t.Errorf("KeyHashIndexer(unlabelled) = %v, want nil (not indexed)", got)
	}
}

func TestIngestTruncate(t *testing.T) {
	tests := []struct {
		in    string
		limit int
		want  string
	}{
		{in: "short", limit: 10, want: "short"},
		{in: "abcdef", limit: 4, want: "abcd"},
		{in: "aé", limit: 2, want: "a"},
		{in: "a€", limit: 3, want: "a"},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.limit); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
		}
	}
}

// Property: truncate returns a valid-UTF-8 prefix within the limit, the
// input unchanged when it fits, and drops less than one rune otherwise.
func TestIngestTruncateProperty(t *testing.T) {
	prop := func(runes []rune, limit uint8) bool {
		s, n := string(runes), int(limit)
		got := truncate(s, n)
		if !strings.HasPrefix(s, got) || len(got) > max(n, len(s)) || !utf8.ValidString(got) {
			return false
		}
		if len(s) <= n {
			return got == s
		}
		return len(got) <= n && n-len(got) < utf8.UTFMax
	}
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(1))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
