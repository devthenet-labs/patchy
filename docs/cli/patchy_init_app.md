## patchy init app

Scaffold an application repository for patchy's intents and previews

### Synopsis

Write the files an application repository needs before patchy can work on it
from intents and preview its pull requests, into dir (default: the current
directory):

  .patchy/agent.yaml       the agent image the repository declares, an immutable
                           toolchain-v1 tag under --registry/--agent-prefix
  .patchy/Dockerfile       that image: patchy's agent base, pinned by digest,
                           plus the toolchain and the dependencies, offline
  .github/workflows/       uncredentialed builds of the runtime and agent images,
                           and the trusted publishers that push them to ECR
  .github/actions/publish/ the publishers' guard scripts, their tests and a README
                           listing the repository variables and trusted workflows
  Dockerfile, .dockerignore, .gitignore, README.md and a small service, for a new
  application only

The publishers push the runtime image of every open same-repository PR head and
default-branch commit (sha-<commit>) and the agent image from the default branch,
each gated on its own repository variable (PREVIEW_PUBLISH_ENABLED,
AGENT_PUBLISH_ENABLED). No generated file names an account ID, role or repository
ID: the publishers read them from repository variables. The next steps printed
afterwards give the block for patchy's reference terraform module
(deploy/terraform/aws/modules/app, pinned to this CLI's release), which creates
the registry repositories and publisher roles and outputs those variables for
gh variable set. Set them on the repository, never the organization: an
organization variable reaches every repository with these workflows. Only
.patchy/agent.yaml carries the registry, because patchy reads the image from it.

--existing is for an application that already has its source and runtime
Dockerfile: it writes only .patchy/ and the CI publishers, builds the runtime
image in a workflow of its own (runtime-image.yml) so the application's CI is
untouched, and prints what the application must be adapted to. It is not for a
repository init app scaffolded, such as a copy of a template repository: retarget
that with a full init app --force, after removing .patchy/agent.yaml and
.patchy/Dockerfile, which --force keeps and which name the template's image.

The repository defaults to the git checkout's origin remote and the default
branch to the one origin's HEAD names (else main); both are read from .git, so no
git binary is needed. The image name, which both registry repositories end in,
defaults to the repository name made image-safe (Hello.Web becomes hello-web).
The agent base is the one released with this CLI, pinned to the digest its tag
names in the registry now. --agent-base overrides it: a reference pinned by
digest is used as given, and a tag is pinned the same way. A development build
has no agent base of its own, so it needs --agent-base, as does a registry that
cannot be reached; pass a reference pinned by digest (...@sha256:<64 hex>) then.
Nothing else is fetched: no GitHub call is made.

An existing file is never overwritten without --force, and a symbolic link or
other non-regular file never is: every path is checked before any is written.
Nor does --force rewrite the agent toolchain: an existing .patchy/agent.yaml or
.patchy/Dockerfile is the repository's own, bumped with every toolchain change,
so it is kept as it is, and a kept .patchy/agent.yaml must declare a
toolchain-v<N> tag of the agent repository the options publish to. Remove both
to generate them again.
The written paths are printed on stdout, the next steps on stderr.

```
patchy init app [dir] [flags]
```

### Examples

```
  patchy init app --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com
  patchy init app hello-web --repo acme/Hello.Web --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com
  patchy init app --existing --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com --image-name shop
  # a copy of a template repository, retargeted:
  rm .patchy/agent.yaml .patchy/Dockerfile
  patchy init app --force --repo acme/Shop.Web --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com
```

### Options

```
      --agent-base string       agent base image to build FROM, pinned by digest or a tag to pin (default: the one released with this CLI)
      --agent-prefix string     registry path the agent image sits under, as the operator's --repository-image-registries allows (default "patchy/app-envs")
      --default-branch string   the repository's default branch (default: the one origin's HEAD names, else main)
      --existing                write only .patchy/ and the CI publishers, for an application that already has its source
      --force                   overwrite files that already exist, but keep an existing .patchy/agent.yaml and .patchy/Dockerfile
  -h, --help                    help for app
      --image-name string       image name both registry repositories end in (default: the repository name made image-safe)
      --lang string             application language: go (default "go")
      --registry string         ECR registry host, <account>.dkr.ecr.<region>.amazonaws.com (required)
      --repo string             GitHub repository as owner/name (default: the checkout's origin remote)
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

* [patchy init](patchy_init.md)	 - Scaffold what a repository needs to work with patchy, without a cluster

