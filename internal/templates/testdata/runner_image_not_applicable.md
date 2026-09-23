<!-- patchy:runner-image -->
### Agent runner image

patchy did not use `.devcontainer/devcontainer.json` to choose the image the agent runs in:
```text
`.devcontainer/devcontainer.json` builds its image (`build`); patchy does not build images. Publish the image and set `image`, or declare one in `.patchy/agent.yaml`, which takes precedence.
```

The agent ran in the default runner image instead, which carries no language toolchain. Nothing else about this finding
changes.

**To run the agent in your own image**, publish it prebuilt and name it in `image`, or declare it in
`.patchy/agent.yaml`, which takes precedence. `patchy check image <ref>` runs patchy's checks on an image
before you declare it, and the [agent image guide](https://devthenet-labs.github.io/patchy/integrations/agent-images/) covers what the image needs.
