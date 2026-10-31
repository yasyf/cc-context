import json
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import tomllib
import unittest


ROOT = Path(__file__).resolve().parents[2]
FRAGMENTS = ROOT / ".claude/fragments/pre-commit-config.yaml"


def load_yaml(path):
    return json.loads(subprocess.check_output(["ruby", "-rjson", "-ryaml", "-e", "puts JSON.generate(YAML.load_file(ARGV[0]))", str(path)], text=True))


class GoHookPinTest(unittest.TestCase):
    def setUp(self):
        self.fragment = load_yaml(FRAGMENTS / "precommit-go.fragment.yml")
        self.hooks = self.fragment[0]["hooks"]

    def test_fragment_source_and_current_pins(self):
        layout = tomllib.loads((FRAGMENTS / "layout.toml").read_text())
        self.assertIn("precommit-go", layout["fragments"])
        self.assertNotIn("cc-skills:precommit-go", layout["fragments"])
        tools = tomllib.loads((ROOT / "mise.toml").read_text())["tools"]
        go = next(line.split()[1] for line in (ROOT / "go.mod").read_text().splitlines() if line.startswith("go "))
        self.assertEqual(tools["go"], go)
        self.assertTrue(tools["golangci-lint"])
        self.assertIn("mise exec golangci-lint -- golangci-lint run", (ROOT / "Taskfile.yml").read_text())
        ci = load_yaml(ROOT / ".github/workflows/ci.yml")
        steps = ci["jobs"]["lint"]["steps"]
        self.assertTrue(any('golangci-lint = ' in step.get("run", "") and "mise.toml" in step["run"] for step in steps))
        self.assertTrue(any(step.get("with", {}).get("version") == "${{ steps.golangci.outputs.version }}" for step in steps))

    def test_whole_module_new_issue_fix_contract(self):
        self.assertEqual([hook["id"] for hook in self.hooks], ["golangci-lint-fmt", "golangci-lint"])
        self.assertEqual([shlex.split(hook["entry"]) for hook in self.hooks], [
            ["mise", "exec", "golangci-lint", "--", "golangci-lint", "fmt"],
            ["mise", "exec", "golangci-lint", "--", "golangci-lint", "run", "--new-from-rev", "HEAD", "--fix"],
        ])
        for hook in self.hooks:
            self.assertEqual(hook["language"], "system")
            self.assertEqual(hook["types"], ["go"])
            self.assertIs(hook["require_serial"], True)
            self.assertIs(hook["pass_filenames"], False)
            self.assertNotIn("language_version", hook)

    def test_mise_receives_repo_cwd_env_and_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            capture = root / "argv.json"
            mise = root / "mise"
            mise.write_text("#!/usr/bin/env python3\nimport json, os, sys\nfrom pathlib import Path\nPath(os.environ['HOOK_CAPTURE']).write_text(json.dumps({'argv': sys.argv[1:], 'cwd': os.getcwd(), 'GOOS': os.environ['GOOS'], 'CGO_ENABLED': os.environ['CGO_ENABLED']}))\nsys.exit(int(os.environ['HOOK_EXIT']))\n")
            mise.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"], HOOK_CAPTURE=str(capture), HOOK_EXIT="23", GOOS="darwin", CGO_ENABLED="1")
            for hook in self.hooks:
                result = subprocess.run(shlex.split(hook["entry"]), cwd=ROOT, env=env, check=False)
                self.assertEqual(result.returncode, 23)
                actual = json.loads(capture.read_text())
                self.assertEqual(actual["argv"], shlex.split(hook["entry"])[1:])
                self.assertEqual(actual["cwd"], str(ROOT))
                self.assertEqual((actual["GOOS"], actual["CGO_ENABLED"]), ("darwin", "1"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
