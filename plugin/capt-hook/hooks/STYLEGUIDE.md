# cc-context Hooks Style Guide

The concrete style rules for `plugin/capt-hook/hooks/` — the Python source of the capt-hook
`ccx` guard pack. Target Python 3.13+. Go code follows the root `STYLEGUIDE.md`;
this file governs the Python.

## Core Principles

1. **Functional over imperative.** Compose, chain, and return. Skip intermediate
   variables when a pipeline reads well, and reach for the walrus (`:=`) and
   comprehensions instead of loops.
2. **Match for dispatch.** Pattern matching for type dispatch, destructuring, and
   multi-factor decisions. Use `if/elif` only for meaningful boolean flags.
3. **Type everything.** `from __future__ import annotations` in every module.
   Never widen a typed slot to `Any` to quiet the checker.
4. **Fail fast, fail loud.** No defensive coding: no fallbacks, shims, or
   backwards-compat layers, and no guards against impossible states. No sentinel
   values, no silent defaults. If unused, delete it. Crash on the unexpected.
5. **Make invalid states unrepresentable.** `NewType` for branded primitives,
   frozen dataclasses for immutable data, required fields over optionals.
6. **Minimal changes.** Stay within scope. Make the test pass, then stop. Improve
   only the code you touch.
7. **Match surrounding code.** Follow this guide first, then the file you're in,
   then the module. If surrounding code violates this guide, fix it.
8. **Flat over nested.** Early returns and flat control flow. Nesting deeper than
   three levels is a smell.
9. **Never a wrong allow, never a wrong rewrite.** A guard admits only what it can
   prove bounded, and a rewrite emits only what it can prove semantics-identical.
   When a shape is ambiguous, block or fall through — the escape hatches exist for
   exactly that case.

## Functional Style

Avoid intermediate variables. Chain operations or return directly.

```python
# Good
def expand_tool_names(name: str) -> set[str]:
    return (base := set(name.split("|"))) | {
        alias for n in base for alias in (TOOL_ALIASES.get(n), TOOL_ALIASES_REVERSE.get(n)) if alias
    }

# Bad
def expand_tool_names(name):
    base = set(name.split("|"))
    aliases = set()
    for n in base:
        ...
    return base | aliases
```

Use the walrus operator to bind a value once and reuse it inside an expression.

```python
# Good
if (match := WHEEL_CHECKSUM.search(body)):
    return match.group(1)

# Good — walrus in a comprehension, single pass
return [result for item in items if (result := process(item)) is not None]
```

Prefer the dict union operator over unpacking.

```python
config = defaults | user_config | overrides   # not {**defaults, **user_config, ...}
```

Use comprehensions instead of imperative accumulation.

```python
# Good
return [item.transform() for item in items if item.is_valid()]

# Bad
result = []
for item in items:
    if item.is_valid():
        result.append(item.transform())
return result
```

## Type Annotations

Always annotate. Use future annotations and guard expensive or cycle-prone imports
with `TYPE_CHECKING`. Under PEP 563 annotations stay strings, so they need no quotes.

```python
from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from captain_hook import Command

def bounded(self, cmd: Command) -> bool: ...
```

Lazy imports that break cycles or defer heavy modules go at the top of the function
body, before any logic, and never inside an `if`, `for`, or `try`.

```python
# Good
def model_version() -> str:
    from hooks.common import RESOURCES

    return RESOURCES.lookup()

# Bad — import buried in a branch
def model_version() -> str:
    if cached:
        from hooks.common import RESOURCES
        ...
```

Don't widen to `Any` to quiet the type checker. Use the real type, narrow with
`isinstance`, or split the model. Trivial complaints such as `cached_property`
shadowing `property` or descriptor-protocol nuances are noise; ignore them instead
of reaching for `# type: ignore`. Wanting `hasattr` on a typed object means the
type is wrong. Fix it or define a `Protocol`.

## Pattern Matching

Use `match` for type dispatch, destructuring, and decisions that turn on several
factors at once.

```python
match decision:
    case Keep():
        return msg
    case Compress(rate=rate):
        return msg.filter(lambda c: c.type != "text").append(compress(text, rate))
    case Summarize(content=content):
        return msg.append(content)
```

For multi-factor decisions, name the state with a `NamedTuple` so each `case` maps
one-to-one onto a requirement.

```python
match Status(is_fresh, scores.get(id(tc))):
    case Status(score=None):           return tc
    case Status(score=s) if s >= floor: return tc
    case Status(is_fresh=True):        return tc.demote()
    case Status(is_fresh=False):       return tc.exclude()
```

Use `if/elif` when the branches turn on meaningful boolean flags with their own
names. Don't build a tuple just to pattern-match on it.

