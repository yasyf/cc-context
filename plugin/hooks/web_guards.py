"""Web guards: steer whole-page web fetches toward the token-bounded ``ccx web`` ops.

A ``WebFetch`` pulls a whole page through a lossy extraction into context, and a raw
``curl``/``wget`` page dump to stdout floods it the same way — the web analogue of the
file-read floods :mod:`read_guards`/:mod:`command_rewrites` already steer. Both guards
point at the landed ``ccx web`` surface: ``ccx web outline <url>`` maps a page's headings,
``ccx web read <url> --section <ref>`` returns one budget-capped section, and
``ccx web search <url> "<question>"`` answers a prompt with the top-k relevant excerpts.
Their messages teach the same tiered handoff: a single-page lookup calls ``ccx web``
directly, while research across many pages spawns a cheap reader subagent that runs
``ccx web`` and returns only its conclusions, never the raw pages.

``WebSearch`` is deliberately unguarded: its snippets are already token-bounded, and it is
the discovery step that decides which page deserves a ``ccx web`` fetch in the first place.

The ``WebFetch`` guard blocks the *first* attempt per URL per session (``once`` self-gate);
a deliberate re-run of the same URL passes, so an auth-walled or JS-heavy page ``ccx web``
cannot serve stays reachable. The ``curl``/``wget`` guard fires only on an unpiped page GET
to stdout with a remote ``http(s)`` URL — a pipe (``| jq``), a disk sink (``-o``/``-O``/plain
``wget``/redirect), a non-GET method (``-X``/``-d``/``--json``/``-T``/``-I``), a localhost
target, and a scheme-less spelling all stay allowed by construction.
"""

from __future__ import annotations

import ipaddress
import re
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
    Tool,
    hook,
)

# Loopback and non-routable hosts a fetch to which never floods the shared context — a
# local dev server or private service, not a public page. Matched on the parsed hostname.
LOCAL_HOSTS = frozenset({"localhost", "0.0.0.0", "::1"})

# curl short options that consume a value (the rest of the bundle, or the next token when
# the char ends the group). Scanning left-to-right, the first of these ends the group, so a
# header/referer/cookie value is skipped rather than misread as the target URL.
CURL_VALUE_SHORTS = frozenset("oXdTFHAbcCeEuUmKwYyzrx")

# curl short options that mean "not a plain page GET to stdout" — a sink (``-o``/``-O``/``-J``)
# or a non-GET method/body (``-X``/``-d``/``-T``/``-F``/``-I``). Any one present allows the line.
CURL_ALLOW_SHORTS = frozenset("oOJXdTFI")

# curl long options with the same "not a page dump" meaning. ``--data*``/``--form*`` families
# are matched by prefix in :func:`_curl_dumps_page`.
CURL_ALLOW_LONGS = frozenset(
    {"--output", "--remote-name", "--remote-header-name", "--output-dir", "--request", "--json", "--upload-file", "--head"}
)

# curl long options that consume a value which can itself contain an ``http(s)`` token — skip
# the value so it is not mistaken for the target URL. (Sink/method value-longs never reach here:
# they allow the line first.)
CURL_VALUE_LONGS = frozenset({"--header", "--referer", "--user-agent", "--cookie", "--proxy"})


def _is_local_host(host: str) -> bool:
    """Whether ``host`` (a parsed, lowercased hostname) is loopback or a private-scope target.

    Mirrors the Go engine's ``localTarget``: named loopback (``localhost``, ``127.*``) and the
    ``.local``/``.internal`` suffixes, plus any IP literal that is loopback, private (RFC1918),
    link-local, or unspecified — so a target the engine fetches over plain HTTP is never guarded.
    """
    if host in LOCAL_HOSTS or host.startswith("127.") or host.endswith((".local", ".internal")):
        return True
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return False
    return ip.is_loopback or ip.is_private or ip.is_link_local or ip.is_unspecified


def _is_remote_url(token: str) -> bool:
    """Whether ``token`` is a remote ``http(s)://`` URL — the flood target the guards steer.

    A scheme-less spelling (``example.com``, ``localhost:8080/x``) never matches: without an
    ``http(s)://`` prefix it is deliberately a conservative allow, the right bias for a blocker.
    """
    if not re.match(r"^https?://", token, re.IGNORECASE):
        return False
    host = urlsplit(token).hostname or ""
    return bool(host) and not _is_local_host(host)


