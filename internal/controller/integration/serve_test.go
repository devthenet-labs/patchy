// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/generic"
	"github.com/bitwise-media-group/patchy/internal/webhook"
	pkggeneric "github.com/bitwise-media-group/patchy/pkg/generic"
)

// serveReceiver runs the receiver's endpoints on a real webhook server over
// an ephemeral loopback port. stop cancels it and waits for the drain, so
// every accepted delivery has been handled once stop returns.
func serveReceiver(t *testing.T, r *Receiver) (base string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := webhook.NewServer(webhook.Config{Endpoints: r.Endpoints(), Workers: 1}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Serve() = %v, want context.Canceled", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("server did not drain")
		}
	}
	t.Cleanup(stop)
	return "http://" + ln.Addr().String(), stop
}

func deliver(t *testing.T, url string, headers map[string]string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The generic wildcard route authenticates each delivery strictly against
// the Integration its path names: another integration's secret, an unknown
// name, a disabled source or an unreadable Secret are all 401, and only the
// correctly signed delivery is ingested.
func TestGenericRouteIsolation(t *testing.T) {
	disabled := testGenericIntegration("disabled")
	disabled.Spec.Generic.Source.Enabled = false
	c := receiverClient(nil,
		testGenericIntegration("warehouse"), genericSecret("warehouse", "wh-secret"),
		testGenericIntegration("cmdb"), genericSecret("cmdb", "cmdb-secret"),
		testGenericIntegration("nosecret"),
		disabled, genericSecret("disabled", "dis-secret"),
	)
	r := newTestReceiver(c)
	base, stop := serveReceiver(t, r)
	body := []byte(genericFindingsJSON)

	tests := []struct {
		name   string
		path   string
		secret string
		want   int
	}{
		{
			name: "another integration's secret", path: GenericPathFor("warehouse"), secret: "cmdb-secret",
			want: http.StatusUnauthorized,
		},
		{
			name: "an unknown integration", path: GenericPathFor("ghost"), secret: "wh-secret",
			want: http.StatusUnauthorized,
		},
		{
			name: "a disabled source", path: GenericPathFor("disabled"), secret: "dis-secret",
			want: http.StatusUnauthorized,
		},
		{name: "an unreadable secret", path: GenericPathFor("nosecret"), secret: "", want: http.StatusUnauthorized},
		{name: "unsigned", path: GenericPathFor("warehouse"), want: http.StatusUnauthorized},
		{name: "its own secret", path: GenericPathFor("warehouse"), secret: "wh-secret", want: http.StatusAccepted},
	}
	for _, tt := range tests {
		headers := map[string]string{}
		if tt.secret != "" {
			headers[pkggeneric.SignatureHeader] = generic.Sign([]byte(tt.secret), body)
		}
		if tt.name == "an unreadable secret" {
			headers[pkggeneric.SignatureHeader] = generic.Sign(nil, body)
		}
		if got := deliver(t, base+tt.path, headers, body); got != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, got, tt.want)
		}
	}

	stop()
	items := listFindings(t, c)
	if len(items) != 1 || items[0].Spec.Source != "warehouse" {
		t.Fatalf("findings = %+v, want the one authenticated warehouse delivery", items)
	}
}

// End to end on the GitHub route: a signed issue close is accepted and
// reaches the finding; a forged one is refused and changes nothing.
func TestGitHubRouteEndToEnd(t *testing.T) {
	fnd := trackedFinding(v1alpha1.PhaseQueued)
	c := receiverClient(nil, ghIntegrationAt("gh", "creds", ""), patSecret("creds", "pat", "live-secret"), fnd)
	fnd.Status = trackedFinding(v1alpha1.PhaseQueued).Status
	if err := c.Status().Update(t.Context(), fnd); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	r := newTestReceiver(c)
	base, stop := serveReceiver(t, r)
	body := []byte(`{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`)

	forged := map[string]string{
		"X-Hub-Signature-256": hubSignature([]byte("guess"), body),
		"X-GitHub-Event":      "issues", "X-GitHub-Delivery": "d-forged",
	}
	if got := deliver(t, base+GitHubPath, forged, body); got != http.StatusUnauthorized {
		t.Fatalf("forged delivery: status %d, want 401", got)
	}
	ping := map[string]string{
		"X-Hub-Signature-256": hubSignature([]byte("live-secret"), []byte(`{}`)),
		"X-GitHub-Event":      "ping", "X-GitHub-Delivery": "d-ping",
	}
	if got := deliver(t, base+GitHubPath, ping, []byte(`{}`)); got != http.StatusNoContent {
		t.Errorf("ping: status %d, want 204", got)
	}
	signed := map[string]string{
		"X-Hub-Signature-256": hubSignature([]byte("live-secret"), body),
		"X-GitHub-Event":      "issues", "X-GitHub-Delivery": "d-1",
	}
	if got := deliver(t, base+GitHubPath, signed, body); got != http.StatusAccepted {
		t.Fatalf("signed delivery: status %d, want 202", got)
	}
	stop()
	if got := get(t, c, fnd.Name).Status.Phase; got != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %s, want HandedOff after the signed close", got)
	}
}

func TestGenericLookupFailures(t *testing.T) {
	boom := errors.New("cache unavailable")
	failGet := &interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*v1alpha1.Integration); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}

	t.Run("a failed lookup yields no candidate and is logged", func(t *testing.T) {
		var logs bytes.Buffer
		r := newTestReceiver(receiverClient(failGet, testGenericIntegration("warehouse"), genericSecret("warehouse", "s")))
		r.Log = slog.New(slog.NewTextHandler(&logs, nil))
		req, _ := http.NewRequest(http.MethodPost, "http://x"+GenericPathFor("warehouse"), nil)
		req.SetPathValue("name", "warehouse")
		if got := r.genericSecretsFor(t.Context(), req); got != nil {
			t.Errorf("genericSecretsFor() = %q, want none", got)
		}
		if !strings.Contains(logs.String(), "generic integration lookup failed") {
			t.Errorf("logs = %q, want the lookup failure", logs.String())
		}
	})

	t.Run("the handler surfaces a failed lookup", func(t *testing.T) {
		r := newTestReceiver(receiverClient(failGet, testGenericIntegration("warehouse")))
		e := webhook.Event{
			Type: generic.EventFindings, Payload: []byte(genericFindingsJSON), Path: GenericPathFor("warehouse"),
		}
		err := r.handleGeneric(t.Context(), e)
		if !errors.Is(err, boom) {
			t.Errorf("handleGeneric() = %v, want the lookup error", err)
		}
	})

	t.Run("the handler refuses an unparseable path", func(t *testing.T) {
		r := newTestReceiver(receiverClient(nil))
		e := webhook.Event{Type: generic.EventFindings, Payload: []byte(genericFindingsJSON), Path: "/generic/webhooks"}
		err := r.handleGeneric(t.Context(), e)
		if err == nil || !strings.Contains(err.Error(), "unparseable path") {
			t.Errorf("handleGeneric() = %v, want the path refusal", err)
		}
	})
}
