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
cannot serve stays reachable. The ``curl``/``wget`` guard fires only on an unpiped page GET
to stdout with a remote ``http(s)`` URL — a pipe (``| jq``), a disk sink (``-o``/``-O``/plain
``wget``/redirect), a non-GET method (``-X``/``-d``/``--json``/``-T``/``-I``), a localhost
target, and a scheme-less spelling all stay allowed by construction. When the fetch is a plain
page GET (``curl -sL <url>`` / ``wget -qO- <url>``) it is *rewritten* to ``ccx web read <url>
--full`` — readability-extracted markdown, token-bounded — gated on the ``ccx web`` surface being present
(``ccx_supports``); an API/JSON URL (``api.*`` host, ``api`` path segment, ``.json`` path), an
extra flag readability can't honor, or a wrapped line falls back to the block — a chained
line (``&&``/``;``) rewrites occurrence-by-occurrence instead.
A dump whose output an assignment substitution captures is a sink — consumed programmatically,
so it is left untouched rather than rewritten.
"""

from __future__ import annotations

import ipaddress
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
# are matched by prefix in :func:`curl_dumps_page`.
CURL_ALLOW_LONGS = frozenset(
    {"--output", "--remote-name", "--remote-header-name", "--output-dir", "--request", "--json", "--upload-file", "--head"}
)

# curl long options that consume a value which can itself contain an ``http(s)`` token — skip
# the value so it is not mistaken for the target URL. (Sink/method value-longs never reach here:
# they allow the line first.)
CURL_VALUE_LONGS = frozenset({"--header", "--referer", "--user-agent", "--cookie", "--proxy"})

# curl short chars that keep a fetch a plain page GET, safe to rewrite to ``ccx web read``:
# silent (s), show-error (S), fail (f), location/follow (L). A bundle of only these + one remote
# URL maps; any other char (auth, header, method, …) makes the request un-mappable → block.
CURL_REWRITE_SHORTS = frozenset("sSLf")
CURL_REWRITE_LONGS = frozenset({"--silent", "--show-error", "--fail", "--location", "--compressed"})

# wget spellings that keep a fetch a plain quiet stdout dump, safe to rewrite. The stdout
# output-document is handled per-token (``-O -`` / ``-qO-`` / ``--output-document=-``).
WGET_QUIET = frozenset({"-q", "--quiet"})
WGET_STDOUT_FLAGS = frozenset({"-O", "--output-document"})

# Shell builtins that bind their arguments to variables. A `$(curl …)` value one of these captures
# lands in a variable, never on stdout, so a page dump inside it floods no context.
ASSIGNMENT_BUILTINS = frozenset({"export", "local", "declare", "readonly", "typeset"})


def is_local_host(host: str) -> bool:
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


def is_remote_url(token: str) -> bool:
    """Whether ``token`` is a remote ``http(s)://`` URL — the flood target the guards steer.

    A scheme-less spelling (``example.com``, ``localhost:8080/x``) never matches: without an
    ``http(s)://`` prefix it is deliberately a conservative allow, the right bias for a blocker.
    """
    if not re.match(r"^https?://", token, re.IGNORECASE):
        return False
    host = urlsplit(token).hostname or ""
    return bool(host) and not is_local_host(host)


def is_local_url(url: str) -> bool:
    """Whether ``url`` targets a loopback/private host — never worth guarding a fetch to."""
    host = urlsplit(url).hostname
    return bool(host) and is_local_host(host)


class WholePageWebFetch(CustomCondition):
    """Matches the first ``WebFetch`` of a given remote URL this session.

    A ``WebFetch`` pulls the whole page through a lossy extraction. This fires on the first
    attempt per URL (``once`` self-gate, precedent: json_guards' ``SeenEmittingJson``), so a
    deliberate same-URL re-run passes — the escape hatch for a page ``ccx web`` can't serve.
    A local/loopback target never matches.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        url = (evt._tool_input.get("url") or "").strip()
        if not url or is_local_url(url):
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
        # Private-scope IP literals are local like the Go engine's localTarget — never guarded.
        Input(tool="WebFetch", tool_input={"url": "http://192.168.1.1/admin"}): Allow(),
    },
)


