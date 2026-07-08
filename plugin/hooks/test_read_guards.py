"""Tests for the large-file Read note in ``read_guards``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_read_guards.py

``_read_note`` counts the file's lines to report an honest window (``showing lines 1-100
of N total``), so it needs a real file on disk — a tmp file with a known line count. The
inline ``tests={}`` in read_guards.py cover the rewrite/block *decision*; the note text is
disk-dependent, so it lives here.
"""

from __future__ import annotations

from pathlib import Path

from hooks.common import READ_WINDOW_LINES
from hooks.read_guards import _read_note


def test_note_reports_window_and_total(tmp_path: Path) -> None:
    p = tmp_path / "big.txt"
    p.write_text("".join(f"line{i}\n" for i in range(350)))
    note = _read_note(p)
    assert f"lines 1-{READ_WINDOW_LINES} of 350 total" in note
    assert "ccx code outline" in note


def test_note_counts_final_unterminated_line(tmp_path: Path) -> None:
    # A file whose last line has no trailing newline still counts as a line.
    p = tmp_path / "no_trailing_newline.txt"
    p.write_text("a\nb\nc")
    note = _read_note(p)
    assert "of 3 total" in note
