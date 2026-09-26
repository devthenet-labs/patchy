# Status cookie prerequisite: security and liveness review

Reviewed separately after implementation, against main `be00e9a` (0.12.6), 2026-09-26. This is the first slice-2
prerequisite, not preview activation. No controller, Finding state-machine, credential, RBAC or infrastructure change is
included.

## Security checks

- Every production session chunk, OAuth state and SPA hint uses a `__Host-patchy-*` name, `Secure`, `Path=/`, and no
  Domain. The rule applies to expiry responses too. OAuth state previously used `/oauth2/`; retaining that path would
  make a prefixed cookie invalid, so both creation and deletion now use `/`.
- Session and state remain HttpOnly; only the three non-secret SPA hints are JavaScript-readable. Neither server nor
  HTTPS SPA falls back to legacy or local-development cookies.
- Server namespace selection depends solely on operator configuration, not request or forwarded headers. HTTP
  development has a distinct `patchy-dev-*` namespace. A regression sends forged cookies before a valid protected
  session and supplies `X-Forwarded-Proto: http`; the protected session still wins.
- Migration expires only the fixed set of old host-only cookies, using the old state's actual `/oauth2/` path.
  Parent-domain cookies cannot be identified from a Cookie header: they are ignored, not trusted or copied into the
  protected namespace. No parent-domain deletion or broad Clear-Site-Data operation is introduced.
- A valid sealed session supplied under the old name is rejected, not merely a malformed blob. Likewise, a valid OAuth
  transaction with its CSRF value supplied under the legacy/dev name is rejected before code exchange.
- Existing same-origin POST protection and authentication/authorization enforcement are unchanged. No new scope, token,
  GitHub call or Kubernetes access is introduced.

## Liveness checks

- OAuth callback expiry now matches the state cookie's path. A cookie-jar regression checks the state disappears; logout
  also removes the session. Full OIDC login, refresh and rejection tests continue to run.
- JavaScript includes Secure on HTTPS cookie deletion; otherwise error and logout hints could fail to clear. The UI
  regression double rejects invalid prefixed deletions and verifies each hint is consumed exactly once.
- Legacy cleanup emits at most 14 expiry cookies per invocation, even for duplicate names. It has no external call,
  retry or persistent migration state. Repeated requests cannot launch work or alter controller state.
- Seeded properties exercise varied session sizes up to the ten-chunk bound and both namespaces, including leftover
  expiry; opposite-mode cookies never reassemble into a session.
- Upgrading intentionally requires a fresh sign-in and restarts in-flight logins. Plain HTTP development requires
  `insecure: true`; it must not be used behind an HTTPS ingress. Both consequences are documented.

## Regression evidence and limits

Before the implementation, five new Go scenarios failed: missing host prefixes/path, acceptance of legacy sessions and
OAuth state, absent dev separation, and legacy provider suppression/migration. All three new UI tests also failed. They
passed after the implementation. Review added the cookie-jar, injection-order and bounded-cleanup checks above.

No unresolved security or liveness defect was found in this diff. This is a self-review, not an independent reviewer or
penetration test. The JavaScript tests model prefix enforcement; they are not a real-browser integration test. Live OIDC
is not configured on the current cluster, so a production sign-in cannot be claimed from the release gate. The full
gates and release/live-gate outcomes are recorded in the PR and deployment checkpoint, separately from this code review.
Infrastructure application still requires the owner's individual approval.
