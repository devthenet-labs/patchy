// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// usage is one response's token usage across every field the API reports.
type usage struct {
	input         int64
	cacheCreation int64
	cacheRead     int64
	output        int64
}

// total is the sum every limit counts.
func (u usage) total() int64 { return u.input + u.cacheCreation + u.cacheRead + u.output }

// merge raises each field to the higher of the two: the API reports
// cumulative values (message_delta's output_tokens is the running total, not
// an increment), so the latest event is the truth and never lowers a count.
func (u usage) merge(o usage) usage {
	return usage{
		input:         max(u.input, o.input),
		cacheCreation: max(u.cacheCreation, o.cacheCreation),
		cacheRead:     max(u.cacheRead, o.cacheRead),
		output:        max(u.output, o.output),
	}
}

// minus is the per-field increment from o to u.
func (u usage) minus(o usage) usage {
	return usage{
		input:         u.input - o.input,
		cacheCreation: u.cacheCreation - o.cacheCreation,
		cacheRead:     u.cacheRead - o.cacheRead,
		output:        u.output - o.output,
	}
}

// usageJSON is the wire shape of a usage object; pointers distinguish an
// absent field from a zero.
type usageJSON struct {
	Input         *int64 `json:"input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	Output        *int64 `json:"output_tokens"`
}

func (j *usageJSON) usage() usage {
	if j == nil {
		return usage{}
	}
	d := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return usage{input: d(j.Input), cacheCreation: d(j.CacheCreation), cacheRead: d(j.CacheRead), output: d(j.Output)}
}

// Scanner bounds: an SSE data line longer than maxSSELine is skipped (usage
// events are a few hundred bytes), and a JSON body longer than maxJSONBody
// is abandoned (its usage then counts as unseen, so the estimate applies).
const (
	maxSSELine  = 64 << 10
	maxJSONBody = 1 << 20
)

type scanMode int

const (
	scanOpaque scanMode = iota // nothing parseable (a binary event stream)
	scanSSE                    // text/event-stream: message_start/message_delta
	scanJSON                   // application/json: a top-level usage object
)

// usageScanner reads token usage off a response as it streams through the
// proxy. Every observed increment is reported at once, so a stream cut right
// after message_start has already charged its input tokens; complete reports
// whether the response reached its end (message_stop, or a JSON body with a
// usage object), which is what decides whether the estimate applies.
type usageScanner struct {
	mode     scanMode
	line     []byte // SSE: the partial current line
	skipping bool   // SSE: inside an over-long line
	body     []byte // JSON: the accumulated body
	over     bool   // JSON: body exceeded maxJSONBody
	seen     usage  // cumulative last-seen values
	complete bool
}

// start picks the mode from the response content type.
func (s *usageScanner) start(contentType string) {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		s.mode = scanSSE
	case strings.HasPrefix(ct, "application/json"):
		s.mode = scanJSON
	default:
		s.mode = scanOpaque
	}
}

// write consumes one chunk of the response and returns the usage increment
// it revealed.
func (s *usageScanner) write(p []byte) usage {
	switch s.mode {
	case scanSSE:
		return s.writeSSE(p)
	case scanJSON:
		if !s.over {
			if len(s.body)+len(p) > maxJSONBody {
				s.over, s.body = true, nil
			} else {
				s.body = append(s.body, p...)
			}
		}
	}
	return usage{}
}

// writeSSE feeds bytes through the line splitter and parses each complete
// data line.
func (s *usageScanner) writeSSE(p []byte) usage {
	var delta usage
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:i])
		p = p[i+1:]
		if !s.skipping {
			delta = delta.mergeAdd(s.parseLine(s.line))
		}
		s.line, s.skipping = s.line[:0], false
	}
	return delta
}

// mergeAdd sums two increments (increments, unlike cumulative values, add).
func (u usage) mergeAdd(o usage) usage {
	return usage{
		input:         u.input + o.input,
		cacheCreation: u.cacheCreation + o.cacheCreation,
		cacheRead:     u.cacheRead + o.cacheRead,
		output:        u.output + o.output,
	}
}

// appendLine buffers part of a line, dropping the line once it exceeds the
// bound.
func (s *usageScanner) appendLine(p []byte) {
	if s.skipping {
		return
	}
	if len(s.line)+len(p) > maxSSELine {
		s.skipping, s.line = true, s.line[:0]
		return
	}
	s.line = append(s.line, p...)
}

// parseLine reads a "data: {...}" SSE line and returns the increment over
// what was seen so far.
func (s *usageScanner) parseLine(line []byte) usage {
	line = bytes.TrimRight(line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return usage{}
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return usage{}
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage *usageJSON `json:"usage"`
		} `json:"message"`
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return usage{}
	}
	switch ev.Type {
	case "message_start":
		return s.observe(ev.Message.Usage.usage())
	case "message_delta":
		return s.observe(ev.Usage.usage())
	case "message_stop":
		s.complete = true
	}
	return usage{}
}

// observe merges a cumulative usage report and returns the increment.
func (s *usageScanner) observe(u usage) usage {
	next := s.seen.merge(u)
	delta := next.minus(s.seen)
	s.seen = next
	return delta
}

// finish closes the scan at end of response and returns any final increment
// (a JSON body is parsed here, once whole).
func (s *usageScanner) finish() usage {
	if s.mode != scanJSON || s.over || len(s.body) == 0 {
		return usage{}
	}
	var resp struct {
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(s.body, &resp); err != nil || resp.Usage == nil {
		return usage{}
	}
	s.complete = true
	return s.observe(resp.Usage.usage())
}

// usageWriter tees the response through a usageScanner. It sits between the
// SSE writer and the audit writer: Unwrap lets the proxy's ResponseController
// reach the real Flusher through it.
type usageWriter struct {
	http.ResponseWriter
	scan    *usageScanner
	onUsage func(usage)
	started bool
	status  int
	// bytes is what a 2xx response streamed through, for the output
	// estimate when neither max_tokens nor a ceiling bounds it.
	bytes int64
}

func (w *usageWriter) WriteHeader(code int) {
	if !w.started {
		w.started, w.status = true, code
		w.scan.start(w.Header().Get("Content-Type"))
		// An encoded body is not parseable text: fail closed to the
		// worst-case charge rather than scan compressed bytes. The broker
		// asks upstreams for identity, so this is a misbehaving upstream.
		if ce := w.Header().Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
			w.scan.mode = scanOpaque
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *usageWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if w.status/100 == 2 {
		w.bytes += int64(len(p))
		if d := w.scan.write(p); d.total() > 0 {
			w.onUsage(d)
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *usageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
