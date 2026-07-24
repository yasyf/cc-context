"""Web guards: steer whole-page web fetches toward the token-bounded ``ccx web`` ops.

A ``WebFetch`` pulls a whole page through a lossy extraction into context, and a raw
``curl``/``wget`` page dump to stdout floods it the same way — the web analogue of the
file-read floods :mod:`read_guards`/:mod:`cat_rewrites` already steer. Both guards
point at the landed ``ccx web`` surface: ``ccx web outline <url>`` maps a page's headings,
``ccx web read <url> --section <ref>`` returns one budget-capped section, and
``ccx web search <url> "<question>"`` answers a prompt with the top-k relevant excerpts.
Their messages teach the same tiered handoff: a one-page question spawns the plugin's
``web-fetch`` agent (URL + prompt in, cited conclusions out) or calls ``ccx web``
directly, while research across many pages spawns ``web-researcher`` — both agents run
``ccx web`` in their own context and return only conclusions, never the raw pages.

``WebSearch`` is deliberately unguarded: its snippets are already token-bounded, and it is
the discovery step that decides which page deserves a ``ccx web`` fetch in the first place.

The ``WebFetch`` guard blocks the *first* attempt per URL per session (``once`` self-gate);
a deliberate re-run of the same URL passes, so an auth-walled or JS-heavy page ``ccx web``
cannot serve stays reachable. The ``curl``/``wget`` guard recognizes only an unpiped page GET
to stdout with a remote ``http(s)`` URL. A direct plain GET (``curl -sL <url>`` /
``wget -qO- <url>``) rewrites to ``ccx web read <url> --full`` when that surface is available.
API URLs, wrappers, environment prefixes, substitutions, unsupported flags, and unresolved
``ccx`` binaries run unchanged.
"""

from __future__ import annotations

import re
import shlex
from typing import TYPE_CHECKING
from urllib.parse import urlsplit

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    Command,
    CommandLine,
    CustomCommandLineCondition,
    CustomCondition,
    Event,
    Input,
    Rewrite,
    Tool,
    hook,
    rewrite_command_occurrences,
)

from .common import ccx_bin, ccx_supports

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

# curl short options that consume a value (the rest of the bundle, or the next token when
# the char ends the group). Scanning left-to-right, the first of these ends the group, so a
# header/referer/cookie value is skipped rather than misread as the target URL.
CURL_VALUE_SHORTS = frozenset("oXdTFHAbcCeEuUmKwYyzrx")

# curl short options that mean "not a plain page GET to stdout" — a sink (``-o``/``-O``/``-J``)
# or a non-GET method/body (``-X``/``-d``/``-T``/``-F``/``-I``). Any one present allows the line.
CURL_ALLOW_SHORTS = frozenset("oOJXdTFI")

# curl long options with the same "not a page dump" meaning. ``--data*``/``--form*`` families
# are matched by prefix in :func:`curl_dumps_page`.
CURL_ALLOW_LONGS = frozenset(
    {"--output", "--remote-name", "--remote-header-name", "--output-dir", "--request", "--json", "--upload-file", "--head"}
)

# curl long options that consume a value which can itself contain an ``http(s)`` token — skip
# the value so it is not mistaken for the target URL. (Sink/method value-longs never reach here:
# they allow the line first.)
CURL_VALUE_LONGS = frozenset({"--header", "--referer", "--user-agent", "--cookie", "--proxy"})

CURL_PLAIN_SHORTS = frozenset("sSL")
CURL_PLAIN_LONGS = frozenset({"--silent", "--show-error", "--location"})
WGET_PLAIN_LONGS = frozenset({"--quiet", "--output-document", "--output-document=-"})
WGET_PLAIN_SHORT = re.compile(r"-(?:q+|q*O-?)$")


def is_remote_url(token: str) -> bool:
    """Whether ``token`` is an unambiguous non-loopback ``http(s)`` URL."""
    try:
        parts = urlsplit(token)
        host = parts.hostname
    except ValueError:
        return False
    return (
        parts.scheme.lower() in {"http", "https"}
        and bool(host)
        and host != "localhost"
        and host != "::1"
        and not host.startswith("127.")
    )


