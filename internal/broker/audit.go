// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"log/slog"
	"net/http"
	"time"
)

// auditWriter counts what one response wrote so the audit line can report
// status and bytes. It exposes the wrapped writer via Unwrap so the proxy's
// ResponseController still reaches the real Flusher.
type auditWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *auditWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// auditEntry is what one request's audit line carries.
type auditEntry struct {
	pod     string
	route   string
	model   string
	reason  string // the rejection reason; "" when proxied
	status  int
	bytes   int64
	elapsed time.Duration
	// tokens is what this request charged the pod; estimated marks a
	// charge that came (in part) from the request-size estimate because
	// the response's usage was never seen.
	tokens    int64
	estimated bool
	// totals is the pod's running count after this request, so operators
	// can size the limits from observed runs.
	totals podTotals
}

// audit emits the one slog line every request gets: caller pod, route,
// method, path, status, duration, bytes, the model named, the tokens
// charged and the pod's running totals. Never bodies, never headers — model
// prompts and completions must not land in the broker's logs.
func audit(log *slog.Logger, r *http.Request, e auditEntry) {
	msg := "proxied"
	if e.reason != "" {
		msg = "rejected"
	}
	log.LogAttrs(r.Context(), slog.LevelInfo, msg,
		slog.String("pod", e.pod),
		slog.String("route", e.route),
		slog.String("method", r.Method),
		slog.String("path", clip(r.URL.Path)),
		slog.Int("status", e.status),
		slog.Duration("duration", e.elapsed),
		slog.Int64("bytes", e.bytes),
		slog.String("model", clip(e.model)),
		slog.String("reason", e.reason),
		slog.Int64("tokens", e.tokens),
		slog.Bool("estimated", e.estimated),
		slog.Int64("pod_requests", e.totals.requests),
		slog.Int64("pod_tokens", e.totals.tokens))
}
