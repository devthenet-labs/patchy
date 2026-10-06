// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// Captured claude 2.1.263 command events (ids shortened).
const (
	lineTaskStarted = `{"type":"system","subtype":"task_started","task_id":"b578qoc1g",` +
		`"tool_use_id":"toolu_01","description":"Run the suite","task_type":"local_bash",` +
		`"is_backgrounded":false,"uuid":"u1","session_id":"5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c"}`
	lineTaskNotification = `{"type":"system","subtype":"task_notification","task_id":"b578qoc1g",` +
		`"tool_use_id":"toolu_01","status":"completed","output_file":"","uuid":"u2",` +
		`"session_id":"5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c"}`
	lineToolResults = `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_01","content":"ok  ./..."},` +
		`{"type":"text","text":"not a result"},` +
		`{"type":"tool_result","tool_use_id":"toolu_02","content":[{"type":"text","text":"done"}]}]}}`
)

func TestScanTasks(t *testing.T) {
	const session = "5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c"
	tests := []struct {
		name string
		line string
		want []TaskEvent
	}{
		{"init", `{"type":"system","subtype":"init","session_id":"` + session + `","tools":["Bash"]}`,
			[]TaskEvent{{Kind: TaskSession, Session: session}}},
		{"init without a session", `{"type":"system","subtype":"init"}`, nil},
		{"a foreground command starts", lineTaskStarted,
			[]TaskEvent{{Kind: TaskStarted, Session: session, Task: "b578qoc1g", ToolUse: "toolu_01"}}},
		{"a start that names no session", `{"type":"system","subtype":"task_started","task_id":"b1",` +
			`"tool_use_id":"toolu_9","task_type":"local_bash"}`,
			[]TaskEvent{{Kind: TaskStarted, Task: "b1", ToolUse: "toolu_9"}}},
		{"a backgrounded command is ignored", `{"type":"system","subtype":"task_started","task_id":"b2",` +
			`"tool_use_id":"toolu_3","task_type":"local_bash","is_backgrounded":true}`, nil},
		{"another task type is ignored", `{"type":"system","subtype":"task_started","task_id":"a1",` +
			`"tool_use_id":"toolu_4","task_type":"local_agent","is_backgrounded":false}`, nil},
		{"a start with no task id is ignored", `{"type":"system","subtype":"task_started",` +
			`"task_type":"local_bash"}`, nil},
		{"a command ends", lineTaskNotification,
			[]TaskEvent{{Kind: TaskEnded, Task: "b578qoc1g", ToolUse: "toolu_01"}}},
		{"tool results answer their calls", lineToolResults, []TaskEvent{
			{Kind: TaskAnswered, ToolUse: "toolu_01"}, {Kind: TaskAnswered, ToolUse: "toolu_02"},
		}},
		{"a prompt as a plain string", `{"type":"user","message":{"role":"user","content":"tool_result"}}`, nil},
		{"another system event", `{"type":"system","subtype":"task_updated","task_id":"b578qoc1g"}`, nil},
		{"a system event of no task", `{"type":"system","subtype":"api_retry","attempt":1}`, nil},
		{"an assistant turn", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01",` +
			`"name":"Bash","input":{"command":"go test ./..."}}]}}`, nil},
		{"the result", `{"type":"result","subtype":"success","session_id":"` + session + `"}`, nil},
		{"not json", `task_started "system"`, nil},
		{"empty", ``, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Without a command to answer, a line yields what it would
			// otherwise, its answers aside.
			var unanswered []TaskEvent
			for _, ev := range tt.want {
				if ev.Kind != TaskAnswered {
					unanswered = append(unanswered, ev)
				}
			}
			for _, h := range []TaskWatcher{NewClaude(), NewFake()} {
				if got := h.ScanTasks([]byte(tt.line), true); !reflect.DeepEqual(got, tt.want) {
					t.Errorf("%T.ScanTasks(answers) = %+v, want %+v", h, got, tt.want)
				}
				if got := h.ScanTasks([]byte(tt.line), false); !reflect.DeepEqual(got, unanswered) {
					t.Errorf("%T.ScanTasks(no answers) = %+v, want %+v", h, got, unanswered)
				}
			}
		})
	}
}

