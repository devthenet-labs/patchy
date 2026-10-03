# Enable intents (optional)

The security pipeline is complete once [the pipeline is verified](verify.md). Intent-driven development is an optional
second use of the same installation: a human opens an issue in an intent repository, patchy plans the change and posts
the plan, an approver approves it, and patchy builds exactly that plan in the application's own toolchain image and
opens the pull request. With previews on, each such pull request is also deployed to a URL of its own until it merges.

It adds, beside what you have already installed:

- the **intent-controller** (`intentController.enabled`), which polls GitHub and needs no webhook;
- **repository-declared agent images** (`agent.repositoryImages`), because a build runs in the application's own
  toolchain image, never the default one;
- a `Project` per application, in the `patchy-config` chart;
- the GitHub App's intent permissions (issues on the intent repository; contents and pull requests on each application
  repository), which `patchy setup github-app --intents` asks for;
- with previews, the preview foundation and the **preview-controller**, which need **EKS Auto Mode** and **ECR**, and
  the reference terraform module for the AWS side.

[Deploying intents and previews](../intents/deploying.md) is the whole path, from prerequisites to a merged intent with
a working preview, and [Onboarding an application](../intents/onboarding-app.md) is the part you repeat for each
application repository.
