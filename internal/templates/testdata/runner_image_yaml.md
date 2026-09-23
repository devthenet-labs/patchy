<!-- patchy:runner-image -->
### Agent runner image

`.patchy/agent.yaml` declares `ghcr.io/acme/go-env:1.26`. patchy pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd`, verified its
signature, and runs the agent in it with patchy's own tools added under `/patchy/bin`.

To change the image, edit `.patchy/agent.yaml`; the [agent image guide](https://devthenet-labs.github.io/patchy/integrations/agent-images/) covers what the image needs.
