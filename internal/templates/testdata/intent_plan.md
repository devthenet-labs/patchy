<!-- patchy:plan patchy/target-1 r2 sha256:05126c8813d2 -->
## Plan r2

**Summary:** Add GET /version returning {sha, built} as JSON

## Approach

Add a `GET /version` handler in `internal/server` that returns the commit SHA and build time, set with `-ldflags` at build time.

## Steps

1. Add `version.go` with `Commit` and `Built` variables.
2. Register the handler next to `/healthz`.

## Test plan

- a handler test asserting the JSON shape
- `go test ./...`

## Risks

A new public endpoint; it reveals the deployed commit, which is public anyway.

---

### Plan data

The plan's frontmatter, as the build agent reads it:

```yaml
---
summary: Add GET /version returning {sha, built} as JSON
repositories: [patchy-target]
new_dependencies: []
questions: []
confidence: high
estimated_max_turns: 60
estimated_token_budget: 300000
---
```

### To approve

Add the `patchy:approved` label to this issue, or comment `/patchy approve`. patchy then builds exactly this plan: r2, `sha256:05126c8813d2`. If this comment or the issue description is edited after patchy posted the plan, the approval is refused and patchy asks for a new plan.

For a different plan, say what should change in a comment, then re-apply the `patchy:target` label or comment `/patchy replan`. `/patchy cancel` stops work on this intent.
