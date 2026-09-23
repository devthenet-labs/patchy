<!-- patchy:runner-image -->
### Agent runner image

`.devcontainer/devcontainer.json` declares `ghcr.io/acme/dev-env:2`. patchy pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd` and verified its signature. The agent will run in it, with patchy's own tools added under `/patchy/bin`.

To change the image, edit `.devcontainer/devcontainer.json`; the [agent image guide](https://devthenet-labs.github.io/patchy/integrations/agent-images/) covers what the image needs.
