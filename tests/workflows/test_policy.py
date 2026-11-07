"""Synthetic fixtures exercise the credentialed-action source-to-sink policy."""
import copy
import json
import re
import importlib.util
from pathlib import Path
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("policy", ROOT / "scripts/check_workflows.py")
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


class WorkflowPolicyTests(unittest.TestCase):
    def setUp(self):
        self.good = yaml.load("""
permissions: {contents: read}
jobs:
  checks:
    uses: ./.github/workflows/pr.yaml
  publish:
    needs: checks
    permissions: {contents: read, packages: write}
    steps:
      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        with: {persist-credentials: false}
      - uses: docker/login-action@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
        with: {password: '${{ secrets.DOCKER_PASSWORD }}'}
""", Loader=yaml.BaseLoader)

    def test_supported_publication(self):
        self.assertEqual(policy.validate(self.good), [])

    def test_mutable_credentialed_actions(self):
        for ref in ("docker/login-action@v4.6.0", "docker/login-action@main", "docker/login-action@abcdef", "docker://image:latest", "${{ inputs.action }}"):
            with self.subTest(ref=ref):
                doc = copy.deepcopy(self.good)
                doc["jobs"]["publish"]["steps"][1]["uses"] = ref
                self.assertTrue(policy.validate(doc))

    def test_reusable_external_workflow(self):
        self.good["jobs"]["checks"]["uses"] = "owner/repo/.github/workflows/checks.yaml@main"
        self.assertTrue(policy.validate(self.good))

    def test_checkout_default_or_true(self):
        for value in ({}, {"persist-credentials": "true"}):
            self.good["jobs"]["publish"]["steps"][0]["with"] = value
            self.assertTrue(policy.validate(self.good))

    def test_workflow_write_token(self):
        self.good["permissions"]["packages"] = "write"
        self.assertTrue(policy.validate(self.good))

    def test_secret_scope(self):
        for target in (self.good, self.good["jobs"]["publish"]):
            target["env"] = {"PASSWORD": "${{ secrets.DOCKER_PASSWORD }}"}
            self.assertTrue(policy.validate(self.good))
            del target["env"]

    def test_unexpected_job_write_token(self):
        self.good["jobs"]["checks"]["permissions"] = {"packages": "write"}
        self.assertTrue(policy.validate(self.good))

    def test_unexpected_write_scope(self):
        self.good["jobs"]["publish"]["permissions"]["actions"] = "write"
        self.assertTrue(policy.validate(self.good))

    def test_renovate_requires_reviewed_sha_updates(self):
        config = json.loads(next(ROOT.glob("renovate.json*")).read_text())
        rules = [rule for rule in config["packageRules"] if "github-actions" in rule.get("matchManagers", [])]
        self.assertTrue(rules)
        for dep_type, pin_digests in (("action", True), ("workflow", True), ("uses-with", False)):
            with self.subTest(dep_type=dep_type):
                settings = {}
                for rule in rules:
                    if dep_type in rule.get("matchDepTypes", [dep_type]):
                        settings.update(rule)
                self.assertIs(settings.get("pinDigests"), pin_digests)
                self.assertIs(settings.get("automerge"), False)

    def test_version_comments_remain_reviewable(self):
        for path in (ROOT / ".github/workflows").glob("*.y*ml"):
            for line in path.read_text().splitlines():
                if "uses:" in line and "@" in line:
                    if "zekihan/release-workflows/" in line:
                        self.assertRegex(line, r"@[0-9a-f]{40} # main$")
                        continue
                    self.assertRegex(line, r"@[0-9a-f]{40} # v[0-9]+[.][0-9]+[.][0-9]+$")

    def test_repository(self):
        for path in sorted((ROOT / ".github/workflows").glob("*.y*ml")):
            with self.subTest(workflow=path.name):
                self.assertEqual(policy.validate(yaml.load(path.read_text(), Loader=yaml.BaseLoader)), [])


if __name__ == "__main__":
    unittest.main()
