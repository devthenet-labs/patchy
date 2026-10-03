"""Pin the build/publish boundary as well as the data validators."""
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
BRANCH = "main"
RUNTIME_WORKFLOW = "runtime-image.yml"
RUNTIME_WORKFLOW_NAME = "runtime image"
KINDS = {"runtime": "AGENT", "agent": "RUNTIME"}  # kind -> the other kind's variable prefix
# The workflows the build/publish boundary is made of; any others in the
# repository are its own business.
PUBLISHING_WORKFLOWS = [RUNTIME_WORKFLOW, "agent-image.yml", "publish-images.yml", "publish-runtime.yml",
                        "publish-agent.yml"]


def workflow(name):
    return (ROOT / "workflows" / name).read_text()


def job_block(text, job):
    """The text of one top-level job in a workflow (jobs are indented two spaces)."""
    match = re.search(rf"^  {job}:\n(.*?)(?=^  [a-z][a-z0-9_-]*:\n|\Z)", text, re.M | re.S)
    if not match:
        raise AssertionError(f"job {job} not found")
    return match.group(1)


class WorkflowBoundaryTest(unittest.TestCase):
    def test_builds_have_no_oidc_credentials_or_publisher_configuration(self):
        for name in [RUNTIME_WORKFLOW, "agent-image.yml"]:
            text = workflow(name)
            for forbidden in ["id-token:", "secrets.", "vars.", "role-to-assume:", "docker login", "--push"]:
                self.assertNotIn(forbidden, text, f"{name}: {forbidden}")
            self.assertIn('test -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}"', text)
            self.assertIn("persist-credentials: false", text)

    def test_builds_are_reproducible(self):
        # A re-run rebuilds the same digest, which the publisher then finds
        # already published instead of refusing a different image at an
        # immutable tag.
        for name in [RUNTIME_WORKFLOW, "agent-image.yml"]:
            text = workflow(name)
            self.assertIn("SOURCE_DATE_EPOCH", text, name)
            self.assertIn("rewrite-timestamp=true", text, name)

    def test_default_branch_builds_are_never_cancelled(self):
        # Every default-branch commit publishes the runtime image a preview of
        # an unchanged repository runs, so those runs get one group per commit
        # (never a shared ref group, where a newer push replaces a pending
        # run) and are never cancelled.
        text = workflow(RUNTIME_WORKFLOW)
        self.assertIn("cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n", text)
        self.assertIn("format('pr-{0}', github.event.pull_request.number) || github.sha }}\n", text)
        self.assertNotIn("github.ref }}", text.split("concurrency:", 1)[1].split("jobs:", 1)[0])

    def test_publishers_are_separate_trusted_workflows(self):
        for kind, other in KINDS.items():
            text = workflow(f"publish-{kind}.yml")
            upper = kind.upper()
            self.assertIn("ref: ${{ github.workflow_sha }}", text)
            self.assertIn("sparse-checkout: .github/actions/publish\n", text)
            self.assertIn(f"github.ref == 'refs/heads/{BRANCH}'", text)
            self.assertIn(f"role-to-assume: ${{{{ vars.{upper}_ROLE_ARN }}}}", text)
            self.assertIn(f"IMAGE_REPOSITORY: ${{{{ vars.{upper}_IMAGE_REPOSITORY }}}}", text)
            self.assertIn(f"IMAGE_KIND: {kind}\n", text)
            # A publisher never even names the other kind's role or repository.
            self.assertNotIn(f"vars.{other}_", text)
            self.assertNotIn("inputs.kind", text)
            self.assertNotIn("secrets.", text)
            self.assertNotIn("secrets:", text)
            for forbidden in ["docker run", "docker build", "head.sha", "npm install", "go test"]:
                self.assertNotIn(forbidden, text)
            # The configuration check runs before any credential is requested.
            self.assertLess(text.index("check-config.sh"), text.index("configure-aws-credentials"))
            self.assertLess(text.index("validate_oci.py"), text.index("configure-aws-credentials"))
            self.assertLess(text.index("guard.cjs"), text.index("configure-aws-credentials"))

    def test_the_agent_tag_comes_from_the_declaration(self):
        text = workflow("publish-agent.yml")
        self.assertIn("AGENT_TAG: ${{ steps.guard.outputs.tag }}", text)
        self.assertNotIn("AGENT_TAG", workflow("publish-runtime.yml"))
        # A change to the declaration alone (a tag bump) rebuilds the image.
        self.assertIn("'.patchy/**'", workflow("agent-image.yml"))

    def test_runtime_and_agent_publishing_are_gated_separately(self):
        text = workflow("publish-images.yml")
        runtime, agent = job_block(text, "runtime"), job_block(text, "agent")
        self.assertEqual(text.count("vars.PREVIEW_PUBLISH_ENABLED == 'true'"), 1)
        self.assertEqual(text.count("vars.AGENT_PUBLISH_ENABLED == 'true'"), 1)
        self.assertIn("vars.PREVIEW_PUBLISH_ENABLED == 'true'", runtime)
        self.assertIn("uses: ./.github/workflows/publish-runtime.yml", runtime)
        self.assertIn("vars.AGENT_PUBLISH_ENABLED == 'true'", agent)
        self.assertIn("uses: ./.github/workflows/publish-agent.yml", agent)
        self.assertIn("github.event.workflow_run.event != 'pull_request'", agent)
        self.assertIn(f"workflows: [{RUNTIME_WORKFLOW_NAME}, agent image]", text)
        self.assertIn(f"name: {RUNTIME_WORKFLOW_NAME}\n", workflow(RUNTIME_WORKFLOW))
        self.assertIn("name: agent image\n", workflow("agent-image.yml"))
        self.assertNotIn("secrets: inherit", text)

    def test_only_the_publishers_assume_a_role(self):
        # The dispatcher calls the publishers and holds no role itself: each
        # role trusts exactly one publisher workflow (job_workflow_ref).
        dispatcher = workflow("publish-images.yml")
        for forbidden in ["role-to-assume", "configure-aws-credentials", "_ROLE_ARN", "steps:"]:
            self.assertNotIn(forbidden, dispatcher)
        for name in PUBLISHING_WORKFLOWS:
            text = workflow(name)
            expected = {"publish-runtime.yml": 1, "publish-agent.yml": 1}.get(name, 0)
            self.assertEqual(text.count("role-to-assume:"), expected, name)
            self.assertEqual(text.count("configure-aws-credentials@"), expected, name)

    def test_the_agent_publisher_never_reads_pull_requests(self):
        # Agent images come from the default branch only.
        self.assertNotIn("pull-requests:", workflow("publish-agent.yml"))
        self.assertNotIn("pull-requests:", job_block(workflow("publish-images.yml"), "agent"))

    def test_no_workflow_runs_untrusted_code_with_a_privileged_trigger(self):
        for name in PUBLISHING_WORKFLOWS:
            self.assertNotIn("pull_request_target", workflow(name), name)


if __name__ == "__main__":
    unittest.main()
