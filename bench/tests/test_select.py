"""Unit tests for the run/pilot task selector's fnmatch-glob support (no API calls).

Run: cd bench && python -m unittest tests.test_select
"""

from __future__ import annotations

import unittest
from types import SimpleNamespace

from ccxbench import taskgen
from ccxbench.__main__ import parse_arms, select
from ccxbench.types import ARMS


def _args(**kw) -> SimpleNamespace:
    base = {"tasks": None, "categories": None, "sample": None, "limit": None}
    base.update(kw)
    return SimpleNamespace(**base)


class TestSelectGlob(unittest.TestCase):
    def test_exact_id_still_matches(self) -> None:
        sel = select(taskgen.all_tasks(), _args(tasks="flood-t1-click-convert"))
        self.assertEqual([t.id for t in sel], ["flood-t1-click-convert"])

    def test_bracket_glob_selects_six_headline_flood_tasks(self) -> None:
        ids = [t.id for t in select(taskgen.all_tasks(), _args(tasks="flood-t[1-6]-*"))]
        self.assertEqual(len(ids), 6)
        self.assertNotIn("flood-t7-tornado-close-exec", ids)

    def test_star_glob_includes_the_diagnostic(self) -> None:
        self.assertEqual(len(select(taskgen.all_tasks(), _args(tasks="flood-*"))), 7)

    def test_comma_list_mixes_exact_and_glob(self) -> None:
        ids = sorted(t.id for t in select(taskgen.all_tasks(), _args(tasks="flood-t1-*,nav-click-command")))
        self.assertEqual(ids, ["flood-t1-click-convert", "nav-click-command"])


class TestParseArms(unittest.TestCase):
    def test_valid_subset_returned_in_canonical_order(self) -> None:
        # Input order/whitespace does not matter; the result follows ARMS order and dedupes.
        self.assertEqual(parse_arms("ccx-cli, baseline ,ccx-cli"), tuple(a for a in ARMS if a in ("baseline", "ccx-cli")))

    def test_full_selection_matches_default(self) -> None:
        self.assertEqual(parse_arms(",".join(ARMS)), ARMS)

    def test_unknown_arm_errors(self) -> None:
        with self.assertRaises(SystemExit):
            parse_arms("baseline,ccx-turbo")

    def test_baseline_less_selection_errors(self) -> None:
        with self.assertRaises(SystemExit):
            parse_arms("ccx-cli,ccx-mcp")


if __name__ == "__main__":
    unittest.main()
