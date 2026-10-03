## patchy check

Check something the way patchy will judge it, before the pipeline does

### Synopsis

Judge something you are handing to patchy the way the pipeline will judge it,
from your workstation, and report every verdict as a PASS, FAIL or SKIP line.

check image needs no cluster: it checks an agent image a repository means to
declare, and the kubeconfig flags are inert for it. check project reads the
cluster with your own kubeconfig, and GitHub and the registry with your own
credentials, to tell whether a Project is ready for its first intent.

### Options

```
  -h, --help   help for check
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

* [patchy](patchy.md)	 - Work with patchy security findings from the terminal
* [patchy check image](patchy_check_image.md)	 - Check an agent image a repository means to declare
* [patchy check project](patchy_check_project.md)	 - Check that a Project is ready for its first intent

