<!-- patchy:plan patchy/target-1 r2 sha256:5f051fa7769d -->
## Plan r2

**Summary:** Show the API's greeting on the home page

**Repositories:** patchy opens one pull request in each of: `devthenet-labs/marigold-web`, `devthenet-labs/Acme.Web_App`.

### Before you approve

Each build runs offline, in the image its own repository declares, so it cannot fetch anything new. This plan adds dependencies: add each one to the image of the repository that needs it, on that repository's default branch, before you approve, or its build will fail.

- `github.com/acme/jsonx v1.2.0 (devthenet-labs/Acme.Web_App)`

### To approve

Add the `patchy:approved` label to this issue, or comment `/patchy approve`. patchy then builds exactly the plan below: r2, `sha256:5f051fa7769d`. If this comment or the issue description is edited after patchy posted the plan, the approval is refused and patchy asks for a new plan.

For a different plan, say what should change in a comment, then re-apply the `patchy:target` label or comment `/patchy replan`. `/patchy cancel` stops work on this intent.

### The plan

This is the planner's report exactly as the build agent reads it, byte for byte, with nothing in it rendered or left out. GitHub does not wrap it, so scroll it sideways to read a long line to its end. Its digest is `sha256:5f051fa7769d869ea3c54312cc94f750c7f5b67ff9b871fae9f89999e4da4342`.

```markdown
---
summary: "Show the API's greeting on the home page"
repositories:
  - "https://github.com/devthenet-labs/marigold-web"
  - "https://github.com/devthenet-labs/Acme.Web_App"
new_dependencies: []
questions: []
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

## Approach

The API serves `GET /api/greeting` as `{"message": string}`; the page fetches it same-origin.

## Steps

### https://github.com/devthenet-labs/Acme.Web_App

1. Add the handler.

### https://github.com/devthenet-labs/marigold-web

1. Fetch `/api/greeting` and fill `#greeting`.
```
