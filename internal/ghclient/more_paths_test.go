// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v90/github"
)

// TestGraphQLMalformedResponses: a 200 whose body is not the GraphQL envelope,
// or carries no data, is an error — never "not edited".
func TestGraphQLMalformedResponses(t *testing.T) {
	for _, tt := range []struct {
		name, response, wantMsg string
	}{
		{"not json", `<html>proxy error</html>`, "decode the graphql response"},
		{"no data", `{}`, "the response carries no data"},
		{"data not an object", `{"data":"nope"}`, "ghclient: comment edited IC_x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, c := newFakeGraphQLClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, tt.response)
			})
			got, err := c.CommentEdited(context.Background(), "IC_x")
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("CommentEdited = (%v, %v), want error containing %q", got, err, tt.wantMsg)
			}
			if errors.Is(err, ErrNodeNotFound) {
				t.Errorf("malformed response read as a missing node: %v", err)
			}
		})
	}
}

// TestDeleteIssueGraphQLFailures: once the issue resolves, a non-200 from the
// GraphQL endpoint or an undecodable body fails the delete.
func TestDeleteIssueGraphQLFailures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		wantMsg string
	}{
		{"bad gateway", http.StatusBadGateway, `{}`, "graphql status 502"},
		{"undecodable", http.StatusOK, `not json`, "decode graphql response"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rest, c := newFakeGraphQLClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			rest.HandleFunc("GET /repos/o/r/issues/3", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, `{"number":3,"node_id":"I_3"}`)
			})
			err := c.DeleteIssue(context.Background(), testRepo, 3)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("DeleteIssue error = %v, want containing %q", err, tt.wantMsg)
			}
			if errors.Is(err, ErrDeleteUnauthorized) {
				t.Errorf("failure misread as unauthorized: %v", err)
			}
		})
	}
}

func TestGraphQLURL(t *testing.T) {
	for _, tt := range []struct {
		name string
		gh   func(t *testing.T) *github.Client
		want string
	}{
		{"github.com", func(t *testing.T) *github.Client {
			gh, err := newGitHub(http.DefaultTransport, "")
			if err != nil {
				t.Fatal(err)
			}
			return gh
		}, "https://api.github.com/graphql"},
		{"enterprise", func(t *testing.T) *github.Client {
			gh, err := newGitHub(http.DefaultTransport, "https://ghes.example.com")
			if err != nil {
				t.Fatal(err)
			}
			return gh
		}, "https://ghes.example.com/api/graphql"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := graphqlURL(tt.gh(t)); got != tt.want {
				t.Errorf("graphqlURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestJobLogDownloadDoesNotFollowRedirects: the credential-less log client
// treats a redirect from the signed URL as a failed download rather than
// following it somewhere new.
func TestJobLogDownloadDoesNotFollowRedirects(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/actions/jobs/1/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v3/_logs/1")
		w.WriteHeader(http.StatusFound)
	})
	followed := false
	mux.HandleFunc("GET /_logs/1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v3/_logs/elsewhere")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("GET /_logs/elsewhere", func(w http.ResponseWriter, _ *http.Request) {
		followed = true
		_, _ = w.Write([]byte("should not be read"))
	})
	_, err := c.GetJobLogTail(context.Background(), testRepo, 1, 10)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Errorf("GetJobLogTail error = %v, want the redirect reported as HTTP 302", err)
	}
	if followed {
		t.Error("the log client followed a second redirect")
	}
}

// TestPATAlertEnumeratorSurfacesListErrors: a failing repository listing
// fails the walk instead of reporting it complete.
func TestPATAlertEnumeratorSurfacesListErrors(t *testing.T) {
	base, _ := newFailingServer(t)
	c, err := NewToken("pat-token", base)
	if err != nil {
		t.Fatal(err)
	}
	e := &PATAlertEnumerator{Client: c}
	complete, err := e.Enumerate(context.Background(), []string{"o/r"}, func(Repo, *Alert) bool { return true })
	if err == nil || complete {
		t.Errorf("Enumerate = (%v, %v), want an error and incomplete", complete, err)
	}
}

// TestAppAlertEnumeratorRepoWalkErrors: in the repository-inventory walk, a
// failing inventory and a failing per-repository listing both fail the walk.
func TestAppAlertEnumeratorRepoWalkErrors(t *testing.T) {
	for _, tt := range []struct {
		name      string
		inventory bool // whether the inventory listing succeeds
		wantMsg   string
	}{
		{"inventory fails", false, "list installation repositories"},
		{"repository listing fails", true, "list o/r alerts"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mux, app := newFakeApp(t)
			mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, `[{"id":1,"account":{"login":"o"},"target_type":"User"}]`)
			})
			installationTokens(t, mux, "1")
			mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
				if !tt.inventory {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				writeJSON(t, w, `{"total_count":1,"repositories":[{"name":"r","owner":{"login":"o"}}]}`)
			})
			mux.HandleFunc("GET /repos/o/r/code-scanning/alerts", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			})
			e := &AppAlertEnumerator{App: app}
			complete, err := e.Enumerate(context.Background(), nil, func(Repo, *Alert) bool { return true })
			if err == nil || complete || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Enumerate = (%v, %v), want incomplete with %q", complete, err, tt.wantMsg)
			}
		})
	}
}