class WholePageWebFetch(CustomCondition):
    """Matches the first ``WebFetch`` of a given remote URL this session.

    A ``WebFetch`` pulls the whole page through a lossy extraction. This fires on the first
    attempt per URL (``once`` self-gate, precedent: json_guards' ``SeenEmittingJson``), so a
    deliberate same-URL re-run passes — the escape hatch for a page ``ccx web`` can't serve.
    A local/loopback target never matches.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        url = (evt._tool_input.get("url") or "").strip()
        if not is_remote_url(url):
            return False
        return evt.ctx.s.once(url, scope="ccx-web-fetch")


hook(
    Event.PreToolUse,
    only_if=[Tool("WebFetch"), WholePageWebFetch()],
    message=(
        "BLOCKED: WebFetch pulls a whole page through a lossy extraction into context. "
        "Drop-in: spawn the `cc-context:web-fetch` agent with this URL and your prompt verbatim — "
        "it reads the page in its own context and returns only the cited answer. Reading inline "
        "instead? `ccx web outline <url>` maps its headings, `ccx web read <url> --section <ref>` "
        "returns one budget-capped section, and `ccx web search <url> \"<question>\"` answers your "
        "prompt with the top-k relevant excerpts. "
        "Researching across pages? Spawn `cc-context:web-researcher` with the question and seed "
        "URLs. Escape hatch — `ccx web` can't serve this URL: re-run the same WebFetch; "
        "a repeat of the URL passes."
    ),
    block=True,
    tests={
        Input(tool="WebFetch", tool_input={"url": "https://docs.example.com/en/guide/config"}): Block(
            pattern="ccx web outline"
        ),
        Input(tool="WebFetch", tool_input={"url": "http://localhost:3000/health"}): Allow(),
        Input(tool="WebFetch", tool_input={"url": "http://127.0.0.1:8080/metrics"}): Allow(),
    },
)


def sinks_stdout(cmd: Command) -> bool:
    """Whether ``cmd`` redirects its stdout to a file — a disk sink, not a context flood.

    A stderr-only redirect leaves stdout flooding; only a default/stdout redirect counts.
    """
    return any(
        r.op in (">", ">>", ">|", ">&") and (r.fd is None or r.fd == 1) for r in cmd.redirects
    )


def scan_curl_shorts(token: str) -> tuple[bool, bool]:
    """Scan a bundled short group (``-sSfLo``), returning ``(allows_line, consumes_next_token)``.

    ``allows_line`` is set as soon as a sink/method char (:data:`CURL_ALLOW_SHORTS`) appears.
    Otherwise the first value-consuming char (:data:`CURL_VALUE_SHORTS`) ends the group; when it
    is the last char its value is the following token, so the caller must skip that token.
    """
    j = 1
    while j < len(token):
        ch = token[j]
        if ch in CURL_ALLOW_SHORTS:
            return True, False
        if ch in CURL_VALUE_SHORTS:
            return False, j == len(token) - 1
        j += 1
    return False, False


def curl_dumps_page(args: tuple[str, ...]) -> bool:
    """Whether a ``curl`` argv is a plain page GET to stdout — a remote URL, no sink, no method flag."""
    remote = False
    i = 0
    n = len(args)
    while i < n:
        a = args[i]
        if a == "--":
            return remote or any(is_page_url(t) for t in args[i + 1 :])
        if a.startswith("--"):
            name = a.split("=", 1)[0]
            if name in CURL_ALLOW_LONGS or name.startswith(("--data", "--form")):
                return False
            if name == "--url":
                # curl's long form for the target URL (`--url=<u>` / `--url <u>`) — scan the
                # value like a positional, since the plain page GET can name it this way.
                val = a.split("=", 1)[1] if "=" in a else (args[i + 1] if i + 1 < n else "")
                if is_page_url(val):
                    remote = True
                i += 1 if "=" in a else 2
                continue
            if "=" not in a and name in CURL_VALUE_LONGS:
                i += 2
                continue
            i += 1
            continue
        if a.startswith("-") and len(a) > 1:
            allow, consumes_next = scan_curl_shorts(a)
            if allow:
                return False
            i += 2 if consumes_next else 1
            continue
        if is_page_url(a):
            remote = True
        i += 1
    return remote


def wget_to_stdout(args: tuple[str, ...]) -> bool:
    """Whether a ``wget`` argv sets its output document to stdout (``-O -``/``-qO-``/``--output-document=-``).

    ``wget`` writes to a file by default, so a plain ``wget <url>`` is a disk download, not a
    context flood; only an explicit stdout output-document dumps the page.
    """
    for i, a in enumerate(args):
        if a in ("-O", "--output-document"):
            return i + 1 < len(args) and args[i + 1] == "-"
        if a.startswith("--output-document="):
            return a.split("=", 1)[1] == "-"
        if a.startswith("-") and not a.startswith("--") and "O" in a:
            rest = a[a.index("O") + 1 :]
            return rest == "-" if rest else (i + 1 < len(args) and args[i + 1] == "-")
    return False


def wget_dumps_page(args: tuple[str, ...]) -> bool:
    """Whether a ``wget`` argv dumps a remote page to stdout."""
    return wget_to_stdout(args) and any(is_page_url(a) for a in args)


class PageDumpToStdout(CustomCommandLineCondition):
    """Matches an unpiped ``curl``/``wget`` page GET to stdout with a remote ``http(s)`` URL.

    Pipes, redirects, non-GET methods, API URLs, wrappers, environment prefixes,
    substitutions, loopback targets, and scheme-less URLs do not match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        for occ in cl.occurrences:
            cmd = occ.command
            if occ.piped or sinks_stdout(cmd) or not occurrence_is_direct(cl, occ):
                continue
            if occurrence_dumps(cmd):
                return True
        return False


