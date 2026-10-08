# Preview sign-in

Previews run agent-written code against staging data, so who can open one is a security boundary. Until now the only
guard was the preview load balancer's IP allowlist (`preview.inboundCIDRs`). Preview sign-in adds a second one: every
preview Ingress gets the load balancer's `authenticate-oidc` action, pointed at patchy's own sign-in relay
([preview-auth](../configuration/preview-auth.md)), which signs viewers in with GitHub through your Dex, once per
browser session, and admits only the people and teams you list.

It is off by default, and turning it on keeps the IP allowlist. Dropping the allowlist is a separate, later decision
with its own gate ([Opening previews to the internet](#opening-previews-to-the-internet)).

What it guarantees:

- No request reaches a preview without a signed-in viewer the API server's RBAC admits, for that Preview's Project.
- Nothing the load balancer forwards to the preview identifies the viewer or works anywhere else: a pairwise subject per
  viewer and Preview, and an opaque access token that lasts minutes and works only at the relay's `/userinfo` for that
  same Preview. No GitHub login, email, group or Dex token reaches the app.
- The app's own `Authorization` header and login are untouched, and patchy's cookies have names no app uses
  ([For application authors](#for-application-authors)).

## Before you start

- Previews work with the allowlist: you have finished [Deploying intents and previews](deploying.md) through step 14,
  with `previewController.enabled`.
- Dex runs for your installation, with its GitHub connector, as the [status page sign-in](../status-ui.md) uses it.
- The relay's host, for example `preview-auth.patchy.acme.dev`, on the edge load balancer beside the webhook: a DNS
  record and a certificate covering it. With the [Terraform module](terraform-module.md), set
  `preview_auth = { host = "preview-auth.patchy.acme.dev" }`: it adds the host to the edge certificate and the edge
  alias records, and `helm_values` gains `previewAuth.host` and the relay Ingress's annotations. The host must not sit
  under the preview host suffix (the preview wildcard would send it to the preview load balancer).
- The edge load balancer must stay reachable from the whole internet. The preview load balancer calls the relay's
  `/token` and `/userinfo` from its own public addresses, which change; an allowlisted edge makes every sign-in fail.

## 1. Dex: the one redirect URI

The only identity-provider setup sign-in needs is one Dex static client for the relay, with exactly one redirect URI:

```yaml
staticClients:
  - id: patchy-preview-auth
    name: patchy previews
    secretEnv: PATCHY_PREVIEW_AUTH_CLIENT_SECRET
    redirectURIs:
      - https://preview-auth.patchy.acme.dev/dex/callback
```

The preview hosts never talk to Dex, so no wildcard and no per-preview URI is needed. The Terraform module prints this
URI as `preview_auth_dex_redirect_uri`, and the chart's install notes print it too. Put the same client secret in a
Secret in the release namespace:

```sh
kubectl -n patchy create secret generic patchy-preview-auth-dex --from-literal=clientSecret="$SECRET"
```

## 2. Who may view

Viewers are RBAC. The chart renders the ClusterRole `<release>-preview-viewer` (`get` on the virtual subresource
`projects/previews`) and binds the teams and users you list to it in the release namespace, which grants every Project.
The relay asks the API server, as the viewer, at sign-in and at every refresh.

```yaml
previewAuth:
  claims:
    usernameClaim: preferred_username # the GitHub login, through Dex's GitHub connector
    usernamePrefix: "github:"
    groupsPrefix: "github:"
  viewers:
    teams: ["acme:qa", "acme:product"] # <org>:<team-slug>, as Dex names GitHub teams by default
    users: ["octocat"]
```

Choose them with care:

- **Prefer teams.** A GitHub login can be renamed, and the old name registered by someone else; a team is managed in
  your organization.
- **Logins are matched exactly.** RBAC compares subject names case-sensitively, but GitHub logins are not
  case-sensitive. List a login exactly as Dex returns it in `preferred_username`, which is the login's own spelling.
- **Check the connector before you write team names.** What arrives as a group depends on the Dex GitHub connector's
  settings: `orgs` (which organizations and teams are read), `loadAllGroups`, and `teamNameField` (`slug`, `name` or
  `both`; team names are not slugs). Depending on them, an organization's name can arrive as a group of its own, so do
  not assume organization membership alone grants nothing: bind teams, and read the connector's configuration first.
- **No viewer, no access.** With `viewers` empty, every sign-in is refused (a 403 page) until someone is bound; the
  install notes warn about it.

A grant for one Project only is a Role with `resourceNames: [<project>]` on `projects/previews`, bound beside the
chart's binding; the review asks by Project name, so it works without a chart change.

## 3. Stage one: permit

Sign-in rolls out in two `helm upgrade`s, because Helm applies an Ingress before the admission policies that judge it,
and a refused Ingress write must never be how you find out.

```yaml
# patchy-previews.yaml, added
previewAuth:
  enabled: true
  stage: permit
  host: preview-auth.patchy.acme.dev # from helm_values when terraform manages it
  dex:
    issuerURL: https://dex.patchy.acme.dev
    clientID: patchy-preview-auth
    existingSecret: patchy-preview-auth-dex
  sessionTimeout: 3600 # the ALB's session; see "What a stolen session cookie can do"
  viewers:
    teams: ["acme:qa"]
```

```sh
helm history patchy -n patchy --max 1   # the rollback point
helm upgrade patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy --version X.Y.Z --namespace patchy \
  -f patchy-values.yaml -f patchy-previews.yaml -f <(terraform -chdir=infra output -raw helm_values)
```

This revision deploys the relay (two replicas spread over zones and nodes, behind a PodDisruptionBudget), generates its
keys (once; later upgrades reuse them), renders each slot's load balancer client Secret
`patchy-preview-oidc-g<generation>` with a Role that lets EKS Auto Mode (`Group eks:managed`) read exactly that Secret,
binds the viewers, and lets the slot Ingress policy admit the pinned sign-in annotations. Nothing requires sign-in yet,
and no Ingress changes.

Check it before going on:

```sh
kubectl get validatingadmissionpolicy patchy-preview-ingresses \
  -o jsonpath='{.metadata.annotations.patchy\.bitwisemedia\.uk/preview-auth-admits}{"\n"}'   # g1
kubectl -n patchy rollout status deploy/patchy-preview-auth
patchy check project <project> -n patchy   # preview-auth and preview-auth-dex PASS
```

`preview-auth-dex` fails when Dex does not know the client or the redirect URI; fix that now.

## 4. Stage two: require

Set `stage: require` in `patchy-previews.yaml` and run the same `helm upgrade`.

The render refuses unless the live slot Ingress policy already admits this key generation (the marker above). A render
without a cluster (`helm template`, GitOps) cannot look it up: set `previewAuth.permitConfirmedGeneration` to the
generation once the permit stage is live. Such a render also cannot look up chart-managed keys, so the chart refuses it
unless `previewAuth.keys.existingSecret` names an operator-owned keys Secret (and you supply the slot Secrets
`patchy-preview-oidc-g<generation>`): otherwise every sync would generate new keys and sign every viewer out.

This revision turns sign-in on in preview-controller (it patches every live preview's Ingress within one poll interval,
without restarting a Pod), puts the placeholder Ingress on slot 0's set, and adds the kept policy
`patchy-preview-ingress-auth`, which refuses any slot Ingress without its slot's set. preview-controller's sweep reports
an Ingress without it (`patchy.preview.ingress.unauthenticated`) and deletes it after three poll intervals.

## Check it

Gate G1, from an address the allowlist admits:

1. Every preview Ingress carries its slot's set, the cookie `patchy-preview-s<slot>` in `patchy-preview-<slot>`:

   ```sh
   kubectl get ingress -A -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,COOKIE:.metadata.annotations.alb\.ingress\.kubernetes\.io/auth-session-cookie'
   ```

2. The load balancer's rules match: in `aws elbv2 describe-rules` on the preview load balancer's 443 listener, every
   rule but the default authenticates (`authenticate-oidc`, issuer `https://<relay host>`, the slot's client id
   `patchy-preview-s<slot>`), and none forwards without it. A rejected Secret or Role makes Auto Mode keep a whole
   Ingress group on its old rules while the Kubernetes objects look right, so this step is not optional.
3. `patchy check project <project> -n patchy`: `preview-auth-host` PASSes for the placeholder and every Ready preview,
   each answering without credentials with a redirect to the relay for its own slot's client.
4. In a browser, a viewer you bound signs in once and reaches the app; a second preview opens without Dex.
5. Someone you did not bind gets the relay's 403 page.
6. The app sees only a pairwise `sub` in `x-amzn-oidc-identity` and an opaque access token in `x-amzn-oidc-accesstoken`;
   replayed at `https://<relay host>/userinfo` it returns that `sub` only, and after the access-token lifetime (10
   minutes) a 401.

The relay probes every Ready preview host the same way once a minute and exports
`patchy.preview_auth.unprotected_hosts`; alert on anything above zero. While the allowlist is set, that probe reaches
the preview load balancer only if the cluster's NAT egress addresses are in `preview.inboundCIDRs`; otherwise its hosts
all count as unreachable (`patchy.preview_auth.probe.unreachable_hosts`) and it proves nothing, so run G1 by hand.

## Rotating the keys

The keys are the relay's master secret (every token key, the pairwise key and each slot's client secret derive from it)
and its ID-token signing key, in the kept Secret `<release>-preview-auth-keys`. A rotation is the same pair of upgrades:

1. Set `previewAuth.keys.rotate` to the current generation plus one, and upgrade. The new generation becomes current and
   the old one previous; the slot policy admits both; Ingresses stay on the old one, and the relay accepts both. The
   install notes say "ROTATION IN PROGRESS".
2. The same upgrade again. The live policy now admits the new generation, so preview-controller and the placeholder move
   every Ingress to it (a new Secret name, so the load balancer re-reads its client).
3. Once every slot Ingress carries the new set (G1 step 1), an upgrade with `previewAuth.keys.dropPrevious: true` ends
   the overlap. Sessions signed with the old generation end at their next refresh.

Each generation's slot Secrets (`patchy-preview-oidc-g<n>`) are kept on upgrade and uninstall. Delete the old ones once
the overlap has ended: `kubectl -n patchy-preview-<n> delete secret patchy-preview-oidc-g<old>`.

If the keys Secret is lost, the next upgrade refuses: this release generated keys before. Restore it, or set
`previewAuth.keys.regenerate=true`, which signs everyone out and needs the permit stage again for the new generation.

## Rolling back

The require stage's policy is kept on rollback and uninstall, on purpose: a rollback must never leave previews without
sign-in. So undo it in this order, and only while `preview.inboundCIDRs` is still set:

1. Scale preview-controller to zero: `kubectl -n patchy scale deploy/patchy-preview-controller --replicas=0`. It would
   otherwise retry Ingress writes the old policy refuses.
2. Delete the kept policy's binding:
   `kubectl delete validatingadmissionpolicybinding patchy-preview-all-slots-ingress-auth`. From here, previews are
   guarded by the allowlist alone.
3. `helm rollback patchy <revision>` (or upgrade with `previewAuth.stage=permit`, or with `previewAuth.enabled=false`),
   then check preview-controller is scaled back up (`kubectl -n patchy get deploy/patchy-preview-controller`), and scale
   it to 1 if not.

Without step 2, the render refuses an upgrade that stops requiring sign-in (the binding is live), and a `helm rollback`
fails part-way, when the placeholder drops its annotations. Rolling back from the permit stage needs none of this: no
Ingress carries the annotations yet.

## Opening previews to the internet

Emptying `preview.inboundCIDRs` (with no `preview.prefixListsIDs`) makes sign-in the only guard. Do not do it on G1
alone. G1 shows that sign-in is enforced; it cannot show what a stolen session cookie can do, which depends on load
balancer behaviour nobody has measured yet. The gate is the ALB probe: on a throwaway load balancer, it records whether
the session cookie reaches the target and with which attributes (T1, T2), and whether a cookie issued for one host and
client is accepted on another (T3c). Run it first, and let its results set `sessionTimeout`.

The chart enforces the interim rule. It refuses an empty allowlist unless sign-in is already required,
`preview.allowPublicWithAuth: confirmed` is set, and `previewAuth.sessionTimeout` is at most 900 seconds. Nothing is
inferred from the cluster. With the Terraform module, `previews.inbound_cidrs = []` is accepted only with `preview_auth`
set.

### What a stolen session cookie can do

The attacker to plan for is not a viewer. It is whoever controls a preview's code: an agent's output, possibly
prompt-injected, which sees every request a viewer's browser sends to that preview.

- If the load balancer forwards its session cookie to the target, or sets it without HttpOnly (unknown until the probe's
  T1 and T2), the preview's code can read a viewer's cookie and send it anywhere through the viewer's browser. Preview
  pods have no egress beyond DNS, but the browser does.
- With the allowlist set, a stolen cookie works only from an admitted address. Without it, it works from anywhere: on
  the same preview host until `sessionTimeout`, and, if the load balancer accepts a cookie from one host on another
  host's rule (unknown until T3c), on another preview's host too, until the load balancer next refreshes there, or until
  `sessionTimeout` if it never refreshes.
- Per-slot cookie names do not prevent this: an attacker replaying a cookie chooses the names it sends. What limits it
  is the relay refusing, at the next refresh, a token whose client, slot, Preview or label is not the host's own, and
  whatever the load balancer itself binds to its session.
- So the viewers being the same for every Project does not make this harmless: the stolen session belongs to a viewer,
  and the code using it does not.

Until the probe has run, `sessionTimeout` is that window. Keep it short.

### What the probe found on EKS Auto Mode (2026-10-08)

A live probe on devthenet-dev (a throwaway preview that listed the cookie and header names it received) answered three
of these questions for the AWS load balancer as it behaves today:

- **T1, the cookie is not forwarded.** The load balancer removes its own `patchy-preview-s<slot>-*` cookie before the
  request reaches the target and forwards every other cookie. The app sees `x-amzn-oidc-*` headers, never the session.
- **T2, the cookie is HttpOnly, Secure and host-only**, with `SameSite=None` and a 7-day `Expires`. The session ends at
  `sessionTimeout` regardless: the expiry is inside the cookie, not the cookie's own lifetime. So neither the preview's
  server code nor its JavaScript can read a viewer's session.
- **T3c, a session replays across hosts in the same slot.** A cookie issued on one preview host, sent by hand to another
  host in the same slot (same client and cookie name), passed authentication there without the relay being asked. The
  relay's binding to a Preview applies only when the load balancer next calls it.
- Replay across slots (another client and cookie name) was not tested.

So stealing a session needs the viewer's machine or browser, not the preview's code. Once stolen, it works on every host
in its slot, and without the allowlist from anywhere, until `sessionTimeout`. That is why the chart caps it at 900
seconds for an empty allowlist.

### Changing `sessionTimeout` on a live install

Helm applies the placeholder Ingress, and the preview-controller patches the slot Ingresses, before the slot admission
policies change, so every Ingress write in an upgrade meets the previous revision's policies. The policies therefore
judge `auth-session-timeout` as a ceiling, not an exact value: any whole number of seconds from 1 up to `sessionTimeout`
(a shorter load-balancer session is never weaker). Every other pinned annotation is still compared exactly. The ceiling
is recorded on `patchy-preview-ingresses`:

```sh
kubectl get validatingadmissionpolicy patchy-preview-ingresses \
  -o jsonpath='{.metadata.annotations.patchy\.bitwisemedia\.uk/preview-auth-session-timeout}{"\n"}'   # 900
```

- **Lowering** takes one upgrade. The placeholder and the controller apply the new, shorter session at once, which the
  live policy's higher ceiling admits.
- **Raising** takes two upgrades with the same values, like a key rotation. The first records the new ceiling and keeps
  applying the old session, and its NOTES say `SESSION TIMEOUT CHANGE IN PROGRESS`. The second applies the new session.
  Neither upgrade is refused.
- **The first upgrade from chart 0.12.25 or earlier** meets policies that compare the timeout exactly. With the same
  `sessionTimeout`, nothing changes. Otherwise the chart reads the value the live policy pins and keeps applying it,
  with the ceiling the longer of the two, and the next upgrade applies the new value. Lowering is two upgrades this one
  time. If that upgrade also empties the allowlist (`preview.allowPublicWithAuth`) while the pinned value is over 900
  seconds, the render is refused: lower the session first, with the allowlist still set.
- **A render without a cluster** (`helm template`, Argo CD, Flux) cannot read the live policy, so it applies
  `sessionTimeout` directly. Against policies from this chart, lowering syncs cleanly, and a raise is refused on the
  placeholder until the policy has synced, so the sync must be retried once. The first sync off chart 0.12.25 or earlier
  meets policies that compare the timeout exactly, so any change, lowering included, is refused on the placeholder until
  the policies have synced: retry the sync once.

Up to 0.12.25 any change failed the first upgrade on the placeholder with
`preview Ingress sign-in annotations must be exactly one admitted key generation's pinned set for this slot`, and the
same upgrade passed on a second run (devthenet-dev: rev 69 failed, rev 70 deployed).

## For application authors

What a previewed app sees, and must not do:

- `/oauth2/idpresponse` is the load balancer's: the app never receives requests for it.
- Cookie names starting `patchy-preview-s` belong to the load balancer, and `__Host-patchy-pa*` to the relay; do not use
  them.
- `x-amzn-oidc-identity`, `x-amzn-oidc-accesstoken` and `x-amzn-oidc-data` carry only the pairwise subject and an opaque
  token: they are not a user identity, and the app must not treat them as one. `x-amzn-oidc-data` also shows the load
  balancer's ARN and account id.
- The app's own `Authorization` header and login are untouched.
- There is no sign-out of a preview: the load balancer's session ends after `sessionTimeout`. The relay's `/logout` only
  stops the silent sign-in to the next preview.
- When the session ends, a `fetch` or XHR gets a cross-origin redirect to the relay instead of its answer; reload the
  page to sign in again.
- One preview can send credentialed requests to another (they share a registrable domain); do not rely on SameSite
  cookies between previews.
