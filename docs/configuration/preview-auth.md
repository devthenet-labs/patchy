# preview-auth

The preview sign-in relay: the OpenID provider every preview host's load balancer signs viewers in through. It is
optional and off by default (the chart's `previewAuth.enabled`), chart-only (there is no kustomize component), and not a
controller: it has no reconcilers and no leader election, so every replica serves.

The relay exists because previews run agent-written code against staging data. Every preview Ingress gets the ALB's
`authenticate-oidc` action pointed at the relay, so nobody reaches a preview without signing in, and nothing the ALB
then forwards to the preview identifies the viewer or works anywhere else. The relay never touches the app's
`Authorization` header or its own login, and its cookies have names no app uses.

For the operator's path (Dex, viewers, the two-stage rollout, rotation, rollback and the allowlist), read
[Preview sign-in](../intents/preview-sign-in.md). This page is the reference.

## How a sign-in flows

```text
browser            preview ALB (slot s)                 preview-auth                     Dex (GitHub)
  GET https://demo-1.<suffix>/
                   no session cookie: 302 to
                   <relay>/authorize?client_id=patchy-preview-s<s>
                     &redirect_uri=https://demo-1.<suffix>/oauth2/idpresponse
  ---------------------------------------------------> 1. the Preview at label demo-1:
                                                          live, in slot s, its Project
                                                       2. no relay session: 302 to Dex ------> sign in at GitHub
  <--------------------------------------------------- 3. /dex/callback: ID token checked <--- (once per browser
                                                          for the relay's own client,          session)
                                                          relay session cookie set
                                                       4. access review: get on
                                                          projects/previews, name=<Project>
                                                       5. 302 to the callback with a code
                   /oauth2/idpresponse?code=...
                   POST <relay>/token (slot s's secret) --> 6. the code redeemed once (Lease
                                                          ledger), the Preview and the
                                                          access review checked again
                   GET <relay>/userinfo ----------------> 7. {"sub": "<pairwise>"} only
                   session cookie patchy-preview-s<s>-0..
  the app, with x-amzn-oidc-* headers holding only the pairwise sub and an opaque access token
```

A second preview in the same browser session skips Dex: the relay's own session cookie (a browser-session cookie,
sealed, at most `--preview-auth-session-max-age` old) signs the viewer in at once, after the access review. Every
refresh the ALB makes runs the Preview check and the access review again.

## What reaches the preview

Everything the ALB forwards is treated as published to the pull request's author:

- `sub` is `HMAC(pairwise key, client, Preview UID, user)`: stable for one viewer on one Preview, unrelated across
  Previews and slots, and never the GitHub login or Dex's subject.