def sinks_stdout(cmd: Command) -> bool:
    """Whether ``cmd`` redirects its stdout to a file — a disk sink, not a context flood.

    A stderr-only redirect (``2>/dev/null``) leaves stdout flooding, so it is *not* a sink and
    the guard still blocks; only an output redirect on the default/stdout fd counts.
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
            return remote or any(is_remote_url(t) for t in args[i + 1 :])
        if a.startswith("--"):
            name = a.split("=", 1)[0]
            if name in CURL_ALLOW_LONGS or name.startswith(("--data", "--form")):
                return False
            if name == "--url":
                # curl's long form for the target URL (`--url=<u>` / `--url <u>`) — scan the
                # value like a positional, since the plain page GET can name it this way.
                val = a.split("=", 1)[1] if "=" in a else (args[i + 1] if i + 1 < n else "")
                if is_remote_url(val):
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
        if is_remote_url(a):
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
    return wget_to_stdout(args) and any(is_remote_url(a) for a in args)


class PageDumpToStdout(CustomCommandLineCondition):
    """Matches an unpiped ``curl``/``wget`` page GET to stdout with a remote ``http(s)`` URL.

    Allowed by construction: a command whose stdout is piped (``| jq``) or redirected to a file,
    a curl sink (``-o``/``-O``) or non-GET method (``-X``/``-d``/``--json``/``-T``/``-I``), a
    non-stdout ``wget`` (its default disk download), a localhost target, a scheme-less URL, and
    a substitution whose output an assignment captures (``H=$(curl …)``, ``export H=$(curl …)``).
    An unpiped ``curl -s <api>`` blocks on purpose — a raw JSON dump floods context the same way,
    and the pipe hatch (`… | jq`) is right there.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        for occ in cl.occurrences:
            cmd = occ.command
            if occ.piped or sinks_stdout(cmd) or occurrence_captured(occ):
                continue
            # `occurrence_dumps` matches on the unwrapped executable, so a wrapper prefix
            # (`timeout 10 curl …`, `sudo curl …`, `env FOO=1 curl …`) can't slip the dump past.
            if occurrence_dumps(cmd):
                return True
        return False


def curl_rewrite_url(args: tuple[str, ...]) -> str | None:
    """The single remote URL of a rewritable ``curl`` page dump, or None to fall back to the block.

    Rewritable iff every flag is a plain-GET-to-stdout spelling (:data:`CURL_REWRITE_SHORTS`
    bundles / :data:`CURL_REWRITE_LONGS`) and exactly one remote ``http(s)`` URL is present. Any
    other flag (``-H``/``-u``/``--retry``/``--url``…), a second URL, or a non-remote positional
    returns None, so the guard blocks with its steer rather than drop a flag that changes the request.
    """
    url = None
    for a in args:
        if a.startswith("--"):
            if a not in CURL_REWRITE_LONGS:
                return None
            continue
        if a.startswith("-") and len(a) > 1:
            if any(c not in CURL_REWRITE_SHORTS for c in a[1:]):
                return None
            continue
        if not is_remote_url(a) or url is not None:
            return None
        url = a
    return url


def wget_rewrite_url(args: tuple[str, ...]) -> str | None:
    """The single remote URL of a rewritable ``wget`` stdout dump, or None to fall back to the block.

    Rewritable iff every flag is a quiet/stdout spelling (``-q``/``--quiet``, ``-O -``/``-qO-``/
    ``-qO -``/``--output-document=-``) and exactly one remote URL is present; any other flag, a
    non-stdout ``-O``, or a second URL returns None so the guard blocks rather than mis-rewrite.
    """
    url = None
    skip_next = False
    for i, a in enumerate(args):
        if skip_next:
            skip_next = False
            continue
        if a in WGET_QUIET:
            continue
        if a in WGET_STDOUT_FLAGS:
            if i + 1 < len(args) and args[i + 1] == "-":
                skip_next = True
                continue
            return None
        if a.startswith("--output-document="):
            if a.split("=", 1)[1] != "-":
                return None
            continue
        if a.startswith("-") and not a.startswith("--") and len(a) > 1:
            ok, body, k = True, a[1:], 0
            while k < len(body):
                if body[k] == "q":
                    k += 1
                    continue
                if body[k] == "O":
                    rest = body[k + 1 :]
                    if rest:
                        ok = rest == "-"
                    else:
                        ok = i + 1 < len(args) and args[i + 1] == "-"
                        skip_next = ok
                    break
                ok = False
                break
            if not ok:
                return None
            continue
        if not is_remote_url(a) or url is not None:
            return None
        url = a
    return url


