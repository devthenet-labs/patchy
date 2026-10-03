# Hello.Web

A small Go HTTP service, scaffolded by `patchy init app` so that [patchy](https://github.com/devthenet-labs/patchy) can
build intents in it and preview its pull requests. It listens on port 8080, answers `/healthz` once it is
ready and writes nothing to disk.

## Develop

Use Go 1.26.6.

```sh
gofmt -l .
go vet ./...
go test -race ./...
go run .
```

The root `Dockerfile` builds the runtime image a preview runs: a static binary on distroless, uid 65532, no shell,
listening on 8080. A preview runs it with a read-only root filesystem and no writable `/tmp`:

```sh
docker build -t hello-web:local .
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 127.0.0.1:8080:8080 hello-web:local
curl -s localhost:8080/healthz
```

`.patchy/Dockerfile` is a different image: the one patchy's coding agent works in, which is patchy's agent base plus the
pinned Go toolchain and this module's dependencies, offline. `.patchy/agent.yaml` declares its immutable tag.

## CI and images

- `test` (`ci.yml`) tests the exact head and builds the runtime image without credentials.
- `agent image` (`agent-image.yml`) builds the agent image from `main`, also without credentials.
- `publish images` (`publish-images.yml`) hands each successful build to its trusted publisher, which pushes it to ECR.
  [`.github/actions/publish/README.md`](.github/actions/publish/README.md) describes the trust boundary.

### Repository variables

None of them is secret. Set the configuration first and the two `*_PUBLISH_ENABLED` gates last:

| Variable                   | Value                                          | What it is                                                          |
| -------------------------- | ---------------------------------------------- | ------------------------------------------------------------------- |
| `PUBLISH_REPOSITORY_ID`    | `gh api repos/acme/Hello.Web --jq .id`         | this repository's immutable numeric ID, which the guard checks      |
| `PUBLISH_OWNER_ID`         | `gh api repos/acme/Hello.Web --jq .owner.id`   | its owner's immutable numeric ID                                    |
| `AWS_REGION`               | `eu-west-2`                                    | the registry's AWS region                                           |
| `ECR_REGISTRY`             | `123456789012.dkr.ecr.eu-west-2.amazonaws.com` | the ECR registry host                                               |
| `AGENT_IMAGE_REPOSITORY`   | `patchy/app-envs/hello-web`                    | the agent image's ECR repository, which `.patchy/agent.yaml` names  |
| `AGENT_ROLE_ARN`           | the agent publisher role ARN                   | assumed only by `publish-agent.yml`                                 |
| `RUNTIME_IMAGE_REPOSITORY` | `patchy/previews/hello-web`                    | the runtime (preview) image's ECR repository                        |
| `RUNTIME_ROLE_ARN`         | the runtime publisher role ARN                 | assumed only by `publish-runtime.yml`                               |
| `AGENT_PUBLISH_ENABLED`    | `true`                                         | publishes the agent image; anything else skips it                   |
| `PREVIEW_PUBLISH_ENABLED`  | `true`                                         | publishes runtime images; set it last, once previews are configured |

### Trusted workflows

Only these two workflows assume an AWS role, and each role trusts only its own workflow at `main`:

| Trusted workflow (`job_workflow_ref`)                                  | Assumes            | Publishes                                                                            |
| ---------------------------------------------------------------------- | ------------------ | ------------------------------------------------------------------------------------ |
| `acme/Hello.Web/.github/workflows/publish-runtime.yml@refs/heads/main` | `RUNTIME_ROLE_ARN` | the runtime image as `sha-<commit>`, and `main-<commit>` for a default-branch commit |
| `acme/Hello.Web/.github/workflows/publish-agent.yml@refs/heads/main`   | `AGENT_ROLE_ARN`   | the agent image as the `toolchain-v<N>` tag `.patchy/agent.yaml` declares            |

### Changing the agent toolchain

Edit `.patchy/Dockerfile` (or the dependencies in `go.mod`) and bump the `toolchain-v<N>` tag in `.patchy/agent.yaml` in
the same commit: published tags are immutable, so a changed image needs a new tag.
