## patchy check

Check an artifact the way patchy will judge it, without a cluster

### Synopsis

Judge something you are about to hand to patchy the way the pipeline will judge
it, from your workstation and with no cluster access. The kubeconfig flags are
inert here.

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

