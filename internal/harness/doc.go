// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package harness abstracts the coding-agent CLIs patchy drives. A harness
// builds the argv for one prompted run (PromptSpec), the runner package
// executes it, and the harness parses the CLI's stream-json stdout back into
// an AgentResult — harnesses never touch os/exec themselves, so every caller
// can be tested against captured output.
//
// Four harnesses register: Claude (the claude CLI, patchy's default agent),
// Codex (the codex CLI, driving OpenAI models), Copilot (the copilot CLI, which
// brokers both vendors' models and so is every model's fallback rather than any
// model's preferred harness) and Fake (replays a fixture file through cat, for
// tests and the agent-runner's --fake mode).
//
// Claude and Fake can also continue a session an earlier run left behind
// (Resumer), which agentrun's report repair is built on; codex and copilot
// cannot, so a stage on them keeps a refused report refused.
package harness