def is_api_url(url: str) -> bool:
    """Whether ``url`` names an endpoint curl should handle unchanged."""
    parts = urlsplit(url)
    if (parts.hostname or "").lower().startswith("api."):
        return True
    path = parts.path.rstrip("/")
    if path.endswith(".json") or path.endswith("/graphql"):
        return True
    if any(kw in parts.query.lower() for kw in ("json", "graphql")):
        return True
    return "api" in [seg for seg in parts.path.split("/") if seg]


def is_page_url(url: str) -> bool:
    """Whether ``url`` is a remote page rather than an API-shaped endpoint."""
    return is_remote_url(url) and not is_api_url(url)


def curl_plain_flag(arg: str) -> bool:
    return arg == "--" or arg in CURL_PLAIN_LONGS or (
        arg.startswith("-")
        and not arg.startswith("--")
        and len(arg) > 1
        and all(char in CURL_PLAIN_SHORTS for char in arg[1:])
    )


def wget_plain_flag(arg: str) -> bool:
    return (
        arg in WGET_PLAIN_LONGS
        or arg in {"-", "--"}
        or WGET_PLAIN_SHORT.fullmatch(arg) is not None
    )


def occurrence_rewrite_url(cmd: Command) -> str | None:
    """The URL of a direct plain ``curl``/``wget`` page dump."""
    urls = tuple(arg for arg in cmd.args if is_page_url(arg))
    if len(urls) != 1:
        return None
    url = urls[0]
    others = tuple(arg for arg in cmd.args if arg != url)
    if cmd.executable == "curl" and all(curl_plain_flag(arg) for arg in others):
        return url
    if (
        cmd.executable == "wget"
        and wget_to_stdout(cmd.args)
        and all(wget_plain_flag(arg) for arg in others)
    ):
        return url
    return None


def occurrence_is_direct(cl: CommandLine, occ: Occurrence) -> bool:
    """Whether wrappers, prefixes, and substitutions leave the occurrence unambiguous."""
    return (
        occ.nesting == 0
        and not occ.command.env
        and not any(child.host == occ for child in cl.occurrences)
    )


def occurrence_dumps(cmd: Command) -> bool:
    """Whether a direct command is a remote page dump to stdout."""
    if cmd.executable == "curl":
        return curl_dumps_page(cmd.args)
    if cmd.executable == "wget":
        return wget_dumps_page(cmd.args)
    return False


def occurrence_can_rewrite(evt: BaseHookEvent, occ: Occurrence) -> bool:
    """Whether the occurrence is direct, unpiped, and without a stdout sink."""
    return (
        not occ.piped
        and not sinks_stdout(occ.command)
        and occurrence_is_direct(evt.cmd.line, occ)
    )


def page_dump_to(evt: BaseHookEvent, occ: "Occurrence") -> str | None:
    cmd = occ.command
    if not occurrence_can_rewrite(evt, occ) or not occurrence_dumps(cmd):
        return None
    url = occurrence_rewrite_url(cmd)
    if url is None:
        return None
    if not (ccx := ccx_bin()) or not ccx_supports("web", "read"):
        return None
    return f"{shlex.quote(ccx)} web read {shlex.quote(url)} --full"


def page_dump_note(evt: BaseHookEvent, pairs: "list[tuple[Occurrence, str]]") -> str:
    """Note the rewrite(s) and steer toward `ccx web outline`/`ccx web search`.

    A single distinct URL across `pairs` names that URL in the outline/search suggestions too;
    multiple distinct URLs fall back to a literal `<url>` placeholder — one trailing suggestion
    can't name several targets.
    """
    rewrites = []
    urls: set[str] = set()
    for occ, _ in pairs:
        cmd = occ.command
        url = occurrence_rewrite_url(cmd)
        urls.add(url)
        rewrites.append(f"`{cmd.executable} … {url}` → `ccx web read {url} --full`")
    target = urls.pop() if len(urls) == 1 else "<url>"
    return (
        f"Rewrote {', '.join(rewrites)}: readability-extracted markdown of the page, token-bounded — "
        "NOT the raw HTML (tags, scripts, and attributes are stripped). "
        f"Next time map its headings first with `ccx web outline {target}`, or ask a question with "
        f'`ccx web search {target} "<question>"`.'
    )


