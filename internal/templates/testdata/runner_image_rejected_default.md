<!-- patchy:runner-image -->
### Agent runner image

patchy could not use `docker.io/library/golang:1.26`, declared in `.patchy/agent.yaml` (`NotAllowlisted`):
```text
image `docker.io/library/golang:1.26` is not under an allowlisted registry path (ghcr.io/acme/)
```

The agent ran in the default runner image instead, which carries no language toolchain.

**How to fix it:** correct the image or `.patchy/agent.yaml`, then run `patchy check image docker.io/library/golang:1.26`, which runs every check patchy runs and reports each verdict. The [troubleshooting table](https://devthenet-labs.github.io/patchy/integrations/agent-images/#troubleshooting) gives the fix for `NotAllowlisted`. patchy reads the declaration once per finding, so the fix applies to the next finding on this repository.
