# GitHub code scanning

GitHub is the only **two-way** source: findings come in from code scanning, and patchy writes back — tracking issues,
dismissals, remediation branches and pull requests. Everything else in the system is one-way.

!!! info "One provider, two resources"

    This page is the GitHub **Integration** — alerts in, tracking issues and dismissals out. Repository clone and
    push access is the separate [GitHub **Forge**](../forges/github.md); one GitHub App can back both. The
    [integrations overview](../index.md) maps every provider's roles.

If you have not registered the App yet, start at [Create the GitHub App](../../getting-started/github-app.md); this page
is about what the resulting resource does.

## How alerts arrive

`POST /github/webhooks` is the one URL the App delivers to. Each delivery's `X-Hub-Signature-256` is HMAC-validated
against the `webhookSecret` of every configured `Integration`, before any handling — see
[integration-controller](../../configuration/integration-controller.md#the-webhook-receiver) for the response codes and
the delivery-queue behaviour.

```yaml
apiVersion: patchy.bitwisemedia.uk/v1alpha1
kind: Integration
metadata:
  name: github
  namespace: patchy
spec:
  provider: github
  secretRef:
    name: patchy-github # appID + privateKey (or token), plus webhookSecret
  interval: 10m # credential revalidation
  github:
    # baseURL: https://ghes.example.com/api/v3   # GHES only
    codeScanningAlerts:
      enabled: true # ingestion, and dismissal on an "ignore" verdict
    issues:
      enabled: true # the tracking projection and the human signals back
      approveComment: /approve
```

Each capability block is independent. An `Integration` with only `codeScanningAlerts` ingests findings and never opens
an issue; one with only `issues` is a tracking surface for findings that arrived from somewhere else — which is exactly
how a [Google Cloud SCC](google-scc.md) estate gets its tracking issues.

Only `created` and `reopened` alert actions produce a finding. `fixed`, `closed_by_user`, `appeared_in_branch` and the
rest are state GitHub already manages. Only alerts on the repository's default branch are ingested: an alert whose most
recent instance is on another branch (including patchy's own `patchy/<finding>` remediation branches) or on a
pull-request ref (`refs/pull/<n>/merge`) is skipped, so a fix CodeQL rejects on its PR cannot loop back in as a new
finding.

## What patchy keeps

Unlike an SCC notification, the webhook payload is only a summary — the rule help markdown comes from the API — so
ingest fetches the full alert before building the `Finding`:

| Finding field             | From                                                                      |
| ------------------------- | ------------------------------------------------------------------------- |
| `spec.repository`         | the delivery's repository owner and name                                  |
| `spec.advisories`         | GHSA ids, then CVEs, then CWEs from the CodeQL rule tags                  |
| `spec.alerts[].id`        | the alert number; `.url` is its HTML URL                                  |
| `spec.alerts[].locations` | the alert's path, line range and snippet                                  |
| `spec.severity`           | `security_severity_level`, or the raw rule severity mapped onto the scale |
| `spec.description`        | the rule description and help text                                        |

Advisories are ordered most-authoritative-first, and a rule with no recognizable advisory falls back to `rule:<rule id>`
so accumulation still has a stable key. Alerts sharing an advisory family against the same repository fold into one
`Finding` until the accumulation window closes.

## What patchy writes back

Three write paths, each gated differently:

- **The tracking issue.** A Finding is projected to an issue: templated body, the
  [projected labels](../../labels.md#the-projected-labels), enrichments and reports as comments, and open/closed state.
  Strictly one-way — issue state is never parsed back into pipeline state, only the explicit signals below are.
- **Alert dismissal.** An `ignore` verdict closes the code-scanning alert as a false positive. This is the write-back
  half of the `codeScanningAlerts` capability; the SCC analogue (muting) is
  [not yet implemented](google-scc.md#what-is-not-implemented).
- **The remediation branch and pull request.** Pushed through the [`Forge`](../forges/github.md), not the `Integration`
  — a different credential and a different controller.

### The fallback repository

A cloud finding that [never resolves a repository](../enhancers/index.md#when-no-repository-resolves) has nowhere
natural to carry its tracking issue. `github.issues.fallbackRepository` (`"owner/repo"`) names one: findings with no
repository of their own are projected there instead, so they stay visible to humans. They still cannot be remediated —
the issue is a human surface, not a work tree.

### Human signals

These are the only things on GitHub that move `Finding` state:

| Signal                                   | Effect                      |
| ---------------------------------------- | --------------------------- |
| Issue closed                             | → `HandedOff`               |
| Issue reopened                           | `Dismissed` → `HandedOff`   |
| `/approve` comment                       | recorded on `spec.approval` |
| PR on `patchy/<finding>` merged          | `InReview` → `Remediated`   |
| PR on `patchy/<finding>` closed unmerged | → `Failed`                  |

A PR close counts only when it is the PR the finding recorded — the same number, in the finding's repository, from a
branch there, not a fork's — so a stray branch named `patchy/<finding>` moves nothing. The PR body's `Fixes #N` closes
the tracking issue as the PR merges, and the two deliveries can arrive in either order: an issue closed while its
finding is `InReview` is checked against the PR first. Merged or closed, the PR's outcome applies; still open, or one
GitHub answers 404 or 403 for, means a human closed the issue (`HandedOff`) — provided GitHub still reports the issue
closed, since a close and a quick reopen can be delivered reopen first. Renaming the repository during review is covered
too: a close from the new name is checked against the recorded PR, which GitHub still serves under the old name. A
transfer to another owner is covered only while the App's installation on the old owner (or the token) can still read
the repository; otherwise GitHub answers 404, the close moves nothing, and the finding stays `InReview` for a human to
close out. Until GitHub answers, the close waits on the finding as the `ReviewClosePending` condition and is retried, so
an API outage delays it rather than losing it. Suspending the Integration, turning its issues off, or deleting it holds
the close too: it settles once an issues-enabled Integration is back.

`approveComment` changes the command; it defaults to `/approve`. Who may approve is RBAC on the status page and the CLI,
but on an issue it is whoever can comment — so treat the comment as a convenience for repositories whose write access
already matches your approval policy.

## Credentials

One Secret, which the [Forge](../forges/github.md#credentials) can reference too. Two accepted shapes:

| Keys                   | Use                                                             |
| ---------------------- | --------------------------------------------------------------- |
| `appID` + `privateKey` | GitHub App — short-lived, single-repository installation tokens |
| `token`                | A personal access token, for development                        |

Plus `webhookSecret` on the `Integration`'s Secret, for receiver HMAC validation.

Prefer the App. Installation tokens are minted per repository per operation and expire in an hour; a PAT is long-lived,
carries its owner's whole access, and cannot use the delivery sweep below. Each resource revalidates its Secret on
`spec.interval` and reports the result on its `Ready` condition, so a rotated-away credential surfaces as a condition
rather than as a failed run.

Besides code scanning alerts and issues, the credential needs **Contents: read** — even when a separate App backs the
[Forge](../forges/github.md#credentials). Ingest compares commits (GitHub's compare API) to recognize a reopen of a
remediated alert that observed code the merged fix already replaced, and to confirm the fix is still on the branch.
Without the permission GitHub refuses every such lookup on a private repository, and the check fails open: each of those
reopens opens a successor Finding, as if the check did not exist, and the `Integration` reports `CommitAncestry=False`
(reason `ContentsReadDenied`) until a lookup succeeds. It is a separate condition from `Ready` because ingestion itself
keeps working.

## The failed-delivery sweep

GitHub does not retry a failed webhook delivery. Anything missed while the receiver was down, or while its queue was
full, sits in the 30-day delivery log until someone redelivers it by hand.

```yaml
spec:
  github:
    redelivery:
      enabled: true
      lookback: 24h # bounded by GitHub's 30-day retention
```

On each reconcile interval the controller lists recent deliveries and asks GitHub to redeliver those that never got a
2xx. **App credentials are required** — the delivery log is App-scoped, so a PAT integration cannot sweep.

## The manual backfill

The sweep can only re-send deliveries that happened. Alerts raised _before_ the App was installed on (or associated
with) a repository never produced a delivery, so no replay finds them. The backfill does: a one-shot walk of the
provider's **open** alerts through the list API, ingesting each one exactly as a webhook delivery would (idempotent —
alerts that already have findings fold in).

Request it by stamping `spec.backfill` — from the status page's configuration view, with
`patchy backfill <integration>`, or directly. The RBAC verb is `backfill` on `integrations` (enforced by the
[admission policy](../../deployment/kustomize.md) for every client). The controller consumes the request once, echoes it
on `status.backfill.backfilledAt`, and reports the walk (`listed`, `ingested`, `truncated`, `error`) beside it.

```yaml
spec:
  backfill:
    by: op@example.com
    at: "2026-08-14T12:00:00Z"
    repositories: ["acme/", "acme-labs/tool"] # optional; empty = full scope
```

The semantics, in brief:

- **Filter entries are prefixes** of `owner/name`: `acme/` covers an owner, `acme/service-` a name prefix, `acme/shop`
  one repository (and any name extending it). Matching is case-insensitive. Empty means everything the credential can
  see.
- **App credentials** fan out across the App's installations: organization accounts through the org-wide alert listing,
  user accounts (and any account whose filter entries all name repositories) by enumerating the installation's
  repository inventory and walking only matching repositories.
- **PAT credentials** cannot discover repositories, so every filter entry must be an exact `owner/name`; a bare prefix
  is reported as an error on `status.backfill.error`.
- **The walk is bounded** (~10K alerts per pass, plus a repository-inventory cap). A pass that exceeds it sets
  `status.backfill.truncated`; re-request with a narrower prefix to cover the rest — there is no cursor.
- **Only open alerts** are listed; dismissed and fixed alerts are not backfilled.
- The list API omits the rule-help markdown a webhook-driven ingest fetches per alert, so backfilled findings fall back
  to the rule description. Everything else — accumulation, labels, tracking issues — behaves identically.

## GitHub Enterprise Server

Point the resource at your instance and everything else is identical (the
[Forge](../forges/github.md#github-enterprise-server) takes the same field):

```yaml
spec:
  github:
    baseURL: https://ghes.example.com/api/v3
```

## Egress proxy

`spec.github.proxy` routes all of this integration's GitHub traffic — issue projection, alert fetches and dismissals,
the delivery sweep, the backfill — through an HTTP/HTTPS forward proxy, the pattern for GitHub Enterprise Cloud
organizations behind an IP allowlist:

```yaml
spec:
  github:
    proxy:
      url: http://proxy.corp.example:3128
```

Basic-auth credentials go in the credential Secret under the optional keys `proxyUsername` and `proxyPassword`, never in
the URL. A `spec.github.proxy` overrides the process `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` environment for this
integration's traffic; unset, the environment applies.

!!! warning "Set the proxy on the Forge too"

    The integration-controller authenticates through the **Integration** — its own Secret and `baseURL`, not the
    Forge's — so a proxy on the [Forge](../forges/github.md#egress-proxy) alone leaves this resource's calls (the
    backfill's `list installations`, for one) outside the allowlist. An allowlisted estate must set the proxy on
    **both** resources.
