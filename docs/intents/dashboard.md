# The intents dashboard

The status page can show every intent a person may see: a board of intents by phase, each intent's timeline, and a panel
for each agent run that answers the three questions a running intent raises: is it stuck, what has it spent, and when
will it stop. It is read-only. Approval stays a GitHub fact (the approve label or `/patchy approve` on the intent issue,
bound to the plan's digest), and nothing on the dashboard can approve, cancel or replan an intent.

It is off by default. Turning it on adds per-Project access, mandatory identity hardening and a stricter browser
envelope for the whole status page; read this page before you do.

## What it shows

**The board** has a column per stage: planning, awaiting approval, building, in review (revise and check-fix rounds
included), done (merged, closed, failed, until the intent expires). A Blocked intent stays in the column of the phase it
was blocked from, with a badge and the reason. A card shows the issue, the plan's one-line summary, the repositories,
every agent run that is pending or running (a multi-repository build runs one per repository at once), the attempt of
the newest run against the two each round gets, revisions and CI-fix rounds against the Project's limits, recorded spend
against the cost ceiling, its age and time in phase, and its pull request and preview links. A suspended intent is
badged: it is not reconciled while suspended, so its status may be stale.

**The timeline** of one intent lists its phase changes; the plan revision and digest, with a link to the plan comment;
the accepted approval with who approved, how (label or command), the plan revision and both digests it is bound to; the
pull requests; the preview; and every run with its stage, round, attempt, outcome, duration, recorded cost and image
(source and digest).

**The run panel** shows how long the run has taken against its stage time limit, the Job's deadline and the idle
watchdog's limit (20 minutes without a model turn or tool result, by default), which tool the agent called last and has
no result for yet and for how long, the last activity, and every condition that stops the run, in plain English. The
output-token kill switch is listed as what it is: a count of streamed output tokens, not a bound on spend. The cost
ceiling is checked only before the next launch, so a running run can pass it; the panel says so. A run still waiting
for a run slot can be opened too: its panel follows it live from the moment its agent starts.

Spend on the dashboard is recorded spend only: the cost each run reported when it was collected. A run's spend appears
when it ends. There are no live token or dollar figures yet.

### What it never shows

Run details and condition messages can quote a scheduler or kubelet message naming private nodes, or an image reference
carrying a private registry host and account ID. The dashboard shows neither. Outcomes and block reasons are fixed
sentences keyed by patchy's own vocabulary (`evicted: the agent pod was evicted`,
`the repository's agent image was rejected`); images appear as their source and digest, never their registry. Raw
`kubectl describe` remains the operator's tool for the rest.

Agent text (plans, reports, transcripts, the plan summary) and issue text are shown as plain text only, framed as
written by the agent, never rendered as markdown and never turned into links. Characters that render as nothing (bidi
and zero-width controls, tag characters, terminal escapes) are shown as their code point, `[U+202E]`. Links come only
from fields the controllers write, and only as `https`.

## Two read tiers, per Project

Access is decided per Project by Kubernetes RBAC, through SubjectAccessReviews the status server runs for the signed-in
user, on two **virtual** subresources of `projects.patchy.bitwisemedia.uk`:

| Grant                           | Shows                                                                                  |
| ------------------------------- | -------------------------------------------------------------------------------------- |
| `get` on `projects/intents`     | The board, timelines and run panels: phases, durations, recorded cost, outcomes, links |
| `get` on `projects/transcripts` | With `projects/intents`: plan text, run reports and transcripts, persisted and live    |

Nothing on the API server serves these subresources, so the rules grant `kubectl` nothing; the status server's reviews
are their only reader. The tiers nest: `projects/transcripts` without `projects/intents` opens nothing. A rule without
`resourceNames` covers every Project; `resourceNames` scopes it to the Projects named:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: patchy-intents-content-storefront
  namespace: patchy
rules:
  - apiGroups: [patchy.bitwisemedia.uk]
    resources: [projects/intents, projects/transcripts]
    resourceNames: [storefront] # the Project's name
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: patchy-intents-content-storefront
  namespace: patchy
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: patchy-intents-content-storefront
subjects:
  - kind: Group
    apiGroup: rbac.authorization.k8s.io
    name: github:acme:storefront # a prefixed org:team group
