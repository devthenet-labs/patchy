// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// What claude 2.1.263 does when its Bash tool runs a command in -p
// stream-json mode (probed): it prints a system event of subtype
// task_started (task_id, tool_use_id, task_type "local_bash",
// is_backgrounded, session_id); while the command runs it appends the
// command's combined output, line by line as it is produced, to
// <tmp>/claude-<uid>/<slug>/<session>/tasks/<task>.output, where <tmp> is
// CLAUDE_CODE_TMPDIR or /tmp (TMPDIR does not move it) and <slug> is the
// CLI's working directory with '/' and '.' made '-'; it deletes the file
// when the command ends, then prints a system event of subtype
// task_notification and the tool result as a user event.

// claudeTmpEnv names the variable that moves the claude CLI's temporary
// directory, and claudeTmpDefault is where it is otherwise.
const (
	claudeTmpEnv     = "CLAUDE_CODE_TMPDIR"
	claudeTmpDefault = "/tmp"
)

// claudeTaskEvent is the part of a stream-json line the command events
// read: the system events' task fields, and the tool_use_id of each block
// of a user event's message.
type claudeTaskEvent struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	SessionID      string `json:"session_id"`
	TaskID         string `json:"task_id"`
	ToolUseID      string `json:"tool_use_id"`
	TaskType       string `json:"task_type"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	Message        struct {
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	} `json:"message"`
}

// ScanTasks reads the command events off one stream-json line; see
// scanStreamTasks.
func (c *Claude) ScanTasks(line []byte, answers bool) []TaskEvent {
	return scanStreamTasks(line, answers)
}

// TaskOutputGlob is where claude writes a running command's output; see
// taskOutputGlob.
func (c *Claude) TaskOutputGlob(env []string, uid int, session, task string) string {
	return taskOutputGlob(env, uid, session, task)
}

// Markers a line must hold to carry a command event: a system event whose
// subtype is init or a task's, and a user event only when it answers a tool
// call. A line holding one is only a candidate; the decode decides.
var (
	markInit       = []byte(`"init"`)
	markTask       = []byte(`"task_`)
	markToolResult = []byte(`"tool_result"`)
)

// scanStreamTasks projects one stream-json line onto the command events: the
// init event's session, a foreground local_bash command's start, a command's
// end, and, when answers is set, the tool calls a user event answers. Every
// other line, a backgrounded command and every other task type among them,
// yields nothing. A line that cannot hold one is passed over without a
// decode, and so is a tool result when answers is not set.
func scanStreamTasks(line []byte, answers bool) []TaskEvent {
	if !bytes.Contains(line, markTask) && !bytes.Contains(line, markInit) &&
		(!answers || !bytes.Contains(line, markToolResult)) {
		return nil
	}
	var ev claudeTaskEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "system":
		switch ev.Subtype {
		case "init":
			if ev.SessionID != "" {
				return []TaskEvent{{Kind: TaskSession, Session: ev.SessionID}}
			}
		case "task_started":
			if ev.TaskID != "" && ev.TaskType == "local_bash" && !ev.IsBackgrounded {
				return []TaskEvent{{Kind: TaskStarted, Session: ev.SessionID, Task: ev.TaskID, ToolUse: ev.ToolUseID}}
			}
		case "task_notification":
			if ev.TaskID != "" {
				return []TaskEvent{{Kind: TaskEnded, Task: ev.TaskID, ToolUse: ev.ToolUseID}}
			}
		}
	case "user":
		if !answers {
			return nil
		}
		var out []TaskEvent
		for _, b := range ev.Message.Content {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				out = append(out, TaskEvent{Kind: TaskAnswered, ToolUse: b.ToolUseID})
			}
		}
		return out
	}
	return nil
}

// taskOutputGlob is the pattern matching claude's output file for command
// task of session. The directory under claude-<uid> is a slug of the CLI's
// working directory; it is matched, not computed, since only the CLI knows
// its rule for every character. The temporary directory is the last
// CLAUDE_CODE_TMPDIR in env, or /tmp when that is unset or empty; one that
// is not absolute, or an id that could leave its path segment or act as a
// pattern, gives "".
func taskOutputGlob(env []string, uid int, session, task string) string {
	if !taskIDShaped(session) || !taskIDShaped(task) {
		return ""
	}
	tmp := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, claudeTmpEnv+"="); ok {
			tmp = v
		}
	}
	if tmp == "" {
		tmp = claudeTmpDefault
	}
	if !filepath.IsAbs(tmp) {
		return ""
	}
	return filepath.Join(globEscape(filepath.Clean(tmp)), "claude-"+strconv.Itoa(uid), "*", session, "tasks",
		task+".output")
}

// taskIDShaped reports whether s is shaped like the session and task ids
// the CLI names: letters, digits, '-' and '_', at most 128 of them.
func taskIDShaped(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// globEscape quotes the characters filepath.Match treats as a pattern, so a
// directory is matched as itself. Windows has no quoting ('\\' separates
// paths there); the CLI runs in a Linux pod.
func globEscape(s string) string {
	if filepath.Separator == '\\' {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