rewrite_command_occurrences(
    only_if=[PageDumpToStdout()],
    to=page_dump_to,
    note=page_dump_note,
    tests={
        Input(command="curl -s https://api.example.com/v1/data"): Allow(),
        Input(command="curl https://example.com/data.json"): Allow(),
        Input(command="curl https://example.com/api/v1"): Allow(),
        Input(command="curl -s 'https://example.com/data?format=json'"): Allow(),
        Input(command="curl 'https://example.com/report?q=graphql'"): Allow(),
        Input(command="curl https://example.com/graphql"): Allow(),
        Input(command="curl -H 'X-Auth: t' https://example.com"): Allow(),
        Input(command="curl -u user:pass https://example.com"): Allow(),
        Input(command="curl --retry 3 https://example.com"): Allow(),
        Input(command="curl --url=https://example.com/large.html"): Allow(),
        Input(command="curl --url https://example.com/large.html"): Allow(),
        Input(command="curl -f https://example.com"): Allow(),
        Input(command="curl --compressed https://example.com"): Allow(),
        Input(command="curl -- https://example.com"): Rewrite(pattern="ccx web read"),
        Input(command="wget -qO- -- https://example.com"): Rewrite(pattern="ccx web read"),
        Input(command="curl -s https://example.com 2>/dev/null"): Rewrite(pattern="ccx web read"),
        Input(command="curl https://example.com && echo done"): Rewrite(pattern="ccx web read"),
        Input(command="mkdir -p out && curl -s https://example.com/page"): Rewrite(pattern="mkdir -p out && "),
        Input(command="curl https://example.com 2>/dev/null"): Rewrite(pattern="ccx web read"),
        Input(command="timeout 10 curl https://example.com/big.html"): Allow(),
        Input(command="sudo curl https://example.com/page"): Allow(),
        Input(command="env TOKEN=x curl https://example.com/page"): Allow(),
        Input(command="TOKEN=x curl https://example.com/page"): Allow(),
        Input(command="H=$(curl -sL https://example.com/)"): Allow(),
        Input(command="export H=$(curl https://example.com/page)"): Allow(),
        Input(command="H=`curl -sL https://example.com/`"): Allow(),
        Input(command="H=$(wget -qO- https://example.com/)"): Allow(),
        Input(command="H=$(timeout 10 curl https://example.com/)"): Allow(),
        Input(command="H=$(curl https://example.com | jq .)"): Allow(),
        Input(command='H=$(curl https://example.com/); echo "$H"'): Allow(),
        Input(command="H=$(curl https://a.example/); curl -s https://b.example/"): Rewrite(pattern="ccx web read"),
        Input(command='echo "$(curl https://example.com)"'): Allow(),
        Input(command='printf "%s" "$(curl https://example.com)"'): Allow(),
        Input(command='curl -H "H: $(id)" https://example.com/x'): Allow(),
        Input(command="curl $URL"): Allow(),
        Input(command="curl https://example.com | jq ."): Allow(),
        Input(command="curl https://example.com | grep -c foo"): Allow(),
        Input(command='curl -H "H: $(id)" https://example.com/x | bash'): Allow(),
        Input(command="curl -o page.html https://example.com"): Allow(),
        Input(command="curl -sSfLo page.html https://example.com"): Allow(),
        Input(command="timeout 10 curl -o f https://example.com/f"): Allow(),
        Input(command="curl --url=http://localhost:8080/x"): Allow(),
        Input(command="curl https://example.com > page.html"): Allow(),
        Input(command="wget https://example.com"): Allow(),
        Input(command="curl -X POST https://api.example.com"): Allow(),
        Input(command="curl --json '{}' https://api.example.com/v1"): Allow(),
        Input(command="curl -I https://example.com"): Allow(),
        Input(command="curl http://localhost:3000/health"): Allow(),
        Input(command="curl https://127.0.0.1/metrics"): Allow(),
        Input(command="curl localhost:8080/health"): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"curl https://example.com\")\nasyncio.run(main())'"
        ): Allow(),
    },
)
