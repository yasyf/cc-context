from graphlib import TopologicalSorter
import json
from pathlib import Path
import subprocess
import unittest

EXPECTED_CANCEL = "${{ !inputs.commit && github.event_name != 'workflow_dispatch' }}"
EXPECTED_DYNAMIC_PROFILES = 5


def load_workflow(name):
    return json.loads(subprocess.check_output(["ruby", "-rjson", "-ryaml", "-e", "puts JSON.generate(YAML.load_file(ARGV[0]))", str(Path(".github/workflows") / name)], text=True))


class NativeReleaseGateTest(unittest.TestCase):
    def setUp(self):
        self.release = load_workflow("release.yml")
        self.ci = load_workflow("ci.yml")

    def test_exact_commit_and_native_failure_dependencies(self):
        jobs = self.release["jobs"]
        descriptor = jobs["descriptor"]
        self.assertEqual(descriptor["needs"], "release")
        self.assertEqual(descriptor["outputs"], {"commit": "${{ steps.commit.outputs.sha }}"})
        commit_steps = [step for step in descriptor["steps"] if step.get("id") == "commit"]
        self.assertEqual(len(commit_steps), 1)
        self.assertIn('echo "sha=$(git rev-parse HEAD)" >> "$GITHUB_OUTPUT"', commit_steps[0]["run"])
        for name in ["post-release-ci", "post-release-guides"]:
            job = jobs[name]
            self.assertEqual(job["needs"], "descriptor")
            expected = {"commit": "${{ needs.descriptor.outputs.commit }}"}
            if name == "post-release-guides":
                expected["parallel_commits"] = True
            self.assertEqual(job["with"], expected)
            for key in ["if", "continue-on-error", "concurrency", "steps"]:
                self.assertNotIn(key, job)
        self.assertNotIn("concurrency", self.release)
        graph = {name: ([job["needs"]] if isinstance(job.get("needs"), str) else job.get("needs", [])) for name, job in jobs.items()}
        for parents in graph.values():
            self.assertTrue(set(parents) <= jobs.keys())
        self.assertEqual(set(TopologicalSorter(graph).static_order()), jobs.keys())

    def test_permissions_and_single_validation_calls(self):
        jobs = self.release["jobs"]
        ci = jobs["post-release-ci"]
        guides = jobs["post-release-guides"]
        self.assertEqual(ci["uses"], "./.github/workflows/ci.yml")
        self.assertEqual(ci["permissions"], {"contents": "read"})
        self.assertNotIn("secrets", ci)
        self.assertRegex(guides["uses"], r"^yasyf/cc-guides/\.github/workflows/guides\.yml@[0-9a-f]{40}$")
        self.assertEqual(guides["permissions"], {"contents": "write"})
        self.assertEqual(guides["secrets"], "inherit")
        self.assertEqual(jobs["descriptor"]["permissions"], {"contents": "write"})
        self.assertEqual(sum(job.get("uses") == ci["uses"] for job in jobs.values()), 1)
        self.assertEqual(sum(job.get("uses") == guides["uses"] for job in jobs.values()), 1)

    def test_read_only_pr_native_call_fixture_matches_release(self):
        fixture = load_workflow("native-release-contract.yml")
        jobs = fixture["jobs"]
        self.assertEqual(set(jobs), {"source", "native-ci", "native-guides"})
        self.assertEqual(fixture["permissions"], {"contents": "read"})
        self.assertNotIn("concurrency", fixture)
        self.assertEqual(jobs["source"]["outputs"], {"commit": "${{ steps.commit.outputs.sha }}"})
        self.assertEqual(jobs["source"]["steps"][0]["with"], {"ref": "${{ github.event.pull_request.head.sha }}"})
        for role in ["ci", "guides"]:
            expected = dict(self.release["jobs"]["post-release-" + role])
            expected["needs"] = "source"
            expected["with"] = dict(expected["with"], commit="${{ needs.source.outputs.commit }}")
            self.assertEqual(jobs["native-" + role], expected)
        source = Path(".github/workflows/native-release-contract.yml").read_text()
        self.assertIn("  pull_request:", source)
        self.assertNotIn("workflow_dispatch", source)
        self.assertNotIn("  push:", source)

    def test_polling_and_redispatch_removed(self):
        source = Path(".github/workflows/release.yml").read_text()
        for command in ["gh run watch", "gh run view", "workflow_run_id", "/dispatches", "wait_for_run"]:
            self.assertNotIn(command, source)
        self.assertFalse(Path(".github/scripts/test-release-observer.py").exists())

    def test_exact_commit_profile_and_cancellation(self):
        self.assertEqual(self.ci["permissions"], {"contents": "read"})
        self.assertEqual(self.ci["concurrency"]["group"], "ci-${{ inputs.commit || github.ref }}")
        self.assertEqual(self.ci["concurrency"]["cancel-in-progress"], EXPECTED_CANCEL)
        profiles = []
        for job in self.ci["jobs"].values():
            if "strategy" in job and "os" in job["strategy"]["matrix"]:
                os = job["strategy"]["matrix"]["os"]
                if isinstance(os, str):
                    profiles.append(os)
        self.assertEqual(len(profiles), EXPECTED_DYNAMIC_PROFILES)
        for profile in profiles:
            self.assertIn("github.event_name == 'pull_request' || inputs.commit != ''", profile)
            self.assertNotIn("github.event_name == 'workflow_dispatch'", profile)


if __name__ == "__main__":
    unittest.main(verbosity=2)
