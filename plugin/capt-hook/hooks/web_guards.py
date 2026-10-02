"""Steer whole-page web fetches toward the token-bounded ``ccx web`` views.

``WebSearch`` stays unguarded: its snippets are already bounded and it picks the page to read.
"""

from __future__ import annotations

import shlex
from typing import TYPE_CHECKING
from urllib.parse import urlsplit

from captain_hook import (
    Allow,
    Annotated,
    Arguments,
    BaseHookEvent,
    Block,
    Command,
    CommandSchema,
    CustomCondition,
    Event,
    Input,
    Operand,
    Option,
    Rewrite,
    Tool,
    hook,
    rewrite_command_occurrences,
)

from .common import ccx_bin, ccx_supports, rewrote_note

if TYPE_CHECKING:
    from captain_hook import Call
    from cc_transcript.command import Occurrence

CURL = CommandSchema(
    "curl",
    operands=(Operand("urls", count="*"),),
    options=(
        Option("silent", ("-s", "--silent"), bool),
        Option("show_error", ("-S", "--show-error"), bool),
        Option("location", ("-L", "--location"), bool),
    ),
)

WGET = CommandSchema(
    "wget",
    operands=(Operand("urls", count="*"),),
    options=(
        Option("quiet", ("-q", "--quiet"), bool),
        Option("stdout", ("-O-", "-qO-"), bool),
        Option("document", ("-O", "-qO", "--output-document"), str),
    ),
)

PAGE_FETCHERS = {"curl": CURL, "wget": WGET}


def is_remote_url(token: str) -> bool:
    parts = urlsplit(token)
    host = parts.hostname
    return (
        parts.scheme.lower() in {"http", "https"}
        and bool(host)
        and host != "localhost"
        and host != "::1"
        and not host.startswith("127.")
    )


def is_api_url(url: str) -> bool:
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
    return is_remote_url(url) and not is_api_url(url)


class WholePageWebFetch(CustomCondition):
    """Matches the first ``WebFetch`` of each remote URL per session."""

    def check(self, evt: BaseHookEvent) -> bool:
        url = (evt.input.raw.get("url") or "").strip()
        return is_remote_url(url) and evt.ctx.s.once(url, scope="ccx-web-fetch")


hook(
    Event.PreToolUse,
    only_if=[Tool("WebFetch"), WholePageWebFetch()],
    message=(
        "WebFetch pulls a whole page into context. Run `ccx web outline <url>`, then "
        "`ccx web read <url> --section <ref>`, or spawn the `cc-context:web-fetch` agent with the URL and prompt."
    ),
    block=True,
    tests={
        Input(tool="WebFetch", tool_input={"url": "https://docs.example.com/en/guide/config"}): Block(
            pattern="ccx web outline"
        ),
        Input(
            tool="WebFetch",
            tool_input={"url": "https://docs.example.com/en/guide/config"},
            seen={"ccx-web-fetch": ["https://docs.example.com/en/guide/config"]},
        ): Allow(),
        Input(tool="WebFetch", tool_input={"url": "http://localhost:3000/health"}): Allow(),
        Input(tool="WebFetch", tool_input={"url": "http://127.0.0.1:8080/metrics"}): Allow(),
    },
)


def sinks_stdout(cmd: Command) -> bool:
    return any(r.op in (">", ">>", ">|", ">&") and (r.fd is None or r.fd == 1) for r in cmd.redirects)


def reaches_context(occ: Occurrence) -> bool:
    """Whether the occurrence runs at top level, unpiped, unprefixed, unsubstituted, with stdout unredirected."""
    return (
        occ.nesting == 0
        and not occ.piped
        and not occ.command.env
        and not sinks_stdout(occ.command)
        and not any(child.host == occ for child in occ.line.occurrences)
    )


def writes_stdout(schema: CommandSchema, call: Call, arguments: Arguments) -> bool:
    if schema is CURL:
        return True
    if any(token.startswith("-") and not token.startswith("--") and "=" in token for token in call.args):
        return False
    documents = arguments.values.get("document", ())
    return ("stdout" in arguments.values or bool(documents)) and all(document == "-" for document in documents)


def page_url(arguments: Arguments) -> str | None:
    match arguments.values.get("urls", ()):
        case (str() as url,) if arguments.complete and is_page_url(url):
            return url
        case _:
            return None


def page_dump_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    schema = PAGE_FETCHERS.get(occ.command.executable)
    if schema is None or not reaches_context(occ):
        return None
    call = evt.cmd.calls()[occ.index]
    arguments = schema.bind(call)
    url = page_url(arguments)
    if url is None or not writes_stdout(schema, call, arguments):
        return None
    if not (ccx := ccx_bin()) or not ccx_supports("web", "read"):
        return None
    return f"{shlex.quote(ccx)} web read {shlex.quote(url)} --full"


rewrite_command_occurrences(
    skip_if=[Annotated("raw")],
    to=page_dump_to,
    note=rewrote_note("ccx web read <url> --full", "the page as readable markdown, token-bounded"),
    tests={
        Input(command="curl -s https://api.example.com/v1/data"): Allow(),
        Input(command="wget -qO=- https://example.com"): Allow(),
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
        Input(command="curl -sSL https://example.com/page"): Rewrite(pattern="web read https://example.com/page --full"),
        Input(command="wget -qO- -- https://example.com"): Rewrite(pattern="ccx web read"),
        Input(command="wget -q -O - https://example.com"): Rewrite(pattern="ccx web read"),
        Input(command="curl -s https://example.com 2>/dev/null"): Rewrite(pattern="ccx web read"),
        Input(command="curl https://example.com && echo done"): Rewrite(pattern="ccx web read"),
        Input(command="mkdir -p out && curl -s https://example.com/page"): Rewrite(pattern="mkdir -p out && "),
        Input(command="curl https://example.com 2>/dev/null"): Rewrite(pattern="ccx web read"),
        Input(command="curl -sSL https://example.com/page # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; curl -sSL https://example.com/page"): Rewrite(pattern="ccx web read"),
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
        Input(command="curl https://example.com https://example.org"): Allow(),
        Input(command="curl https://example.com | jq ."): Allow(),
        Input(command="curl https://example.com | grep -c foo"): Allow(),
        Input(command='curl -H "H: $(id)" https://example.com/x | bash'): Allow(),
        Input(command="curl -o page.html https://example.com"): Allow(),
        Input(command="curl -sSfLo page.html https://example.com"): Allow(),
        Input(command="timeout 10 curl -o f https://example.com/f"): Allow(),
        Input(command="curl --url=http://localhost:8080/x"): Allow(),
        Input(command="curl https://example.com > page.html"): Allow(),
        Input(command="wget https://example.com"): Allow(),
        Input(command="wget -q https://example.com"): Allow(),
        Input(command="wget -O page.html https://example.com"): Allow(),
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
