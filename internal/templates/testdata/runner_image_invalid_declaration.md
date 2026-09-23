<!-- patchy:runner-image -->
### Agent runner image

patchy could not use the declaration in `.patchy/agent.yaml` (`InvalidDeclaration`):
```text
`.patchy/agent.yaml` has unknown key `build`; `image` is the only key
```

The agent ran in the default runner image instead, which carries no language toolchain.

**How to fix it:** correct the image or `.patchy/agent.yaml`, then run `patchy check image <ref>`, which runs every check patchy runs and reports each verdict. The [troubleshooting table](https://devthenet-labs.github.io/patchy/integrations/agent-images/#troubleshooting) gives the fix for `InvalidDeclaration`. patchy reads the declaration once per finding, so the fix applies to the next finding on this repository.
