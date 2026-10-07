"""Run read-only ``gh`` calls through ``ccx vcs gh`` so they draw on the GitHub App's quota.

A JSON-flagged read that ``ccx format`` wraps is left to that rewrite, which routes it the same way.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from captain_hook import Allow, Annotated, BaseHookEvent, Input, Rewrite, rewrite_command_occurrences

from .common import app_read, ccx_bin, rewrote_note, spells_argv
from .json_guards import wraps

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence


def gh_app_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if (
        occ.nesting
        or wraps(occ)
        or not spells_argv(occ.command, occ.line.raw, occ.command.span)
        or not (ccx := ccx_bin())
    ):
        return None
    return app_read(occ.command, ccx)


rewrite_command_occurrences(
    skip_if=[Annotated("raw")],
    to=gh_app_to,
    note=rewrote_note(
        "ccx vcs gh -- <gh args>",
        "same output on the GitHub App's quota; vcs gh runs reads only, so run writes as plain `gh`",
    ),
    tests={
        Input(command="gh pr checks 12"): Rewrite(pattern="vcs gh -- pr checks 12"),
        Input(command="gh run watch 34 --exit-status"): Rewrite(pattern="vcs gh -- run watch 34 --exit-status"),
        Input(command="gh pr checks 12 --watch"): Rewrite(pattern="vcs gh -- pr checks 12 --watch"),
        Input(command="gh pr view 12 --json state | jq -r .state"): Rewrite(
            pattern="vcs gh -- pr view 12 --json state | jq -r .state"
        ),
        Input(command="gh api repos/o/r/pulls/12 --jq .state"): Rewrite(pattern="vcs gh -- api repos/o/r/pulls/12"),
        Input(command="gh pr view 12 --json state"): Allow(),
        Input(command="gh pr checks 12 && gh pr merge 12 --squash"): Rewrite(
            pattern="vcs gh -- pr checks 12 && gh pr merge 12 --squash"
        ),
        Input(command="while true; do gh pr checks 12; sleep 60; done"): Rewrite(pattern="do /"),
        Input(command="gh pr merge 12 --squash"): Allow(),
        Input(command="gh pr comment 12 --body hi"): Allow(),
        Input(command="gh api -X POST repos/o/r/issues/12/comments -f body=hi"): Allow(),
        Input(command="gh api repos/o/r/issues/12/comments -f body=hi"): Allow(),
        Input(command="gh api graphql -f query='{ viewer { login } }'"): Allow(),
        Input(command="gh api -X PUT repos/o/r/pulls/1/merge -f merge_method=merge"): Allow(),
        Input(command="gh api repos/o/r/pulls/1/merge -f merge_method=merge"): Allow(),
        Input(command="gh api --method PUT repos/o/r/pulls/1/merge"): Allow(),
        Input(command="gh api --method=PUT repos/o/r/pulls/1/merge"): Allow(),
        Input(command="gh api -XPUT repos/o/r/pulls/1/merge"): Allow(),
        Input(command="gh api repos/o/r/pulls/1/merge -X PUT"): Allow(),
        Input(command="gh api -X DELETE repos/o/r/git/refs/heads/b"): Allow(),
        Input(command="gh api repos/o/r/pulls/1/merge --input body.json"): Allow(),
        Input(command="gh pr checks 1 --watch && gh api -X PUT repos/o/r/pulls/1/merge"): Rewrite(
            pattern="vcs gh -- pr checks 1 --watch && gh api -X PUT repos/o/r/pulls/1/merge"
        ),
        Input(command="GH_TOKEN=x gh pr checks 12"): Allow(),
        Input(command="gh pr checks 12 # ccx:raw"): Allow(),
        Input(command="echo $(gh pr checks 12)"): Allow(),
        Input(command="ccx vcs gh -- pr checks 12"): Allow(),
    },
)
