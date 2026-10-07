## patchy check project

Check that a Project is ready for its first intent

### Synopsis

Check a Project in the current namespace (-n) from your workstation, before its
first intent: what an intent and its preview need, one line per check, PASS,
FAIL or SKIP, then the repository it is about and the reason.

What the controllers already prove is read from the cluster, never proven again:
intent-controller's Ready verdict on the Project (it mints the App's scoped
tokens on every repository and ensures the labels in-cluster), the
IntentNameConflict condition, and the Ready verdict of each covering Forge. The
check never reads a Secret, so it never handles the App's private key: its
cluster reads are Projects, Forges, ConfigMaps, the preview placeholder
Ingress and Previews, with your kubeconfig.

The rest is checked with your own identity, and each reason says so: every
repository resolves to exactly one Forge; each app repository's agent image,
as .patchy/agent.yaml (or .devcontainer/devcontainer.json) declares it at the
default-branch head, passes source-controller's live policy, read from its
ConfigMap; and, for a previewed Project, previews are on, each previewed
repository's image sits under preview-controller's prefix with
sha-<default-branch head> published, <project>-0.<host suffix> resolves to
the preview load balancer, and it serves a certificate trusted for that name.

GitHub is read with GH_TOKEN, else GITHUB_TOKEN, else anonymously (public
repositories only). A repository on another host (GitHub Enterprise Server)
is read with GH_ENTERPRISE_TOKEN, else GITHUB_ENTERPRISE_TOKEN, only when
GH_HOST names that host, and anonymously otherwise: the Project, not you,
names the hosts, so a github.com token never leaves github.com and an
enterprise token never leaves GH_HOST. Registries are read with your cloud
and docker credentials: an ECR repository through the AWS SDK's default chain
(AWS_PROFILE), Artifact Registry through Application Default Credentials,
any other through your docker config. What this cannot prove is that the
cluster's own credentials work; a Repository's status.runnerImage and a
Preview's status are the evidence for those. The preview load balancer admits
only the chart's preview.inboundCIDRs, so from any other address the TLS check
times out and is a SKIP, not a FAIL.

With preview sign-in on (the chart's previewAuth), three more checks run:
the relay answers its discovery document at its issuer; Dex accepts the relay's
client with its one redirect URI, <relay>/dex/callback (asked of Dex's
authorization endpoint without signing anyone in, since Dex's client list is
not readable); and, once sign-in is required, the placeholder host and every
Ready preview host of the Project answer a request without credentials with
the load balancer's redirect to the relay for that slot's own client.

-o json or -o yaml prints the whole report as data. The exit status is 1 when
any check fails, 3 when the Project does not exist and 4 when you may not
read it.

```
patchy check project <name> [flags]
```

### Examples

```
  patchy check project shop -n patchy
  GH_TOKEN=$(gh auth token) AWS_PROFILE=prod patchy check project shop -n patchy
  GH_HOST=ghe.example.com GH_ENTERPRISE_TOKEN=$(gh auth token -h ghe.example.com) \
    patchy check project shop -n patchy
  patchy check project shop -o json | jq '.checks[] | select(.status == "FAIL")'
```

### Options

```
  -h, --help   help for project
```

### Options inherited from parent commands

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
  -o, --output string              output format: table, wide, json, yaml, name, or markdown (default "table")
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [patchy check](patchy_check.md)	 - Check something the way patchy will judge it, before the pipeline does