## Functions & Methods

Options and flags go keyword-only, after `*`.

```python
def grep_tree_shaped(cmd: Command, *, cwd: Path | None) -> bool: ...
```

Use `@overload` when the return type depends on the argument shape.

```python
@overload
def __getitem__(self, index: int) -> Task: ...
@overload
def __getitem__(self, index: slice) -> tuple[Task, ...]: ...
def __getitem__(self, index: int | slice) -> Task | tuple[Task, ...]:
    return self.tasks[index]
```

Mutable defaults are forbidden in function signatures too: take `list[T] | None = None`
and normalize with `items = items or []` at the top of the body.

Access typed attributes directly instead of routing through helpers that may return
None; a helper that can fail forces every caller into a guard.

## API Design

Accept what callers naturally have. If callers must extract or transform data
before calling, take the parent object and extract internally — a condition takes
the `CommandLine`, not a pre-split argv.

Keep parameters minimal. No speculative flags; add a parameter when there is a
demonstrated need, not just in case.

Types reflect user concepts, not implementation internals. A public signature built
from internal metadata types leaks the implementation; expose the objects callers
think in.

## Error Handling

Keep `try` blocks minimal. Only the line that can throw belongs inside.

```python
# Good
try:
    response = subprocess.run(probe, capture_output=True, text=True)
except OSError:
    return False
return response.returncode == 0
```

No broad `except Exception` that swallows everything. Use dedicated exception
classes. No sentinel return values; raise, or return a typed result. In guard code
the fallthrough (`return False`, or `None` under the registered contract) is a
typed result meaning "no verdict — the command runs" or "no rewrite"; blocks are
reserved for positively identified offenses, and a failure that needs diagnosing
raises instead.

## Code Organization

Module order runs the module docstring, imports, constants, type aliases, helpers,
condition classes, `to`/`note` builders, then the `rewrite_command(...)` /
`hook(...)` / `gate(...)` registrations with their inline `tests={...}` last.
Module-level `UPPER_SNAKE_CASE` constants sit immediately after imports, before any
class or function.

Within a class body, all assignments come before any methods. That covers
constants, `ClassVar`s, and dataclass fields.

No leading underscores on classes, constants, or module-level helpers. Reserve a
leading underscore for a private instance attribute.

Each rewrite family pairs a `<family>_to` builder with a `<family>_note` explainer,
passed to `rewrite_command(to=..., note=...)`. The pairing is the package-wide
idiom — keep it when adding a family, and name new callables into it.

Frozen dataclasses for immutable and config data. Every mutable default needs a
factory such as `field(default_factory=list)`; a bare `[]` or `{}` is a bug.

## Comments & Docstrings

Code documents itself through names, types, and organization. No comments except
TODOs, non-obvious workarounds, and disabled code — plus lane rationale: a guard
helper whose branches encode a threat model carries a docstring stating each lane
and why admitting it is safe. That rationale is load-bearing prose the code cannot
express; everything else is clutter. A docstring that restates the signature is
deleted on sight.

```python
# Good — lane rationale the code can't show
def grep_tree_shaped(cmd: Command, *, cwd: Path | None) -> bool:
    """Report whether one grep positively targets a tree — the flood the guard exists for.

    Only a recursive flag with no operands, a `.`/`..` operand, or an operand one
    stat proves is a directory qualifies. Everything ambiguous — unparseable flags,
    variables, unstattable paths — returns False and the command runs: blocks
    under-match by design.
    """

# Bad — restates the signature
def grep_note(evt: BaseHookEvent) -> str:
    """Return the note for a grep event."""
```

## Testing

Tests live beside the hooks as `test_*.py`, with shared fixtures in `conftest.py`.
Run the pytest suite against published capt-hook — the CI mirror (the sibling
checkout pins unreleased deps whose parser drifts from the released one):

```bash
PYTHONPATH=plugin/capt-hook uv run --no-project --with pytest --with capt-hook pytest plugin/capt-hook/hooks/
```

Every registration also carries inline `tests={...}` — `Input(...)` mapped to
`Rewrite(...)`, `Block()`, or `Allow()` — runnable with `uvx capt-hook test`.
Inline rows cover disk-independent shapes only; anything that classifies operands
against the filesystem lives in pytest with a `tmp_path` tree and a pinned cwd.

Tests exercise the public surface: condition classes via `check_command_line`, the
`to`/`note` builders via their returned strings, and the `Input` harness — not
module internals. Write strict assertions against specific expected values; a test
that can't fail uncovers nothing. Mock the process boundary (the `ccx --help`
probe) and leave the guard under test real. Parameterize
repeated test bodies, giving each case a descriptive `id` and its own expected
values.