```

`deploy/kustomize/base/rbac.users.example.yaml` has the example roles and bindings, and the chart renders the two
every-Project ClusterRoles (`<release>-intents-viewer`, `<release>-intents-content`) with `statusServer.rbac.userRoles`.

A Project a person may not see is invisible: its intents are absent from the board, and asking for one of them, its runs
or its plans answers 404, exactly as for an intent that does not exist. A person who may see a Project but not its
transcripts gets a 403 for plan text and no report or conversation in the run panel. The live conversation is stripped
on the server for each viewer, so two people watching the same run at different tiers share one log follow and still
receive different streams.

!!! warning "Native RBAC bypasses the tiers"

    The tiers bind the dashboard only. Plans and transcripts are stored in ConfigMaps in the release namespace, and raw
    agent output is in the agent pods' logs. Anyone with `get` on `configmaps` in `patchy` reads every Project's plans
    and transcripts (and every Finding's), and anyone with `get` on `pods/log` in `patchy-agents` reads the raw agent
    output, which carries more than the scrubbed transcript does. The built-in `view`, `edit` and `admin` roles include
    both. Give a per-Project viewer only the virtual subresources above, never native reads in these namespaces, and
    check who holds them:

    ```sh
    kubectl auth can-i get configmaps -n patchy --as <user> --as-group <group>
    kubectl auth can-i get pods/log -n patchy-agents --as <user> --as-group <group>
    ```

Grants are by Project **name**. A Project deleted and created again under the same name while the old one's intents are
still within their TTL hands those intents to the new Project's viewers; give a Project a new name rather than reuse
one.

## Identity: OIDC with prefixes

The dashboard shows private code, so it runs only under sign-in mode `oidc`, with both claim prefixes set. The server
refuses to start otherwise (and the chart refuses to render an inline `statusServer.auth.config` that is not mode `oidc`
with both prefixes; an `existingSecret` is checked by the server alone):

- modes `none` and `anonymous` are refused: the first bypasses authorization for every visitor, the second gives every
  visitor one identity's grants. For local development only, `--intents-dev-insecure` lets mode `none` run the views on
  a loopback `--listen-addr`.
- `oidc.claims.usernamePrefix` and `oidc.claims.groupsPrefix` are required. Without them a RoleBinding written for a
  dashboard user also matches any cluster identity of the same name (an EKS access entry, another provider's user), and
  a provider group called `system:masters` would reach the access review as exactly that. A prefix inside `system:` is
  refused.
- When the username claim is `email`, `oidc.claims.requireVerifiedEmail: true` is required too, and a token whose
  `email_verified` claim is not true signs no one in.

The prefixes apply to **every** access review the status server runs, the findings page's included. Setting them renames
every identity: rebind the findings roles to the prefixed names in the same change, or their users lose the findings
page.

### Dex with GitHub

[Dex](https://dexidp.io) with its GitHub connector signs people in with their GitHub account and issues `org:team`
groups, which lines dashboard identities up with the GitHub logins in a Project's approvers. A Dex static client for the
status page:

```yaml
staticClients:
  - id: patchy-status
    name: patchy status
    secretEnv: PATCHY_STATUS_CLIENT_SECRET
    redirectURIs: [https://status.patchy.example.com/oauth2/callback]
connectors:
  - type: github
    id: github
    name: GitHub
    config:
      clientID: $GITHUB_CLIENT_ID
      clientSecret: $GITHUB_CLIENT_SECRET
      redirectURI: https://dex.example.com/callback
      orgs:
        - name: acme # only members sign in; teams become acme:<team>
      teamNameField: slug
```

and the status server's auth config (`statusServer.auth.existingSecret`, key `config.yaml`):

```yaml
mode: oidc
oidc:
  issuerURL: https://dex.example.com
  clientID: patchy-status
  clientSecretFile: /etc/patchy/auth/client-secret # or clientSecret
  redirectURL: https://status.patchy.example.com/oauth2/callback
  scopes: [openid, offline_access, profile, email, groups]
  claims:
    username: preferred_username # the GitHub login
    groups: groups # acme:<team>
    displayName: name
    usernamePrefix: "github:"
    groupsPrefix: "github:"
```

Bindings then name `github:<login>` and `github:acme:<team>`.

The username here is the GitHub login, not an email, so `email_verified` is not what proves who signed in, and the
server does not require it. What does: Dex authenticated the person with GitHub and asserts the login GitHub returned;
the `orgs` restriction admits only members of your organization; and the mandatory prefix keeps the login apart from
every cluster identity. GitHub logins can be renamed, and a released login can be registered by someone else, so bind
teams (which only organization owners control) rather than individual logins where you can. If you bind by email instead
(`username: email`), set `requireVerifiedEmail: true`; the server insists.

Revoking access: an RBAC change takes effect within 20 seconds, on open streams too (they re-check every 20 seconds and
end when the grant is gone). Removing someone from a team at GitHub takes effect only when their ID token is next
refreshed through Dex; a status page session lasts up to `sessionDuration` (7 days by default) through refresh tokens.
Shorten `sessionDuration` if that is too long.

## Turning it on

With the chart:

```yaml
statusServer:
  intents:
    enabled: true
  auth:
    existingSecret: patchy-status-auth # mode oidc with both prefixes, as above
  rbac:
    userRoles: true # optional: the two example ClusterRoles
```

With kustomize, add the component to your overlay:

```yaml
components:
  - ../../components/status-intents
```

Either grants the status server read (`get`, `list`, `watch`) on projects, intents, intentruns and previews in the
release namespace, and `get` on Jobs in the agent namespace, for a live run's clock (it reads the Job's creation and
start time, deadline and idle limit, and never passes the Job itself on). It writes nothing new: no intent, no status,
no Secret.

### What else changes on the page

With the dashboard on, the whole status page runs a stricter browser envelope, because preview apps run pull request
code on hosts that are same-site with the status host, and same-site requests carry the session cookie:

- every `/api` request and the `/events` stream refuse `Sec-Fetch-Site: same-site` and `cross-site`;
- every write (`POST`) also refuses a request with no `Sec-Fetch-Site` at all, so scripts that post findings actions
  without a browser must send `Sec-Fetch-Site: same-origin`;
- streams are `Cache-Control: no-store`;
- a Content-Security-Policy whose `script-src` admits only the page's own inline scripts, by hash, and
  `Strict-Transport-Security`;
- the findings transcript streams re-check the viewer's grant every 20 seconds too;
- the public `/events` stream takes at most 512 subscribers; one person holds at most 6 intents streams at once, the
  server 256.

Every intents stream ends after 10 minutes and the page reopens it through the current session, so a signed-out or
expired session stops streaming within that time. The change signal the board listens on reaches a person only for the
Projects they can see.

Each read of tier 2 content (a plan, a report, a transcript) is logged as one line, `intent content read`, with the
user, Project, intent, run and what was read, never the content.

Completed intents expire with their runs after intent-controller's TTL (14 days by default), and the dashboard forgets
them with them.
