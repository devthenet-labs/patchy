"""Drive check-config.sh and copy-image.sh against fake aws and skopeo."""
import os
import pathlib
import subprocess
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
SHA = "a" * 40
DIGEST = "sha256:" + "b" * 64
OTHER = "sha256:" + "c" * 64

# The fakes keep one file per tag under $FAKE_ECR, holding its digest.
FAKE_AWS = r"""#!/usr/bin/env bash
set -euo pipefail
echo "aws $*" >> "$FAKE_ECR/calls"
case "$1 $2" in
  "ecr get-login-password") echo fake-password ;;
  "ecr describe-images")
    tag=""
    for arg in "$@"; do case "$arg" in imageTag=*) tag=${arg#imageTag=} ;; esac; done
    [[ -n "${FAKE_DESCRIBE_FAILS:-}" ]] && { echo "An error occurred (AccessDeniedException)" >&2; exit 254; }
    if [[ -f "$FAKE_ECR/$tag" ]]; then cat "$FAKE_ECR/$tag"; else
      echo "An error occurred (ImageNotFoundException) when calling the DescribeImages operation" >&2; exit 254; fi ;;
  *) exit 2 ;;
esac
"""
FAKE_SKOPEO = r"""#!/usr/bin/env bash
set -euo pipefail
echo "skopeo $*" >> "$FAKE_ECR/calls"
case "$1" in
  login) cat > /dev/null ;;
  copy) dest=${!#}; printf '%s\n' "$FAKE_PUSH_DIGEST" > "$FAKE_ECR/${dest##*:}" ;;
  *) exit 2 ;;
esac
"""

CONFIG = {
    "AWS_REGION": "us-east-1",
    "ECR_REGISTRY": "123456789012.dkr.ecr.us-east-1.amazonaws.com",
    "IMAGE_REPOSITORY": "team/previews/example-app",
    "ROLE_ARN": "arn:aws:iam::123456789012:role/example-app-runtime-push",
    "IMAGE_KIND": "runtime",
}
AGENT = {
    "IMAGE_KIND": "agent",
    "IMAGE_REPOSITORY": "team/agents/example-app",
    "ROLE_ARN": "arn:aws:iam::123456789012:role/example-app-agent-push",
    "AGENT_TAG": "toolchain-v2",
}


class PublisherScriptTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = pathlib.Path(self.tmp.name)
        self.ecr = root / "ecr"
        self.ecr.mkdir()
        (self.ecr / "calls").touch()
        bin_dir = root / "bin"
        bin_dir.mkdir()
        for name, body in [("aws", FAKE_AWS), ("skopeo", FAKE_SKOPEO)]:
            (bin_dir / name).write_text(body)
            (bin_dir / name).chmod(0o755)
        self.env = {"PATH": f"{bin_dir}:{os.environ['PATH']}", "RUNNER_TEMP": str(root),
                    "FAKE_ECR": str(self.ecr), "FAKE_PUSH_DIGEST": DIGEST,
                    "IMAGE_SHA": SHA, "IMAGE_DIGEST": DIGEST, "IMAGE_SOURCE": "main", **CONFIG}

    def run_script(self, script, **overrides):
        env = {**self.env, **overrides}
        return subprocess.run(["bash", str(HERE / script)], env=env, capture_output=True, text=True, check=False)

    def tags(self):
        return {p.name: p.read_text().strip() for p in self.ecr.iterdir() if p.name != "calls"}

    def copies(self):
        return [line for line in (self.ecr / "calls").read_text().splitlines() if line.startswith("skopeo copy")]

    def test_default_branch_runtime_publishes_sha_and_main_tags(self):
        result = self.run_script("copy-image.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.tags(), {f"sha-{SHA}": DIGEST, f"main-{SHA}": DIGEST})

    def test_main_tag_is_added_when_sha_tag_already_exists(self):
        (self.ecr / f"sha-{SHA}").write_text(DIGEST + "\n")
        result = self.run_script("copy-image.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.tags(), {f"sha-{SHA}": DIGEST, f"main-{SHA}": DIGEST})
        self.assertEqual(len(self.copies()), 1)
        self.assertTrue(self.copies()[0].endswith(f":main-{SHA}"))

    def test_rerun_with_everything_published_copies_nothing(self):
        for tag in [f"sha-{SHA}", f"main-{SHA}"]:
            (self.ecr / tag).write_text(DIGEST + "\n")
        result = self.run_script("copy-image.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.copies(), [])

    def test_different_image_at_an_existing_tag_is_refused(self):
        (self.ecr / f"sha-{SHA}").write_text(OTHER + "\n")
        result = self.run_script("copy-image.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.copies(), [])
        self.assertNotIn(f"main-{SHA}", self.tags())

    def test_pull_request_runtime_publishes_only_the_sha_tag(self):
        result = self.run_script("copy-image.sh", IMAGE_SOURCE="pull_request")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.tags(), {f"sha-{SHA}": DIGEST})

    def test_agent_publishes_the_declared_toolchain_tag_from_the_default_branch_only(self):
        result = self.run_script("copy-image.sh", **AGENT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.tags(), {"toolchain-v2": DIGEST})
        refused = self.run_script("copy-image.sh", IMAGE_SOURCE="pull_request", **AGENT)
        self.assertNotEqual(refused.returncode, 0)

    def test_agent_tag_must_be_a_toolchain_tag(self):
        for tag in ["", "latest", "toolchain-v0", "toolchain-v1 ", "sha-" + SHA, "toolchain-v1;id"]:
            with self.subTest(tag=tag):
                result = self.run_script("copy-image.sh", **{**AGENT, "AGENT_TAG": tag})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.tags(), {})

    def test_a_changed_toolchain_at_a_published_tag_says_to_bump_it(self):
        (self.ecr / "toolchain-v2").write_text(OTHER + "\n")
        result = self.run_script("copy-image.sh", **AGENT)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("bump the toolchain-v<N> tag in .patchy/agent.yaml", result.stderr)
        self.assertEqual(self.copies(), [])

    def test_an_unchanged_toolchain_is_already_published(self):
        (self.ecr / "toolchain-v2").write_text(DIGEST + "\n")
        result = self.run_script("copy-image.sh", **AGENT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Already published", result.stdout)
        self.assertEqual(self.copies(), [])

    def test_registry_errors_other_than_not_found_fail(self):
        result = self.run_script("copy-image.sh", FAKE_DESCRIBE_FAILS="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.copies(), [])

    def test_a_push_that_lands_a_different_digest_fails(self):
        result = self.run_script("copy-image.sh", FAKE_PUSH_DIGEST=OTHER)
        self.assertNotEqual(result.returncode, 0)

    def test_malformed_inputs_fail_before_any_registry_call(self):
        for overrides in [{"IMAGE_SHA": "$(id)"}, {"IMAGE_DIGEST": "sha256:short"}, {"IMAGE_SOURCE": "fork"},
                          {"IMAGE_KIND": "other"}, {"ECR_REGISTRY": "evil.example.com"}]:
            with self.subTest(**overrides):
                result = self.run_script("copy-image.sh", **overrides)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.tags(), {})

    def test_check_config_prints_the_account(self):
        result = self.run_script("check-config.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "123456789012\n")

    def test_check_config_refuses_unset_or_inconsistent_variables(self):
        for overrides in [{"AWS_REGION": ""}, {"ECR_REGISTRY": ""}, {"IMAGE_REPOSITORY": ""}, {"ROLE_ARN": ""},
                          {"IMAGE_KIND": ""},
                          {"ECR_REGISTRY": "123456789012.dkr.ecr.eu-west-1.amazonaws.com"},
                          {"ROLE_ARN": "arn:aws:iam::999999999999:role/example-app-runtime-push"},
                          {"ROLE_ARN": "arn:aws:iam::123456789012:user/someone"},
                          {"IMAGE_REPOSITORY": "single-segment"},
                          {"IMAGE_REPOSITORY": "Team/Previews/App"},
                          {"IMAGE_REPOSITORY": "team/previews/app:tag"}]:
            with self.subTest(**overrides):
                result = self.run_script("check-config.sh", **overrides)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
