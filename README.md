# cc-context

![cc-context banner](https://github.com/yasyf/cc-context/raw/main/docs/assets/readme-banner.webp)

[![PyPI](https://img.shields.io/pypi/v/cc-context.svg)](https://pypi.org/project/cc-context/)
[![Python](https://img.shields.io/pypi/pyversions/cc-context.svg)](https://pypi.org/project/cc-context/)
[![Docs](https://img.shields.io/github/actions/workflow/status/yasyf/cc-context/docs.yml?branch=main&label=docs)](https://yasyf.github.io/cc-context/)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-context/blob/main/LICENSE)

Tools & skills for keeping Claude's context minimal.

cc-context is a toolkit of small CLI commands and Claude Code skills that keep a
coding agent's working context lean — so the model spends its window on the task
instead of on a transcript it has outgrown. A tighter context means faster turns,
lower token cost, and fewer mistakes from a crowded window.

## Install

No install needed — run everything through [uvx](https://docs.astral.sh/uv/):

```bash
uvx cc-context --help
```

`uvx` fetches cc-context into a throwaway environment and runs it. To add it
to a project instead:

```bash
uv add cc-context
```

## Quickstart

The CLI is exposed as both `cc-context` and the shorter `ccx`. Run the starter
command to confirm it's wired up:

```console
$ ccx hello
Hello from cc-context!
```

`ccx --help` lists the available commands. This is an early scaffold — the
context-trimming commands land here as they're built.

## What problems does this solve?

- **Stale transcript crowds the window.** Long sessions fill the context with
  finished work the model no longer needs, leaving less room for the task at hand.
- **Token cost scales with context.** Every turn re-sends the whole window;
  carrying dead weight makes each turn slower and more expensive.
- **A crowded window invites mistakes.** The more buried the relevant detail, the
  more often the model loses the thread or contradicts an earlier decision.
- **State belongs on disk, not in the prompt.** Notes, task lists, and prior
  results are better recalled on demand than kept resident in every turn.

## Docs

[Read the docs](https://yasyf.github.io/cc-context/) for the full guide and API reference.
