# Intent-controller GitHub permission audit

Verified 2026-09-26 against installation 163854331. Tokens were minted for one repository and one permission; no
installation token, App JWT, private key, or signed log URL was printed or saved. The installation already grants
`issues:write`, `pull_requests:write`, `contents:write`, and `checks`, `statuses`, `actions` at read.

The resources were patchy-target PR #46 and patchy PR #63 (the latter already had a conversation comment). All reads
were non-mutating. Write probes deliberately omitted required fields or supplied an invalid enum/type: they could not
create a comment, reaction, review request, PR, commit, tree, blob, or ref. A 422 proves authorization reached
validation, not that a real write succeeded. No workflow files were touched.

## Results and call-site mapping

| Controller operation                                                       | Scope used after this fix                      | Live result                                                                    |
| -------------------------------------------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------ |
| PR conversation list/get, including feedback, commands and notice adoption | `pull_requests:read`                           | 200 for both; public reads also accepted `issues:read`                         |
| PR conversation GraphQL edit history                                       | `pull_requests:read`                           | 200, edit facts returned; public node also accepted `issues:read`              |
| Round notices and command replies                                          | `pull_requests:write`                          | Missing body: 422; `issues:write` returned 403                                 |
| Eyes reaction on a PR conversation comment                                 | `pull_requests:write`                          | Invalid enum: 422; `issues:write` returned 403                                 |
| Find/get PR, including merge/close checks before pushes                    | `pull_requests:read`                           | 200 for both                                                                   |
| Create PR                                                                  | `pull_requests:write`                          | Missing head/base: 422                                                         |
| List reviews and inline review comments                                    | `pull_requests:read`                           | 200 for both                                                                   |
| GraphQL review and inline-comment edit history                             | `pull_requests:read`                           | 200 with edit facts for both                                                   |
| Request reviewers                                                          | `pull_requests:write`                          | Invalid reviewers type: 422; `issues:write` returned 403                       |
| Repository/default branch                                                  | `contents:read` token's implicit metadata read | 200; accepted-permission header says metadata read                             |
| PR branch head and compare                                                 | `contents:read`                                | 200 for both                                                                   |
| Create blob/tree/commit/ref                                                | `contents:write`                               | Missing required fields: 422 for each                                          |
| Fast-forward PR branch ref                                                 | `contents:write`                               | Missing SHA with force=false: 422                                              |
| Check runs and annotations                                                 | `checks:read`                                  | 200 for both                                                                   |
| Commit statuses                                                            | `statuses:read`                                | 200                                                                            |
| Actions workflow jobs                                                      | `actions:read`                                 | 200                                                                            |
| Actions job logs                                                           | `actions:read`                                 | 302 to a signed log URL; redirect not followed or printed                      |
| Collaborator permission for approver authorization                         | `issues:read` token's implicit metadata read   | 200; accepted-permission header says metadata read                             |
| Rate budget                                                                | Existing repository-scoped read token          | 200; permissionless endpoint                                                   |
| App identity and installation/token resolution                             | App JWT                                        | App GET 200 (`patchy-devthenet`); installation GET 200; scoped token mints 201 |

`GetIssue`, label operations, issue events, plan/status/summary comments, comment edits and issue closure remain on the
intent **issue**, with `issues:read` or `issues:write`. No intent-controller call edits or deletes a PR conversation
comment, submits a review, replies to an inline comment, merges a PR, or closes a PR. Those unused operations were not
probed. The compare probe checked JSON rather than downloading a patch; the permission is attached to the endpoint.

## Boundary and limitations

GitHub shares the [issue-comment endpoints](https://docs.github.com/en/rest/issues/comments) between issues and PRs, but
the write permission follows the target resource. Even the live reaction response advertised only `issues=write` while
accepting the PR-scoped token and refusing the issue-scoped one. Endpoint names and headers alone are not proof.

The controller now exposes distinct PR conversation methods, even when a Project's intent issue and PR are in the same
repository. All PR comments, notices, reactions and edit-history reads use those methods. No token was widened to
include both permissions, and agent pods still receive no forge credential.

fakegithub requires `pull_requests` for PR-number and PR-comment-ID endpoints, including GraphQL nodes. It deliberately
does not model GitHub's public-read fallback: otherwise public fixtures could hide incorrect scoping for private
projects. The live probes did **not** prove that the public read endpoints reject an issue-scoped token; they did not.
Tests cover same-repository issue/PR separation, wrong-scope writes, reads, GraphQL and repository isolation.

Notice delivery retains the existing bot-authored marker as its idempotency key. Refusals, lost responses and failed
status writes are retried; a successful prior post is adopted, never posted twice. `roundNoticesThrough` records
delivery separately from revision spend, so repairing an old notice never launches an agent or spends `maxRevisions`.
Merge and cancel are observed before delivery retries, and terminal Intents are retained until their pending notices
settle.