- The ID token carries `iss`, `aud` (the slot's client), `sub`, `iat`, `exp`, `at_hash` and the nonce the ALB sent. It
  carries no name, email or group.
- `/userinfo` returns `{"sub": "..."}` and nothing else.
- The access token is sealed, names only its client, slot, Preview UID, label and pairwise sub, lasts
  `--preview-auth-access-token-ttl`, and works only at `/userinfo` for that same Preview.
- Codes and refresh tokens, the only tokens that carry the viewer's identity, go only to the ALB's backchannel, and need
  the slot's client secret to redeem. The slot client Secrets cannot reach a preview Pod: the slot admission policies
  refuse Secret volumes, `secretKeyRef` and `envFrom` Secret references there.

## Endpoints

The relay listens on `--listen-addr` (`:8080`), behind the edge load balancer at `https://<previewAuth.host>`. The
issuer is exactly that origin, with no path and no trailing slash, byte-equal in discovery, every ID token and the
preview Ingress annotations.

| Endpoint                                | Caller                | Purpose                                                                                                                                                                                  |
| --------------------------------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /.well-known/openid-configuration` | anyone                | Discovery: `code` only, `pairwise` subjects, RS256, `client_secret_basic` and `client_secret_post`, S256 PKCE                                                                            |
| `GET /jwks`                             | anyone                | The current and, during a rotation, the previous RSA public key, each with its RFC 7638 thumbprint as `kid`                                                                              |
| `GET /authorize`                        | the viewer's browser  | Checks the client, the redirect URI (exactly `https://<label>.<suffix>/oauth2/idpresponse`), the live Preview in that slot, the relay session and the access review, then sends the code |
| `GET /dex/callback`                     | the viewer's browser  | The one Dex redirect URI. Checks the login double-submit cookie, PKCE, the nonce and the ID token for the relay's own client, then resumes the authorization                             |
| `POST /token`                           | the ALB's backchannel | `authorization_code` and `refresh_token`; the client authenticates with exactly one of Basic and form credentials. Refresh tokens are not rotated                                        |
| `GET` or `POST /userinfo`               | the ALB's backchannel | A bearer token in the header (or, on POST, the form); never in a query string                                                                                                            |
| `POST /logout`                          | the viewer's browser  | Ends the relay session (same-origin only). It cannot end the ALB's own session on a preview host                                                                                         |
| `GET /`                                 | anyone                | A static page                                                                                                                                                                            |
| `GET /healthz`, `/readyz` on `:8081`    | the kubelet           | Ready once the Preview cache has synced; Dex being down does not make the relay unready                                                                                                  |

Error handling is part of the contract:

- **No authorize error is ever a redirect.** A bad request, an unknown or dead Preview, an unbound viewer, a Dex refusal
  or Dex being down is a page on the relay host (400, 403, 404 or 503), never a redirect to `/oauth2/idpresponse`. That
  also covers `placeholder.<suffix>`, which no Preview is ever named.
- **A transient failure is never `invalid_grant`.** When the access review or the Preview lookup fails, `/token` answers
  503 `temporarily_unavailable`, so the ALB keeps the viewer's session until the next attempt. `invalid_grant` means the
  grant is really over: a replayed or expired code, another slot's client, a revoked viewer, or a Preview that ended or
  was replaced at the same label.
- A code is spent in the ledger only after the Preview check and the access review pass, so a transient failure never
  burns a good code.

Every response carries `Strict-Transport-Security` (without `includeSubDomains`), `X-Content-Type-Options: nosniff`, a
`default-src 'none'; frame-ancestors 'none'` content security policy and `Referrer-Policy: no-referrer`. All other
responses are `Cache-Control: no-store` (discovery and the JWKS may be cached for 5 minutes). Request bodies are limited
to 8 KiB, repeated parameters are refused, and requests are rate-limited per source address.

## Cookies

| Cookie                        | Host        | Attributes                                          | Holds                                                                      |
| ----------------------------- | ----------- | --------------------------------------------------- | -------------------------------------------------------------------------- |
| `__Host-patchy-pa`            | the relay   | Secure, HttpOnly, `Path=/`, `SameSite=Lax`, session | The sealed relay session: the mapped identity and its expiry               |
| `__Host-patchy-pa-login-<h>`  | the relay   | as above, 5 minutes                                 | The sign-in double-submit value; one per sign-in, so two tabs do not clash |
| `patchy-preview-s<slot>-0`... | the preview | set by the ALB                                      | The ALB's encrypted session                                                |

The ALB's cookie names carry no `__Host-` prefix on purpose: their Path and Domain are the ALB's, and a browser silently
drops a `__Host-` cookie with a Domain, which would loop the sign-in.

## Who may view

The relay asks the API server a SubjectAccessReview for `get` on the virtual subresource `projects/previews`
(`patchy.bitwisemedia.uk`), named for the Preview's `spec.project`, in the release namespace, as the viewer: the Dex
username with `--preview-auth-username-prefix` and every group with `--preview-auth-groups-prefix`. Both prefixes are
required, so a viewer binding can never match a cluster identity of the same name. An answer is cached for
`--preview-auth-review-ttl` (20s) per viewer, Project and subresource.

The chart renders the `<fullname>-preview-viewer` ClusterRole (that one rule) and binds `previewAuth.viewers` to it in
the release namespace, which grants every Project. A per-Project grant is a Role with `resourceNames` beside it; the
review asks by name, so it works today. See [who may view](../intents/preview-sign-in.md#2-who-may-view) for how to
choose viewers.

Revocation takes effect:

- for an RBAC change: at the ALB's next refresh, within the access-token lifetime plus the review cache (about 10
  minutes by default), or at the ALB's `auth-session-timeout` if the ALB does not refresh;
- for a removal from a GitHub team: when the relay session ends (`--preview-auth-session-max-age`, 12 hours), since the
  relay asks Dex only at sign-in.

The chart's `previewAuth.sessionTimeout` sets `auth-session-timeout`, and the slot admission policies admit it as a
ceiling: any shorter session passes, a longer one is refused. Lowering it on a live install is one upgrade and raising
it two; see
[changing `sessionTimeout` on a live install](../intents/preview-sign-in.md#changing-sessiontimeout-on-a-live-install).

## Kubernetes access

Least privilege, all rendered by the chart:

- get, list and watch on `previews` in the release namespace (no Intents: the Preview's own `spec.project` names its
  Project);
- create on `subjectaccessreviews`;
- get and update on one Lease (`<fullname>-preview-auth-codes`), by name: the single-use code ledger, updated with a
  compare-and-swap on its resourceVersion, at most 5 retries, capped at 512 entries;
- nothing in the slot namespaces, no write to any patchy resource, and no Secret through the API: the keys Secret and
  the Dex client secret are mounted.

## Flags

As everywhere, each flag is also the matching `PATCHY_*` environment variable (`--preview-auth-issuer` is
`PATCHY_PREVIEW_AUTH_ISSUER`). Beside the [shared flags](index.md#shared-flags-every-controller):

| Flag                                    | Default                         | Purpose                                                                                                                                           |
| --------------------------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--preview-auth-issuer`                 | (required)                      | `https://<relay host>`, no path                                                                                                                   |
| `--preview-auth-host-suffix`            | (required)                      | The preview host suffix; codes go only to `https://<label>.<suffix>/oauth2/idpresponse`                                                           |
| `--preview-auth-slot-count`             | `2`                             | The preview slots, one ALB client (`patchy-preview-s<n>`) each                                                                                    |
| `--preview-auth-keys-dir`               | `/etc/patchy/preview-auth/keys` | The mounted keys Secret: `master`, `generation`, `signingKey`, and during a rotation `previousMaster`, `previousGeneration`, `previousSigningKey` |
| `--preview-auth-dex-issuer-url`         | (required)                      | Dex's issuer (https)                                                                                                                              |
| `--preview-auth-dex-client-id`          | (required)                      | The relay's static client at Dex                                                                                                                  |
| `--preview-auth-dex-client-secret-file` | (required)                      | The mounted file holding that client's secret                                                                                                     |
| `--preview-auth-dex-ca-file`            | —                               | PEM certificates trusted for Dex beside the system roots (a Dex behind a private CA)                                                              |
| `--preview-auth-username-claim`         | `preferred_username`            | The ID-token claim the access review runs for (`email` needs `--preview-auth-require-verified-email`)                                             |
| `--preview-auth-groups-claim`           | `groups`                        | The ID-token claim holding the viewer's groups                                                                                                    |
| `--preview-auth-username-prefix`        | (required)                      | Prepended to the username in access reviews                                                                                                       |
| `--preview-auth-groups-prefix`          | (required)                      | Prepended to every group in access reviews                                                                                                        |
| `--preview-auth-require-verified-email` | `false`                         | Refuse an ID token whose `email_verified` is not true                                                                                             |
| `--preview-auth-code-ttl`               | `1m`                            | Authorization code lifetime                                                                                                                       |
| `--preview-auth-access-token-ttl`       | `10m`                           | Access and ID token lifetime, and every token response's `expires_in`                                                                             |
| `--preview-auth-login-ttl`              | `5m`                            | How long a sign-in through Dex may take                                                                                                           |
| `--preview-auth-session-max-age`        | `12h`                           | The relay session's lifetime, and so every refresh token's                                                                                        |
| `--preview-auth-review-ttl`             | `20s`                           | How long one access review's answer is cached                                                                                                     |
| `--preview-auth-ledger-lease`           | (required)                      | The Lease the code ledger lives on                                                                                                                |
| `--preview-auth-rate-per-second`        | `5`                             | Per-source-address request rate; `0` disables the limit                                                                                           |
| `--preview-auth-rate-burst`             | `30`                            | Per-source-address burst                                                                                                                          |
| `--preview-auth-forwarded-hops`         | `0`                             | Proxies appending to `X-Forwarded-For` in front of the relay; the chart sets `1` behind the edge ALB                                              |
| `--preview-auth-probe-interval`         | `1m`                            | How often every Ready preview host is probed without credentials; `0` disables the probe                                                          |
| `--preview-auth-probe-timeout`          | (built in)                      | One host probe's timeout                                                                                                                          |

The chart sets all of these from `previewAuth` (`previewAuth.config.extra` adds any other key). `/token` and `/userinfo`
are the ALB's own calls, so every one of them arrives from an ALB node's address: they charge the per-address limit only
for a request that fails to authenticate (no valid client secret, or an access token that does not open). The ALB's
authenticated calls are never limited, so nobody can starve sign-ins by making the ALB redeem junk codes.

## Observability

One audit line per request at info: the endpoint, method, status, slot, Preview label, Project, result, the pairwise sub
and the first 8 hex characters of a hash of the token's id. Never a token, a code, a state, a cookie or a GitHub login;
a refused access review logs the viewer's username at warn, for operators.

| Metric                                        | Kind      | What                                                                                                                                 |
| --------------------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `patchy.preview_auth.authorize`               | counter   | Authorize requests by result                                                                                                         |
| `patchy.preview_auth.token`                   | counter   | Token requests by grant type and result                                                                                              |
| `patchy.preview_auth.userinfo`                | counter   | Userinfo requests by result                                                                                                          |
| `patchy.preview_auth.sar`                     | counter   | Decided access reviews by answer                                                                                                     |
| `patchy.preview_auth.ledger`                  | counter   | Code ledger results: ok, replay, conflict, full, error                                                                               |
| `patchy.preview_auth.upstream_login`          | counter   | Dex sign-ins by result                                                                                                               |
| `patchy.preview_auth.rate_limited`            | counter   | Requests refused by the rate limit                                                                                                   |
| `patchy.preview_auth.request.duration`        | histogram | Latency per endpoint                                                                                                                 |
| `patchy.preview_auth.unprotected_hosts`       | gauge     | Ready preview hosts that answered an unauthenticated request with anything but the redirect to the relay for their own slot's client |
| `patchy.preview_auth.probe.unreachable_hosts` | gauge     | Hosts that did not answer the probe at all                                                                                           |

`unprotected_hosts` above zero is an incident: the load balancer serves a preview without sign-in although its Ingress
may conform (a failed Auto Mode model build keeps a whole Ingress group on its old rules). While `preview.inboundCIDRs`
is set, the probe reaches the preview load balancer only if the cluster's NAT egress addresses are among them; otherwise
every host counts as unreachable and the probe proves nothing. `patchy check project` runs the same probe from your
workstation ([Preview sign-in](../intents/preview-sign-in.md#check-it)).
