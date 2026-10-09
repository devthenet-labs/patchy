// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInitEnvMode: an OTLP endpoint in the environment selects env mode;
// with every signal's exporter set to none nothing is ever dialled, and the
// logger still writes to stderr.
func TestInitEnvMode(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	var stderr bytes.Buffer
	prov, shutdown, err := Init(context.Background(), Config{Stderr: &stderr, ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init = %v", err)
	}
	if prov.Mode != ModeEnv {
		t.Errorf("mode = %v, want env", prov.Mode)
	}
	prov.Logger.Info("hello from env mode")
	if !strings.Contains(stderr.String(), "hello from env mode") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown = %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("second shutdown = %v, want a no-op", err)
	}
}

// TestInitEnvBadExporterFallsBack: an exporter autoexport does not know is a
// setup error, returned beside a working stderr-only provider.
func TestInitEnvBadExporterFallsBack(t *testing.T) {
	for _, signal := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"} {
		t.Run(signal, func(t *testing.T) {
			clearOTELEnv(t)
			for _, k := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"} {
				t.Setenv(k, "none")
			}
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv(signal, "carrier-pigeon")
			var stderr bytes.Buffer
			prov, shutdown, err := Init(context.Background(), Config{Stderr: &stderr})
			if err == nil {
				t.Fatal("Init accepted an unknown exporter")
			}
			if prov == nil || prov.Mode != ModeDisabled || prov.Logger == nil {
				t.Fatalf("fallback provider = %+v", prov)
			}
			prov.Logger.Warn("still logging")
			if !strings.Contains(stderr.String(), "still logging") {
				t.Errorf("fallback logger wrote %q", stderr.String())
			}
			if err := shutdown(context.Background()); err != nil {
				t.Errorf("fallback shutdown = %v", err)
			}
		})
	}
}

// TestFileExportersFailures: a directory that cannot be made, or a signal
// file that cannot be created, is an error, with nothing left open.
func TestFileExportersFailures(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fileExporters(Config{Dir: filepath.Join(blocker, "sub")}); err == nil ||
		!strings.Contains(err.Error(), "create telemetry dir") {
		t.Errorf("dir under a file = %v", err)
	}
	for _, name := range []string{"traces.json", "metrics.json", "logs.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// A directory where the signal file should go cannot be created
			// as a file.
			if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := fileExporters(Config{Dir: dir})
			if err == nil || !strings.Contains(err.Error(), "create "+name) {
				t.Errorf("blocked %s = %v", name, err)
			}
		})
	}
	// Init surfaces the failure and falls back to stderr.
	clearOTELEnv(t)
	prov, _, err := Init(context.Background(), Config{Dir: filepath.Join(blocker, "sub"), Stderr: &bytes.Buffer{}})
	if err == nil || prov.Mode != ModeDisabled {
		t.Errorf("Init over an unusable dir = %v, mode %v", err, prov.Mode)
	}
}

// errHandler accepts every record and fails to handle it.
type errHandler struct{ err error }

func (errHandler) Enabled(context.Context, slog.Level) bool    { return true }
func (h errHandler) Handle(context.Context, slog.Record) error { return h.err }
func (h errHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h errHandler) WithGroup(string) slog.Handler             { return h }

// TestFanoutJoinsErrorsAndKeepsDelivering: one child's failure neither stops
// the others nor hides another child's failure.
func TestFanoutJoinsErrorsAndKeepsDelivering(t *testing.T) {
	e1, e2 := errors.New("first sink down"), errors.New("second sink down")
	ok := newCaptureHandler(slog.LevelDebug)
	h := newFanoutHandler(errHandler{e1}, ok, errHandler{e2})
	err := slog.New(h).Handler().Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0))
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Errorf("Handle = %v, want both failures", err)
	}
	if ok.count() != 1 {
		t.Errorf("healthy child got %d records, want 1", ok.count())
	}
	if h.WithGroup("") != h {
		t.Error("WithGroup(\"\") must return the handler unchanged")
	}
}
