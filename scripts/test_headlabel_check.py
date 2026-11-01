import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("headlabel_check", ROOT / "headlabel_check.py")
headlabel = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(headlabel)
REAL_RUN = subprocess.run


class HeadlabelTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.environment = dict(os.environ, HOME=str(self.root), GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_TERMINAL_PROMPT="0", GIT_ALLOW_PROTOCOL="file", GIT_AUTHOR_DATE="2000-01-01T00:00:00+0000", GIT_COMMITTER_DATE="2000-01-01T00:00:00+0000", LC_ALL="C")
        self.origin = self.root / "origin"
        self.origin.mkdir()
        self.git(self.origin, "init", "-q")
        self.git(self.origin, "config", "user.name", "Headlabel fixture")
        self.git(self.origin, "config", "user.email", "headlabel@example.invalid")
        self.git(self.origin, "config", "uploadpack.allowFilter", "true")
        self.base = self.commit("base\n", "base")
        self.old = self.commit("base\nsame\n", "series")
        self.git(self.origin, "branch", "old", self.old)
        self.git(self.origin, "checkout", "-q", "--detach", self.base)
        self.environment["GIT_COMMITTER_DATE"] = "2000-01-02T00:00:00+0000"
        self.new = self.commit("base\nsame\n", "series")
        self.git(self.origin, "branch", "new", self.new)
        self.git(self.origin, "checkout", "-q", "--detach", self.base)
        self.changed = self.commit("base\nchanged\n", "series")
        self.git(self.origin, "branch", "changed", self.changed)
        self.repo = self.audit_repo("audit")
        self.cursor = self.root / "cursor"
        self.cursor.write_text("100")
        self.calls = []
        self.local_fetches = []
        self.local_connectivity = []

    def git(self, repo, *arguments, **kwargs):
        return REAL_RUN(["git", "-C", str(repo), *arguments], env=self.environment, capture_output=True, text=True, check=True, **kwargs).stdout.strip()

    def commit(self, content, message):
        (self.origin / "file").write_text(content)
        self.git(self.origin, "add", "file")
        self.git(self.origin, "commit", "-q", "-m", message)
        return self.git(self.origin, "rev-parse", "HEAD")

    def audit_repo(self, name):
        repo = self.root / name
        repo.mkdir()
        self.git(repo, "init", "-q")
        self.git(repo, "remote", "add", "origin", str(self.origin))
        shutil.copytree(self.origin / ".git" / "objects", repo / ".git" / "objects", dirs_exist_ok=True)
        return repo

    def record(self, sequence=101, new=None, number=None, count=1):
        new = new or self.new
        unit = "commit" if count == 1 else "commits"
        return {"seq": sequence, "num": number or f"#{sequence}", "kind": "head", "lane": "fixture", "text": f"HEAD #19 {new[:7]}", "fields": {"attrs": {"head:owner/repo#19": f"{self.old}..{new} restack, range-diff patch-identical ({count} {unit})"}}}

    def invoke(self, records, *, status=0, stderr="", malformed=None, fetch_timeout=False):
        self.calls = []
        self.local_fetches = []
        self.local_connectivity = []

        def run(argv, **kwargs):
            self.calls.append(list(argv))
            if argv[0] == "cci":
                self.assertIn("--since-seq", argv)
                self.assertNotIn("--since", argv)
                self.assertNotIn("--cursor", argv)
                self.assertIn("--json", argv)
                self.assertEqual(argv[argv.index("--kind") + 1], "head")
                since = int(argv[argv.index("--since-seq") + 1])
                data = "\n".join(json.dumps(record) for record in records if record["seq"] > since)
                if malformed is not None:
                    data += "\n" + malformed
                result = subprocess.CompletedProcess(argv, status, data, stderr)
                if kwargs.get("check"):
                    result.check_returncode()
                return result
            if fetch_timeout and "fetch" in argv:
                raise subprocess.TimeoutExpired(argv, 45)
            environment = dict(self.environment)
            if "--no-lazy-fetch" in argv:
                trace = self.root / f"local-trace-{len(self.local_fetches)}.jsonl"
                environment["GIT_TRACE2_EVENT"] = str(trace)
                result = REAL_RUN(argv, env=environment, **kwargs)
                events = [json.loads(line) for line in trace.read_text().splitlines()]
                self.local_fetches.append(sum(event.get("event") == "child_start" and "fetch" in event.get("argv", []) for event in events))
                self.local_connectivity.append(sum(event.get("event") == "child_start" and "rev-list" in event.get("argv", []) and "--all" in event.get("argv", []) for event in events))
                return result
            return REAL_RUN(argv, env=environment, **kwargs)

        stdout = io.StringIO()
        errors = io.StringIO()
        args = headlabel.arguments(["--repo", str(self.repo), "--cursor", str(self.cursor), "--initial-seq", "100"])
        with patch("subprocess.run", side_effect=run), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(errors):
            headlabel.audit(args)
        return stdout.getvalue(), errors.getvalue()

    def fetch_count(self):
        return sum(argv[0] == "git" and "fetch" in argv for argv in self.calls)

    def test_no_argument_defaults(self):
        args = headlabel.arguments([])
        self.assertEqual(args.repo, "/Users/yasyf/.orca/workspaces/monorepo/monorepo/amberjack")
        self.assertEqual(args.drive, "durable-agents-rebuild")
        self.assertEqual(str(args.cursor), "/Users/yasyf/.claude/scratch/durable-agents-rebuild/drive-watchdog/headlabel.internal-seq.cursor")
        self.assertEqual(args.initial_seq, 147940)

    def test_legacy_cursor_writers_cannot_change_internal_sequence_state(self):
        legacy = self.root / "headlabel.cursor"
        internal = self.root / Path(headlabel.DEFAULT_CURSOR).name
        self.assertNotEqual(legacy, internal)
        legacy.write_text("208445")
        starts = []

        def tail(argv, **kwargs):
            self.assertEqual(argv[0], "cci")
            starts.append(int(argv[argv.index("--since-seq") + 1]))
            legacy.write_text(str(900000 + len(starts)))
            record = {"seq": 100 + len(starts), "kind": "head", "lane": "fixture", "text": "no PR claim", "fields": {"attrs": {}}}
            return subprocess.CompletedProcess(argv, 0, json.dumps(record), "")

        with patch.object(headlabel, "DEFAULT_CURSOR", str(internal)):
            args = headlabel.arguments(["--repo", str(self.repo), "--initial-seq", "100"])
            self.assertEqual(args.cursor, internal)
            with patch("subprocess.run", side_effect=tail):
                headlabel.audit(args)
                self.assertEqual(internal.read_text(), "101")
                self.assertEqual(legacy.read_text(), "900001")
                legacy.write_text("0")
                headlabel.audit(args)
                self.assertEqual(internal.read_text(), "102")
                self.assertEqual(legacy.read_text(), "900002")
                legacy.write_text("2000000")
                headlabel.audit(args)
                self.assertEqual(internal.read_text(), "103")
                self.assertEqual(legacy.read_text(), "900003")
        self.assertEqual(starts, [100, 101, 102])

    def test_complete_identical_and_changed_use_one_local_comparison(self):
        for new, mislabeled in [(self.new, False), (self.changed, True)]:
            with self.subTest(new=new):
                self.cursor.write_text("100")
                output, _ = self.invoke([self.record(new=new)])
                self.assertEqual(output.startswith("HEAD-MISLABEL #19 "), mislabeled)
                self.assertEqual(self.fetch_count(), 0)
                self.assertEqual(sum("range-diff" in argv for argv in self.calls), 1)
                self.assertEqual(self.local_fetches, [0])
                self.assertEqual(self.local_connectivity, [0])
                self.assertEqual(self.cursor.read_text(), "101")

    def test_legacy_text_and_imported_public_names(self):
        record = self.record(new=self.changed, number="#9001@other-node")
        record["fields"] = {}
        record["text"] = f"HEAD #19 {self.changed} (was {self.old}): restack, range-diff patch-identical (1 commit)"
        output, _ = self.invoke([record])
        self.assertIn(f"HEAD-MISLABEL #19 {self.changed[:7]} (record #9001@other-node) says patch-identical; range-diff: ", output)
        self.assertEqual(self.cursor.read_text(), "101")
        self.assertEqual(self.fetch_count(), 0)

    def test_legacy_kind_lane_text_form(self):
        record = self.record(new=self.changed)
        record["fields"] = {}
        record["lane"] = "#19"
        record["text"] = f"{self.changed} (was {self.old}): patch-identical (1 commit)"
        output, _ = self.invoke([record])
        self.assertTrue(output.startswith("HEAD-MISLABEL #19 "))

    def test_canonical_multiple_heads_and_authority_over_text(self):
        previews = [f"HEAD 2 PRs: #19 {self.new[:7]}, #20 {self.changed[:7]}", f"HEAD #999 {self.changed} (was {self.old}): patch-identical (1 commit)"]
        for preview in previews:
            with self.subTest(preview=preview):
                self.cursor.write_text("100")
                record = self.record()
                record["text"] = preview
                record["fields"]["attrs"]["head:owner/repo#20"] = f"{self.old}..{self.changed} replay, range-diff patch-identical (1 commit)"
                output, _ = self.invoke([record])
                self.assertEqual(len(output.splitlines()), 1)
                self.assertTrue(output.startswith("HEAD-MISLABEL #20 "))
                self.assertNotIn("#999", output)
                self.assertEqual(sum("range-diff" in argv for argv in self.calls), 2)
                self.assertEqual(self.cursor.read_text(), "101")

    def test_canonical_changed_and_initial_claims_are_irrelevant(self):
        record = self.record()
        record["fields"]["attrs"] = {"head:19": f"{self.new} ship, range-diff none: origin held no head", "head:20": f"{self.old}..{self.changed} ship, range-diff 1 changed"}
        record["text"] = f"HEAD #19 {self.changed} (was {self.old}): patch-identical (1 commit)"
        output, _ = self.invoke([record])
        self.assertEqual(output, "")
        self.assertEqual(self.fetch_count(), 0)
        self.assertEqual(sum("range-diff" in argv for argv in self.calls), 0)
        self.assertEqual(self.cursor.read_text(), "101")

    def test_malformed_or_ambiguous_canonical_entry_never_falls_back(self):
        cases = [
            {"head:bad": f"{self.old}..{self.new} patch-identical (1 commit)"},
            {"head:19": "not-a-range patch-identical (1 commit)"},
            {"head:19": f"{self.new} patch-identical (1 commit)"},
            {"head:19": f"{self.old}..{self.new} patch-identical (1 commit) patch-identical (2 commits)"},
            {"head:19": f"{self.old}..{self.new} patch-identical (unknown commits)"},
            {"head:19": f"{self.old}..{self.new} patch-identical (1 commit)", "head:20": "not-a-range"},
        ]
        for attrs in cases:
            with self.subTest(attrs=attrs):
                record = self.record()
                record["fields"]["attrs"] = attrs
                record["text"] = f"HEAD #19 {self.changed} (was {self.old}): patch-identical (1 commit)"
                with self.assertRaises(ValueError):
                    self.invoke([record])
                self.assertEqual(self.cursor.read_text(), "100")
                self.assertEqual(sum("range-diff" in argv for argv in self.calls), 0)

    def test_partial_batch_and_multi_head_failure_stop_at_audited_record(self):
        irrelevant = self.record(sequence=101)
        irrelevant["text"] = "not a patch-identical claim"
        irrelevant["fields"] = {}
        failing = self.record(sequence=102)
        failing["fields"]["attrs"]["head:owner/repo#20"] = f"{'f' * 40}..{'e' * 40} restack, range-diff patch-identical (1 commit)"
        with self.assertRaises(subprocess.CalledProcessError):
            self.invoke([irrelevant, failing, self.record(sequence=103)])
        self.assertEqual(self.cursor.read_text(), "101")
        self.assertEqual(self.fetch_count(), 1)

    def test_tail_error_and_empty_do_not_advance(self):
        with self.assertRaises(subprocess.CalledProcessError) as failure:
            self.invoke([self.record()], status=7, stderr="CCI query failed")
        self.assertEqual(failure.exception.stderr, "CCI query failed")
        self.assertEqual(self.cursor.read_text(), "100")
        self.assertEqual(len(self.calls), 1)
        output, _ = self.invoke([])
        self.assertEqual(output, "")
        self.assertEqual(self.cursor.read_text(), "100")
        self.cursor.unlink()
        self.invoke([])
        self.assertFalse(self.cursor.exists())

    def test_exclusive_internal_sequence_ignores_public_number_order(self):
        records = [self.record(sequence=100), self.record(sequence=101, number="#90000@one"), self.record(sequence=102, number="#2@two")]
        self.invoke(records)
        self.assertEqual(self.cursor.read_text(), "102")
        self.invoke([*records, self.record(sequence=103, number="#1@three")])
        self.assertEqual(self.cursor.read_text(), "103")
        self.assertEqual(self.calls[0][self.calls[0].index("--since-seq") + 1], "102")

    def test_malformed_json_preserves_successful_prefix(self):
        with self.assertRaises(json.JSONDecodeError):
            self.invoke([self.record()], malformed="{bad")
        self.assertEqual(self.cursor.read_text(), "101")

    def test_fetch_timeout_does_not_acknowledge_or_retry(self):
        record = self.record()
        record["fields"]["attrs"]["head:owner/repo#19"] = f"{'f' * 40}..{'e' * 40} restack, range-diff patch-identical (1 commit)"
        with self.assertRaises(subprocess.TimeoutExpired):
            self.invoke([record], fetch_timeout=True)
        self.assertEqual(self.cursor.read_text(), "100")
        self.assertEqual(self.fetch_count(), 1)
        self.assertEqual(sum("range-diff" in argv for argv in self.calls), 1)

    def test_missing_new_and_old_roots_fetch_then_compare(self):
        for missing in (self.new, self.old):
            with self.subTest(missing=missing):
                self.repo = self.audit_repo("missing-" + missing)
                (self.repo / ".git" / "objects" / missing[:2] / missing[2:]).unlink()
                self.cursor.write_text("100")
                output, _ = self.invoke([self.record()])
                self.assertEqual(output, "")
                self.assertEqual(self.cursor.read_text(), "101")
                self.assertEqual(self.fetch_count(), 1)
                self.assertEqual(sum("range-diff" in argv for argv in self.calls), 2)
                self.assertEqual(self.local_fetches, [0])

    def test_remote_failure_and_invalid_range_preserve_errors(self):
        self.git(self.repo, "remote", "set-url", "origin", str(self.root / "unavailable"))
        record = self.record()
        record["fields"]["attrs"]["head:owner/repo#19"] = f"{'f' * 40}..{'e' * 40} restack, range-diff patch-identical (1 commit)"
        with self.assertRaises(subprocess.CalledProcessError) as failure:
            self.invoke([record])
        self.assertTrue(failure.exception.stderr)
        self.assertEqual(self.cursor.read_text(), "100")
        self.assertEqual(self.fetch_count(), 1)
        self.git(self.repo, "remote", "set-url", "origin", str(self.origin))
        with self.assertRaises(subprocess.CalledProcessError) as failure:
            self.invoke([self.record(count=100)])
        self.assertIn("range-diff", failure.exception.cmd)
        self.assertTrue(failure.exception.stderr)
        self.assertEqual(self.cursor.read_text(), "100")
        self.assertEqual(self.fetch_count(), 1)

    def test_missing_ancestor_tree_blob_ordinary_and_promisor(self):
        missing_objects = {"ancestor": self.base, "tree": self.git(self.origin, "rev-parse", self.old + "^{tree}"), "blob": self.git(self.origin, "rev-parse", self.old + ":file")}
        for promisor in (False, True):
            for label, missing in missing_objects.items():
                with self.subTest(promisor=promisor, missing=label):
                    self.repo = self.audit_repo(f"missing-{promisor}-{label}")
                    if promisor:
                        self.git(self.repo, "config", "core.repositoryformatversion", "1")
                        self.git(self.repo, "config", "extensions.partialClone", "origin")
                        self.git(self.repo, "config", "remote.origin.promisor", "true")
                        self.git(self.repo, "config", "remote.origin.partialclonefilter", "blob:none")
                    (self.repo / ".git" / "objects" / missing[:2] / missing[2:]).unlink()
                    baseline = self.root / f"baseline-{promisor}-{label}"
                    shutil.copytree(self.repo, baseline)
                    fetched = REAL_RUN(["git", "-C", str(baseline), "fetch", "-q", "origin", self.new, self.old], env=self.environment, capture_output=True, text=True)
                    native = None
                    if fetched.returncode == 0:
                        native = REAL_RUN(["git", "-C", str(baseline), "range-diff", self.old + "~1.." + self.old, self.new + "~1.." + self.new], env=self.environment, capture_output=True, text=True)
                    self.cursor.write_text("100")
                    if fetched.returncode != 0 or native.returncode != 0:
                        with self.assertRaises(subprocess.CalledProcessError):
                            self.invoke([self.record()])
                        self.assertEqual(self.cursor.read_text(), "100")
                    else:
                        output, _ = self.invoke([self.record()])
                        self.assertEqual(output, "")
                        self.assertEqual(self.cursor.read_text(), "101")
                    self.assertEqual(self.fetch_count(), 1)
                    self.assertEqual(self.local_fetches, [0])

    def test_bad_line_rendering_is_bounded_with_real_git(self):
        self.git(self.origin, "checkout", "-q", "--detach", self.base)
        for index in range(5):
            old = self.commit(f"old-{index}\n", "old " + "x" * 220 + str(index))
        self.git(self.origin, "checkout", "-q", "--detach", self.base)
        for index in range(5):
            new = self.commit(f"new-{index}\n", "new " + "y" * 220 + str(index))
        self.repo = self.audit_repo("bounded")
        record = self.record(new=new, count=5)
        record["fields"]["attrs"]["head:owner/repo#19"] = f"{old}..{new} restack, range-diff patch-identical (5 commits)"
        output, _ = self.invoke([record])
        bad = output.strip().split("range-diff: ", 1)[1].split(" | ")
        self.assertEqual(len(bad), 4)
        self.assertTrue(all(len(line) <= 140 for line in bad))

    def test_original_27_line_negative_and_generated_metadata_work(self):
        before = json.loads((ROOT / "testdata" / "headlabel-before.json").read_text())
        self.assertEqual(hashlib.sha256(before["source"].encode()).hexdigest(), before["sha256"])
        self.assertEqual(before["sha256"], "e6d98a198a023919517ea36f40603e0b0982be1167f3f4702d13471c8cceacee")
        for size in (32, 10000):
            with self.subTest(generated_metadata=size):
                self.repo = self.audit_repo(f"work-{size}")
                blob = self.git(self.origin, "rev-parse", self.base + ":file")
                self.git(self.repo, "update-ref", "--stdin", input="".join(f"update refs/generated/{index:05d} {blob}\n" for index in range(size)))
                self.assertEqual(len(self.git(self.repo, "for-each-ref", "--format=%(refname)", "refs/generated").splitlines()), size)
                self.cursor.write_text("100")
                self.invoke([self.record()])
                fixed_fetches = self.fetch_count()
                self.assertEqual(fixed_fetches, 0)
                self.assertEqual(self.local_fetches, [0])
                self.assertEqual(self.local_connectivity, [0])
                calls = []
                legacy = f"#101 now HEAD #19 {self.new} (was {self.old}): restack, range-diff patch-identical (1 commit)\n"

                def old_run(argv, **kwargs):
                    calls.append(list(argv))
                    if argv[0] == "cci":
                        return subprocess.CompletedProcess(argv, 0, legacy, "")
                    routed = list(argv)
                    routed[routed.index("-C") + 1] = str(self.repo)
                    return REAL_RUN(routed, env=self.environment, **kwargs)

                self.cursor.write_text("100")
                with patch("subprocess.run", side_effect=old_run), patch("pathlib.Path", return_value=self.cursor), contextlib.redirect_stdout(io.StringIO()):
                    exec(compile(before["source"], "headlabel-before-27lines.py", "exec"), {})
                old_fetches = sum(argv[0] == "git" and "fetch" in argv for argv in calls)
                self.assertEqual(old_fetches, 1)
                print(f"HEADLABEL_WORK n={size} fixed_fetch={fixed_fetches} before_fetch={old_fetches} local_lazy_fetch=0 local_global_connectivity=0")


if __name__ == "__main__":
    unittest.main(verbosity=2)