def is_api_url(url: str) -> bool:
    """Whether ``url`` looks like a JSON/API endpoint readability extraction would mangle.

    An ``api.*`` host, an ``api`` path segment, a ``.json`` or ``/graphql`` path, or a query
    string naming ``json``/``graphql`` (case-insensitive) all signal structured data, not an
    article — the block steers those to the pipe hatch (`… | jq`), not `ccx web read`. The
    query string matters because ``…/data?format=json`` and a ``/graphql`` endpoint return
    JSON that readability would shred.
    """
    parts = urlsplit(url)
    if (parts.hostname or "").lower().startswith("api."):
        return True
    path = parts.path.rstrip("/")
    if path.endswith(".json") or path.endswith("/graphql"):
        return True
    if any(kw in parts.query.lower() for kw in ("json", "graphql")):
        return True
    return "api" in [seg for seg in parts.path.split("/") if seg]


def occurrence_can_rewrite(occ: "Occurrence") -> bool:
    """Report whether an occurrence is outside a pipe and carries no redirects."""
    return not occ.piped and not occ.command.redirects


def host_is_assignment(cmd: Command) -> bool:
    """Whether ``cmd`` is an assignment builtin (:data:`ASSIGNMENT_BUILTINS`) capturing a value into a variable."""
    return bool(cmd.argv) and cmd.argv[0] in ASSIGNMENT_BUILTINS


def occurrence_captured(occ: Occurrence) -> bool:
    """Whether a substituted occurrence's stdout is captured by a variable assignment, never flooding context.

    A bare ``H=$(curl …)`` surfaces the substitution with no host command (an assignment-only
    line); ``export H=$(curl …)`` / ``local H=$(curl …)`` surface it hosted by the assignment
    builtin. Either way the page is captured into a variable, reaching no context — the capture
    runs untouched, and a splice there would hand the consumer extracted markdown instead of raw
    HTML (the ``H=$(curl …)`` incident). A substitution a printing command consumes
    (``echo "$(curl …)"``) is hosted by that command, floods context, and stays guarded. A
    top-level occurrence (``nesting == 0``) is never a capture.
    """
    if occ.nesting == 0:
        return False
    host = occ.host
    return host is None or host_is_assignment(host.command)


def occurrence_dumps(cmd: Command) -> bool:
    """Whether ``cmd`` (seen through any wrapper) is a ``curl``/``wget`` page dump to stdout.

    Matches on the unwrapped executable, same as :class:`PageDumpToStdout`, so a wrapper prefix
    (``timeout``/``sudo``/``env``) still counts as a dump — the condition sees through wrappers to
    match/block; :func:`occurrence_rewrite_url` does not, so a wrapped dump falls to the block.
    """
    inner = cmd.unwrapped
    if inner.executable == "curl":
        return curl_dumps_page(inner.args)
    if inner.executable == "wget":
        return wget_dumps_page(inner.args)
    return False


def occurrence_rewrite_url(cmd: Command) -> str | None:
    """The single remote URL of a directly-invoked ``curl``/``wget`` page dump, or None.

    Only a direct ``curl``/``wget`` maps: a wrapper prefix (``timeout``/``sudo``/``env``) leaves
    ``cmd.executable`` as the wrapper, so it returns None and the occurrence blocks (dropping the
    wrapper would change the command).
    """
    if cmd.executable == "curl":
        return curl_rewrite_url(cmd.args)
    if cmd.executable == "wget":
        return wget_rewrite_url(cmd.args)
    return None


def page_dump_to(evt: BaseHookEvent, occ: "Occurrence") -> str | None:
    cmd = occ.command
    if not occurrence_can_rewrite(occ) or occurrence_captured(occ) or not occurrence_dumps(cmd):
        return None
    url = occurrence_rewrite_url(cmd)
    if url is None or is_api_url(url):
        return None
    if not ccx_supports("web", "read") or not (ccx := ccx_bin()):
        return None
    return f"{shlex.quote(ccx)} web read {shlex.quote(url)} --full"


