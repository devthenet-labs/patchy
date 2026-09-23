<!-- patchy:runner-image -->
### Agent runner image

`.patchy/agent.yaml` declares `ghcr.io/acme/go-env:1.26`. patchy pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd` and verified its signature. No run has used it: each ran in the default runner image instead, which carries no language toolchain, for the reason below.

The remediation (attempt 1) was refused before the agent started (`SandboxUnenforced`): the cluster does not enforce the network isolation patchy requires before it runs a repository-declared image. This is not a problem with your image; until an operator enables NetworkPolicy enforcement and restarts the controllers, this finding runs in the default runner image.

To change the image, edit `.patchy/agent.yaml`; the [agent image guide](https://devthenet-labs.github.io/patchy/integrations/agent-images/) covers what the image needs.
