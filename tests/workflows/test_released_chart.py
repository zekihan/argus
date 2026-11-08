"""The released-chart check never selects the release it is building."""
import os
import subprocess
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def select(tags, ref=""):
    env = {**os.environ, "TAGS": "\n".join(tags), "SELECT_ONLY": "1", "GITHUB_REF": ref}
    result = subprocess.run([str(ROOT / "scripts/check_released_chart.sh"), "/bin/true"], env=env, capture_output=True, text=True, check=True)
    return result.stdout.strip()


class ReleasedChartSelectionTests(unittest.TestCase):
    def test_no_release_yet(self):
        self.assertEqual(select([]), "")
        self.assertEqual(select(["not-a-version", "v1.0.0-rc1"]), "")

    def test_first_release_tag_run_has_no_released_chart(self):
        self.assertEqual(select(["v0.1.0"], "refs/tags/v0.1.0"), "")

    def test_later_tag_run_uses_the_previous_release(self):
        self.assertEqual(select(["v0.1.0", "v0.2.0", "v0.10.0"], "refs/tags/v0.10.0"), "0.2.0")

    def test_edge_run_uses_the_latest_release(self):
        self.assertEqual(select(["v0.1.0", "v0.10.0", "v0.9.3"], "refs/heads/main"), "0.10.0")
        self.assertEqual(select(["v0.1.0", "v0.10.0", "v0.9.3"]), "0.10.0")


if __name__ == "__main__":
    unittest.main()
