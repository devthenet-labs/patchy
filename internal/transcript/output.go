// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OutputPrefix marks a live command-output line on the agent-runner's
// stdout. It is a third stream beside turns and stage results: a running
// command's output, shown to a viewer while the command runs and never
// persisted. It has its own prefix rather than a turn kind so that it spends
// none of the recorder's turn and byte caps, and so that the controller,
// which persists every turn line it reads, never stores it.
const OutputPrefix = "PATCHY-OUTPUT: "

// OutputVersion is the current output chunk schema version.
const OutputVersion = 1

// Output is one chunk of a running command's output. Lines are consecutive
// output lines, the first of them line Line of the command's output (1-based);
// a later chunk whose Line is past the end of the previous one means the
// emitter left lines out. Done is set on a task's last chunk, which may carry
// no lines; Truncated says a limit of the emitter's stopped the task's live
// output for good, which a gap in the line numbers alone (a sample) does not.
// An emitter follows one task at a time: the chunks of one task are never
// interleaved with another's, so a reader may start over at each new task id
// and hold one task's state alone.
type Output struct {
	V         int      `json:"v"`
	Task      string   `json:"task"`
	Line      int      `json:"line"`
	Lines     []string `json:"lines,omitempty"`
	Done      bool     `json:"done,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	At        string   `json:"at,omitempty"`
}

// EncodeOutput renders a chunk as its stdout line, without the trailing
// newline. The lines travel JSON-encoded, so no output text can form a turn
// or an envelope line of its own.
func EncodeOutput(o Output) (string, error) {
	o.V = OutputVersion
	raw, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("transcript: encode output: %w", err)
	}
	return OutputPrefix + string(raw), nil
}

// DecodeOutput recovers a chunk from one log line; ok is false for any line
// that is not an output line. Like Decode it finds the prefix anywhere in the
// line, since the runtime may have prepended a timestamp.
func DecodeOutput(line []byte) (Output, bool) {
	i := bytes.Index(line, []byte(OutputPrefix))
	if i < 0 {
		return Output{}, false
	}
	var o Output
	if err := json.Unmarshal(bytes.TrimSpace(line[i+len(OutputPrefix):]), &o); err != nil {
		return Output{}, false
	}
	if o.V != OutputVersion || o.Task == "" || o.Line < 1 {
		return Output{}, false
	}
	return o, true
}

// HasOutputPrefix reports whether a log line is an output line, so a scanner
// looking for something else can skip it without a full decode.
func HasOutputPrefix(line []byte) bool {
	return bytes.Contains(line, []byte(OutputPrefix))
}