// TestScanTasksPassesOverWithoutADecode: the runner's reading goroutine
// hands every stream line to ScanTasks, so a line that cannot carry a
// command event costs no decode: a tool result while no command waits on
// its answer, and any line naming neither init nor a task.
func TestScanTasksPassesOverWithoutADecode(t *testing.T) {
	c := NewClaude()
	for _, line := range []string{
		lineToolResults,
		`{"type":"system","subtype":"api_retry","attempt":1}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading."}]}}`,
	} {
		b := []byte(line)
		if allocs := testing.AllocsPerRun(50, func() { c.ScanTasks(b, false) }); allocs != 0 {
			t.Errorf("ScanTasks(%.50s, no answers) allocates %.0f times, want none: no decode", line, allocs)
		}
	}
	b := []byte(lineToolResults)
	if allocs := testing.AllocsPerRun(10, func() { c.ScanTasks(b, true) }); allocs == 0 {
		t.Error("ScanTasks of a tool result with answers wanted did not decode it")
	}
}

func TestTaskWatcherIsClaudeStreamOnly(t *testing.T) {
	for _, h := range All() {
		_, ok := h.(TaskWatcher)
		if want := h.ID() == "claude" || h.ID() == "fake"; ok != want {
			t.Errorf("%s implements TaskWatcher = %v, want %v", h.ID(), ok, want)
		}
	}
}

func TestTaskOutputGlob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI's output file is a Linux pod path")
	}
	const session, task = "5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c", "b578qoc1g"
	tests := []struct {
		name          string
		env           []string
		session, task string
		want          string
	}{
		{"the default", []string{"HOME=/workspace", "TMPDIR=/var/tmp"}, session, task,
			"/tmp/claude-65532/*/" + session + "/tasks/" + task + ".output"},
		{"moved", []string{"CLAUDE_CODE_TMPDIR=/scratch/"}, session, task,
			"/scratch/claude-65532/*/" + session + "/tasks/" + task + ".output"},
		{"the last assignment wins", []string{"CLAUDE_CODE_TMPDIR=/a", "CLAUDE_CODE_TMPDIR=/b"}, session, task,
			"/b/claude-65532/*/" + session + "/tasks/" + task + ".output"},
		{"emptied is the default", []string{"CLAUDE_CODE_TMPDIR=/a", "CLAUDE_CODE_TMPDIR="}, session, task,
			"/tmp/claude-65532/*/" + session + "/tasks/" + task + ".output"},
		{"a pattern in the directory is quoted", []string{"CLAUDE_CODE_TMPDIR=/t*p[1]"}, session, task,
			`/t\*p\[1]/claude-65532/*/` + session + "/tasks/" + task + ".output"},
		{"a relative directory", []string{"CLAUDE_CODE_TMPDIR=tmp"}, session, task, ""},
		{"no session", nil, "", task, ""},
		{"a session leaving its segment", nil, "../etc", task, ""},
		{"a task that is a pattern", nil, session, "*", ""},
		{"a task leaving its segment", nil, session, "a/b", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewClaude().TaskOutputGlob(tt.env, 65532, tt.session, tt.task); got != tt.want {
				t.Errorf("TaskOutputGlob = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTaskOutputGlobFindsTheFile lays the CLI's file out as it does, under
// the slug of a working directory with dots in it, and finds it by the
// pattern alone.
func TestTaskOutputGlobFindsTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI's output file is a Linux pod path")
	}
	tmp := t.TempDir()
	const session, task = "5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c", "b578qoc1g"
	dir := filepath.Join(tmp, "claude-65532", "-workspace-repo--git", session, "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, task+".output")
	if err := os.WriteFile(want, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pattern := NewClaude().TaskOutputGlob([]string{"CLAUDE_CODE_TMPDIR=" + tmp}, 65532, session, task)
	got, err := filepath.Glob(pattern)
	if err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("Glob(%q) = %v, %v; want [%s]", pattern, got, err, want)
	}
	if other := NewClaude().TaskOutputGlob([]string{"CLAUDE_CODE_TMPDIR=" + tmp}, 65532, session, "b2"); other != "" {
		if got, _ := filepath.Glob(other); len(got) != 0 {
			t.Errorf("another task's pattern found %v", got)
		}
	}
}