def _is_local_url(url: str) -> bool:
    """Whether ``url`` targets a loopback/private host — never worth guarding a fetch to."""
    host = urlsplit(url).hostname
    return bool(host) and _is_local_host(host)


class WholePageWebFetch(CustomCondition):
    """Matches the first ``WebFetch`` of a given remote URL this session.

    A ``WebFetch`` pulls the whole page through a lossy extraction. This fires on the first
    attempt per URL (``once`` self-gate, precedent: json_guards' ``SeenEmittingJson``), so a
    deliberate same-URL re-run passes — the escape hatch for a page ``ccx web`` can't serve.
    A local/loopback target never matches.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        url = (evt._tool_input.get("url") or "").strip()
        if not url or _is_local_url(url):
            return False
        return evt.ctx.s.once(url, scope="ccx-web-fetch")


hook(
    Event.PreToolUse,
    only_if=[Tool("WebFetch"), WholePageWebFetch()],
    message=(
        "BLOCKED: WebFetch pulls a whole page through a lossy extraction into context. "
        "One page: `ccx web outline <url>` maps its headings, `ccx web read <url> --section <ref>` "
        "returns one budget-capped section, and `ccx web search <url> \"<question>\"` answers your "
        "prompt with the top-k relevant excerpts (your WebFetch prompt works verbatim). "
        "Researching across pages? Spawn a cheap reader subagent that runs `ccx web` and returns "
        "only conclusions. Escape hatch — `ccx web` can't serve this URL: re-run the same WebFetch; "
        "a repeat of the URL passes."
    ),
    block=True,
    tests={
        Input(tool="WebFetch", tool_input={"url": "https://docs.example.com/en/guide/config"}): Block(
            pattern="ccx web outline"
        ),
        Input(tool="WebFetch", tool_input={"url": "http://localhost:3000/health"}): Allow(),
        Input(tool="WebFetch", tool_input={"url": "http://127.0.0.1:8080/metrics"}): Allow(),
        # Private-scope IP literals are local like the Go engine's localTarget — never guarded.
        Input(tool="WebFetch", tool_input={"url": "http://192.168.1.1/admin"}): Allow(),
    },
)


def _sinks_stdout(cmd: Command) -> bool:
    """Whether ``cmd`` redirects its stdout to a file — a disk sink, not a context flood.

    A stderr-only redirect (``2>/dev/null``) leaves stdout flooding, so it is *not* a sink and
    the guard still blocks; only an output redirect on the default/stdout fd counts.
    """
    return any(
        r.op in (">", ">>", ">|", ">&") and (r.fd is None or r.fd == 1) for r in cmd.redirects
    )


def _scan_curl_shorts(token: str) -> tuple[bool, bool]:
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


def _curl_dumps_page(args: tuple[str, ...]) -> bool:
    """Whether a ``curl`` argv is a plain page GET to stdout — a remote URL, no sink, no method flag."""
    remote = False
    i = 0
    n = len(args)
    while i < n:
        a = args[i]
        if a == "--":
            return remote or any(_is_remote_url(t) for t in args[i + 1 :])
        if a.startswith("--"):
            name = a.split("=", 1)[0]
            if name in CURL_ALLOW_LONGS or name.startswith(("--data", "--form")):
                return False
            if name == "--url":
                # curl's long form for the target URL (`--url=<u>` / `--url <u>`) — scan the
                # value like a positional, since the plain page GET can name it this way.
                val = a.split("=", 1)[1] if "=" in a else (args[i + 1] if i + 1 < n else "")
                if _is_remote_url(val):
                    remote = True
                i += 1 if "=" in a else 2
                continue
            if "=" not in a and name in CURL_VALUE_LONGS:
                i += 2
                continue
            i += 1
            continue
        if a.startswith("-") and len(a) > 1:
            allow, consumes_next = _scan_curl_shorts(a)
            if allow:
                return False
            i += 2 if consumes_next else 1
            continue
        if _is_remote_url(a):
            remote = True
        i += 1
    return remote


def _wget_to_stdout(args: tuple[str, ...]) -> bool:
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


def _wget_dumps_page(args: tuple[str, ...]) -> bool:
    """Whether a ``wget`` argv dumps a remote page to stdout."""
    return _wget_to_stdout(args) and any(_is_remote_url(a) for a in args)


class PageDumpToStdout(CustomCommandLineCondition):
    """Matches an unpiped ``curl``/``wget`` page GET to stdout with a remote ``http(s)`` URL.

    Allowed by construction: a command whose stdout is piped (``| jq``) or redirected to a file,
    a curl sink (``-o``/``-O``) or non-GET method (``-X``/``-d``/``--json``/``-T``/``-I``), a
    non-stdout ``wget`` (its default disk download), a localhost target, and a scheme-less URL.
    An unpiped ``curl -s <api>`` blocks on purpose — a raw JSON dump floods context the same way,
    and the pipe hatch (`… | jq`) is right there.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        for cmd, op in cl.parts:
            if op == "|" or _sinks_stdout(cmd):
                continue
            # Match on the unwrapped executable so a wrapper prefix (`timeout 10 curl …`,
            # `sudo curl …`, `env FOO=1 curl …`) can't slip the page dump past the block.
            inner = cmd.unwrapped
            if inner.executable == "curl" and _curl_dumps_page(inner.args):
                return True
            if inner.executable == "wget" and _wget_dumps_page(inner.args):
                return True
        return False


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), PageDumpToStdout()],
    message=(
        "BLOCKED: `curl`/`wget` dumping a page to stdout floods context. "
        "One page: `ccx web outline <url>` maps its headings, then `ccx web read <url> --section <ref>` "
        "for the part you need, or `ccx web search <url> \"<question>\"` for the top-k relevant excerpts. "
        "Researching across pages? Spawn a cheap reader subagent that runs `ccx web` and returns only "
        "conclusions. Escape hatches — API/JSON: pipe it (`curl … | jq`); download: `curl -o <file>` / "
        "plain `wget`; localhost stays allowed."
    ),
    block=True,
    tests={
        Input(command="curl https://example.com"): Block(pattern="ccx web outline"),
        Input(command="curl -sL https://example.com"): Block(),
        Input(command="wget -qO- https://example.com"): Block(pattern="ccx web outline"),
        Input(command="wget -O - https://example.com"): Block(),
        Input(command="curl https://example.com && echo done"): Block(),  # stdout unconsumed
        Input(command="curl -s https://api.example.com/v1/data"): Block(),  # raw JSON dump, deliberate
        Input(command="curl https://example.com 2>/dev/null"): Block(),  # stderr-only redirect still floods stdout
        Input(command="timeout 10 curl https://example.com/big.html"): Block(),  # wrapper prefix
        Input(command="sudo curl https://example.com/page"): Block(),  # wrapper prefix
        Input(command="curl --url=https://example.com/large.html"): Block(),  # --url= long form
        Input(command="curl --url https://example.com/large.html"): Block(),  # --url two-token form
        Input(command="curl https://example.com | jq ."): Allow(),
        Input(command="curl https://example.com | grep -c foo"): Allow(),
        Input(command="curl -o page.html https://example.com"): Allow(),
        Input(command="curl -sSfLo page.html https://example.com"): Allow(),
        Input(command="timeout 10 curl -o f https://example.com/f"): Allow(),  # wrapped disk sink
        Input(command="curl --url=http://localhost:8080/x"): Allow(),  # --url= localhost
        Input(command="curl https://example.com > page.html"): Allow(),
        Input(command="wget https://example.com"): Allow(),  # wget's default disk download
        Input(command="curl -X POST https://api.example.com"): Allow(),
        Input(command="curl --json '{}' https://api.example.com/v1"): Allow(),  # request-body flag, not GET
        Input(command="curl -I https://example.com"): Allow(),  # HEAD request
        Input(command="curl http://localhost:3000/health"): Allow(),
        Input(command="curl https://127.0.0.1/metrics"): Allow(),
        Input(command="curl http://10.0.0.5/status"): Allow(),  # RFC1918 private IP — local
        Input(command="curl localhost:8080/health"): Allow(),  # scheme-less — conservative allow
        # `ccx exec` pass-through is deliberate: the curl inside sh() is one opaque token, and
        # internal/codeexec/sh.go owns in-sandbox policy, not hooks.
        Input(
            command="ccx exec 'async def main(): return await sh(\"curl https://example.com\")\nasyncio.run(main())'"
        ): Allow(),
    },
)
