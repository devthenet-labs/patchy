# status-server

The status page backend: the embedded web dashboard over the `Finding` state machine and the `FindingRollup` statistics,
the sign-in surface, and the three human actions (approve, suspend, resume). It is a server, not a controller — it runs
no reconcilers and takes no leases; a controller-runtime cache gives it live watches, and an SSE stream tells open
browsers to refetch when anything changes.

```sh
status-server serve --namespace patchy --auth-config /etc/patchy/auth/config.yaml
```

The exposure contract is deliberately asymmetric — see [the status page](../status-ui.md) for the UI tour and the RBAC
grammar:

- **Rollup statistics are public.** `GET /api/rollups` (and the SSE stream) serve without a session.
- **The findings surface always requires authentication.** `GET /api/findings` (the trimmed list projection),
  `GET /api/findings/{name}` (one finding's full detail — description, alerts, enrichments, the phase log, and the run
  reports the list omits), and every action `POST` demand a signed-in identity whose RBAC passes the corresponding
  access review. With no auth config at all the server runs in the _unconfigured_ posture: rollups only, and the page
  explains that sign-in is not configured.
- **Agent conversations ride the findings gate.**
  `GET /api/findings/{name}/runs/{investigation|remediation}/{attempt}/transcript` is Server-Sent Events in both modes:
  a finished run replays its stored ConfigMap, a running one streams from the agent's pod log. Following a live run
  needs `pods` and `pods/log` read in `--agent-namespace` (through the API server — the server never dials an agent
  pod); without that grant, completed runs' conversations still serve. See
  [Agent conversations](../status-ui.md#agent-conversations).

## Flags

The [shared flags](index.md#shared-flags-every-controller) (`--listen-addr` is the page's own address here — there is no
webhook), plus:

| Flag                | Env                      | Default         | Purpose                                                                              |
| ------------------- | ------------------------ | --------------- | ------------------------------------------------------------------------------------ |
| `--namespace`       | `PATCHY_NAMESPACE`       | `POD_NAMESPACE` | Namespace the Findings and FindingRollups live in                                    |
| `--agent-namespace` | `PATCHY_AGENT_NAMESPACE` | `patchy-agents` | Namespace the agent Jobs run in; live conversations are followed from their pod logs |
| `--kubeconfig`      | `PATCHY_KUBECONFIG`      | in-cluster      | Kubeconfig path for running outside the cluster                                      |
| `--health-addr`     | `PATCHY_HEALTH_ADDR`     | `:8081`         | healthz/readyz probe listen address                                                  |
| `--auth-config`     | `PATCHY_AUTH_CONFIG`     | _(unset)_       | Mounted authentication config; absent ⇒ rollups-only (see below)                     |

Two more flags turn on the [intents dashboard](../intents/dashboard.md):

| Flag                     | Env                           | Default | Purpose                                                                                                                                        |
| ------------------------ | ----------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `--intents-enabled`      | `PATCHY_INTENTS_ENABLED`      | `false` | Serve the read-only intents views with per-Project access reviews. Needs mode `oidc` with both claim prefixes, and hardens the page (below)    |
| `--intents-dev-insecure` | `PATCHY_INTENTS_DEV_INSECURE` | `false` | Development only: let the intents views run in mode `none`, showing every Project to every visitor. Refused unless `--listen-addr` is loopback |

With `--intents-enabled` the server refuses to start in mode `none` (without the development flag) or `anonymous`, with
no auth config, or in mode `oidc` without `claims.usernamePrefix` and `claims.groupsPrefix` (and
`claims.requireVerifiedEmail` when the username claim is `email`). It also runs a stricter browser envelope for the
whole page: `Sec-Fetch-Site` same-site and cross-site refused on every `/api` request and `/events`, a write without the
header refused, streams `no-store`, open findings transcript streams re-authorised every 20 seconds, a cap on `/events`
subscribers, a Content-Security-Policy and HSTS. See the [dashboard page](../intents/dashboard.md) for what it shows,
its RBAC and its limits. With the flag off the server behaves exactly as before it existed.

## Authentication configuration

`--auth-config` points at a YAML file, conventionally a mounted Secret (`patchy-status-auth`, key `config.yaml` — the
deployments mount it `optional`, so removing the Secret degrades to rollups-only rather than failing the pod). A
present-but-invalid file is a startup error: a broken configuration never silently downgrades to no authentication.

```yaml
mode: oidc # none | anonymous | oidc
sessionDuration: 168h # absolute session lifetime (default 7 days)
# insecure: true               # separate non-Secure cookies — plain-HTTP local dev ONLY
anonymous: # mode: anonymous only
  username: status-viewer
  groups: [patchy-viewers]
oidc: # mode: oidc only
  issuerURL: https://sso.example.com
  clientID: patchy-status
  clientSecret: "..." # or clientSecretFile: /path/to/projected/key
  # scopes: [openid, offline_access, profile, email, groups]
  # authURLParams: {}        # extra authorize-endpoint query parameters
  # autoLogin: false         # bounce straight to the provider instead of the sign-in panel
  # redirectURL: ""          # override the derived <scheme>://<host>/oauth2/callback
  claims: # claim NAMES, mapped onto the identity
    username: email # the subject access reviews run for
    groups: groups
    displayName: name
    # usernamePrefix: "oidc:"     # prepended to the username before every access review
    # groupsPrefix: "oidc:"       # prepended to every group
    # requireVerifiedEmail: true  # refuse a token whose email_verified is not true
```

`usernamePrefix` and `groupsPrefix` work like kube-apiserver's `--oidc-username-prefix` and `--oidc-groups-prefix`: with
them, a binding written for a status page user cannot also match a cluster identity of the same name, and a provider
group named `system:masters` reaches the review as `oidc:system:masters`. A prefix inside `system:` is refused. They
rename every identity for every review, so rebind the existing roles to the prefixed names when you set them. They are
optional unless the intents views are on, and so is `requireVerifiedEmail`, which the views require when the username
claim is `email`. The evaluation API reads the same `claims` block and applies the same rules.

### Modes

- **`none`** — every request is a fixed development identity with authorization bypassed entirely. The dev overlay ships
  this; never expose it.
- **`anonymous`** — every request is the one configured identity, but access reviews still run: cluster RBAC for that
  username/groups decides what every visitor may see and do.
- **`oidc`** — the real SSO flow. The server itself is the OAuth2 client (authorization-code + PKCE); the SPA never sees
  a token.

### Sessions and cookies (mode `oidc`)

There is no server-side session store. The ID token, refresh token, and session start are sealed with AES-256-GCM — the
key is derived (HKDF-SHA256) from the OIDC client secret, so **rotating the client secret signs everyone out** — and
stored in chunked `HttpOnly` cookies (`__Host-patchy-auth`, `__Host-patchy-auth-1`, …). Every request re-verifies the ID
token; an expired token is renewed via the refresh token in place, but never past `sessionDuration` from the original
sign-in. Three small SPA-readable cookies carry no secrets: `__Host-patchy-auth-provider` (how sign-in works),
`__Host-patchy-auth-error` (the last failure), `__Host-patchy-auth-logout` (pauses `autoLogin` after an explicit
sign-out).

All production cookies, including the short-lived `__Host-patchy-oauth2-state` CSRF cookie, use `Secure`, `Path=/` and
no `Domain`. The browser-enforced `__Host-` prefix prevents a sibling preview host from injecting a parent-domain cookie
with the same name. Expiry and SPA deletion use the same attributes. See the
[cookie prefix requirements](https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#name-the-__host-prefix).

Upgrading from the old unprefixed names requires signing in again; an in-flight sign-in must also restart. Legacy
cookies are never accepted as a fallback. Old host-only cookies are expired when encountered, including the old OAuth
state's `/oauth2/` path. Parent-domain cookies cannot be reliably identified from a request and remain ignored.

For explicit `insecure: true` local development over **HTTP only**, both server and SPA use separate `patchy-dev-*`
names without `Secure`. Production never reads these names. Do not set `insecure` behind an HTTPS ingress; the SPA
selects its cookie namespace from the browser's protocol, while the server uses operator configuration, not forwarded
headers.

The callback URL is derived from `X-Forwarded-Proto` / `X-Forwarded-Host` (or the Host header), which assumes a trusted
fronting proxy — set `oidc.redirectURL` explicitly if yours cannot be trusted to strip those.

## Authorization

Per-user grants are ordinary Kubernetes RBAC, resolved server-side with `SubjectAccessReview`s for the signed-in user
(users need no kubeconfig and never talk to the API server):

- native `get` on `findings` gates **viewing** the findings surface;
- the **custom verbs** `approve`, `suspend`, `resume` on `findings.patchy.bitwisemedia.uk` gate the action buttons, one
  verb per button.

Grants are namespace-scoped and stamped into the payload as each finding's `userActions`; the client intersects them
with the finding's own state machine, and every `POST` is re-checked server-side. See
`deploy/kustomize/base/rbac.users.example.yaml` for ready-made viewer / approver / operator tiers.

The server's own ServiceAccount is deliberately narrow: `findings` read + **spec** write (approve records
`spec.approval`; suspend/resume toggle `spec.suspend` — it never writes `findings/status` and never moves a phase; the
owning controllers react to the spec change), `findingrollups` read, and cluster-scoped `subjectaccessreviews create`
for the reviews.