def page_dump_blocks(evt: BaseHookEvent, occ: "Occurrence") -> bool:
    cmd = occ.command
    return (
        occurrence_can_rewrite(occ)
        and occurrence_dumps(cmd)
        and not occurrence_captured(occ)
        and (cmd.span is None or page_dump_to(evt, occ) is None)
    )


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
    block_if=page_dump_blocks,
    block=(
        "BLOCKED: `curl`/`wget` dumping a page to stdout floods context. "
        "Answering a question from the page? Spawn the `cc-context:web-fetch` agent (URL + "
        "question) — it returns only the cited answer. Reading inline? `ccx web outline <url>` "
        "maps its headings, then `ccx web read <url> --section <ref>` for the part you need, or "
        "`ccx web search <url> \"<question>\"` for the top-k relevant excerpts. "
        "Researching across pages? Spawn `cc-context:web-researcher`. "
        "Escape hatches — API/JSON: pipe it (`curl … | jq`); download: `curl -o <file>` / "
        "plain `wget`; localhost stays allowed."
    ),
    note=page_dump_note,
    # The rewrite itself is gated on `ccx_supports("web", "read")`, so its outcome is
    # environment-dependent — those rows live in test_web_guards.py with ccx_supports
    # monkeypatched. Inline coverage is the block fallbacks (`to` returns None *before* the
    # gate, so they block regardless of the local ccx) and the Allow neighbors.
    tests={
        Input(command="curl -s https://api.example.com/v1/data"): Block(pattern="ccx web outline"),  # api.* host
        Input(command="curl https://example.com/data.json"): Block(),  # .json path
        Input(command="curl https://example.com/api/v1"): Block(),  # `api` path segment
        Input(command="curl -s 'https://example.com/data?format=json'"): Block(),  # json in query string
        Input(command="curl 'https://example.com/report?q=graphql'"): Block(),  # graphql in query string
        Input(command="curl https://example.com/graphql"): Block(),  # /graphql endpoint path
        Input(command="curl -H 'X-Auth: t' https://example.com"): Block(),  # header flag → un-mappable
        Input(command="curl -u user:pass https://example.com"): Block(),  # auth flag → un-mappable
        Input(command="curl --retry 3 https://example.com"): Block(),  # retry flag → un-mappable
        Input(command="curl --url=https://example.com/large.html"): Block(),  # --url long form
        Input(command="curl --url https://example.com/large.html"): Block(),  # --url two-token form
        # A compound line's curl occurrence rewrites in place; the sibling survives verbatim.
        Input(command="curl https://example.com && echo done"): Rewrite(pattern="ccx web read"),
        Input(command="mkdir -p out && curl -s https://example.com/page"): Rewrite(pattern="mkdir -p out && "),
        Input(command="curl https://example.com 2>/dev/null"): Block(),  # redirect
        Input(command="timeout 10 curl https://example.com/big.html"): Block(),  # wrapper prefix
        Input(command="sudo curl https://example.com/page"): Block(),  # wrapper prefix
        # Assignment-captured dumps are sinks: the capture is consumed programmatically — never
        # rewrite. The later `echo "$H"` re-dump is outside the guard's data-flow view: accepted.
        Input(command="H=$(curl -sL https://example.com/)"): Allow(),  # the incident shape
        Input(command="export H=$(curl https://example.com/page)"): Allow(),
        Input(command="H=`curl -sL https://example.com/`"): Allow(),  # backtick form
        Input(command="H=$(wget -qO- https://example.com/)"): Allow(),
        Input(command="H=$(timeout 10 curl https://example.com/)"): Allow(),  # wrapped + hosted
        Input(command="H=$(curl https://example.com | jq .)"): Allow(),  # piped capture — the pipe bounds it
        Input(command='H=$(curl https://example.com/); echo "$H"'): Allow(),  # the accepted hole
        # A captured sibling neither blocks nor rewrites; the bare dump still splices.
        Input(command="H=$(curl https://a.example/); curl -s https://b.example/"): Rewrite(pattern="ccx web read"),
        # A substitution a printing command consumes is hosted by that command (not an assignment),
        # so the expanded page reaches stdout through it — it blocks like a bare dump.
        Input(command='echo "$(curl https://example.com)"'): Block(pattern="ccx web outline"),
        Input(command='printf "%s" "$(curl https://example.com)"'): Block(pattern="ccx web outline"),
        Input(command="curl https://example.com | jq ."): Allow(),
        Input(command="curl https://example.com | grep -c foo"): Allow(),
        # An argument substitution must not defeat the pipe exception: the pipe still sinks stdout.
        Input(command='curl -H "H: $(id)" https://example.com/x | bash'): Allow(),
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
