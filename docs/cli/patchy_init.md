## patchy init

Scaffold what a repository needs to work with patchy, without a cluster

### Synopsis

Write the files a repository needs to work with patchy, from templates built into
this CLI. Nothing is sent to a cluster; the kubeconfig flags are inert here.

### Options

```
  -h, --help   help for init
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
* [patchy init app](patchy_init_app.md)	 - Scaffold an application repository for patchy's intents and previews

