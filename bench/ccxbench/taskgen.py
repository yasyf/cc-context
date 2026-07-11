"""Generate the usage-shaped benchmark corpus: complex tasks over pinned real repos plus a control.

Every headline task runs against a genuinely large real checkout (tornado, click) whose
`gold.traversal_files` clear the size floor — big enough that ccx's fixed overhead is noise.
Navigation and trace locate a declaration on a big file or at the end of a cross-file call chain;
large_context enumerates the complete set satisfying a body-level predicate; diff_review lists the
functions a build-time-generated patch touched; targeted_edit is graded by a self-contained,
offline check; intent_search names the module implementing a whole-repo concept. Every answer is
derived at build time from the pinned checkout (see `goldgen`) — no line numbers live here.
non_regression is the control family (repo `empty`, `ccx_helps=False`): it proves ccx adds no harm
where it cannot help and is excluded from every headline.
"""

from __future__ import annotations

from .types import Grader, Task

NAV_SCHEMA = {
    "type": "object",
    "properties": {"file": {"type": "string"}, "line": {"type": "integer"}},
    "required": ["file", "line"],
    "additionalProperties": False,
}
FILE_SCHEMA = {
    "type": "object",
    "properties": {"file": {"type": "string"}},
    "required": ["file"],
    "additionalProperties": False,
}
MEMBERS_SCHEMA = {
    "type": "object",
    "properties": {"members": {"type": "array", "items": {"type": "string"}}},
    "required": ["members"],
    "additionalProperties": False,
}
SYMBOLS_SCHEMA = {
    "type": "object",
    "properties": {"symbols": {"type": "array", "items": {"type": "string"}}},
    "required": ["symbols"],
    "additionalProperties": False,
}
EDIT_SCHEMA = {
    "type": "object",
    "properties": {"changed_file": {"type": "string"}, "summary": {"type": "string"}},
    "required": ["changed_file"],
    "additionalProperties": False,
}
ANSWER_SCHEMA = {
    "type": "object",
    "properties": {"answer": {"type": "string"}},
    "required": ["answer"],
    "additionalProperties": False,
}

DIFF_PROMPT = (
    "The working tree has uncommitted changes, possibly across multiple files. Review the diff and "
    "determine which top-level functions or methods had their bodies modified relative to HEAD. "
    "List their names exactly once each."
)

# Pins the two variance vectors an edit task otherwise leaves open — running the repo's own test
# suite and installing the package/deps — without naming the offline grader's mechanics.
EDIT_SUFFIX = (
    " Make the edit directly in the source. Do not run the repository's test suite and do not "
    "install the package or any dependencies; your change is verified separately after you finish."
)


def _nav(tid: str, repo: str, file: str, decl: str, prompt: str) -> Task:
    """Navigation: find a declaration on a big file. `gold.line` is derived from `decl` at build."""
    return Task(
        id=tid,
        category="navigation",
        repo=repo,
        prompt=prompt + " Respond with the repo-relative file path and the 1-based declaration line.",
        schema=NAV_SCHEMA,
        grader=Grader("file_line", {"line_tolerance": 2}),
        gold={"file": file, "decl": decl, "traversal_files": [file]},
    )


def _trace(tid: str, repo: str, file: str, decl: str, prompt: str, chain: list[str]) -> Task:
    """Trace: the terminal function of a cross-file call chain; `gold.line` derived from `decl`."""
    return Task(
        id=tid,
        category="trace",
        repo=repo,
        prompt=prompt + " Respond with the repo-relative file path and the 1-based declaration line "
        "of that function or method.",
        schema=NAV_SCHEMA,
        grader=Grader("file_line", {"line_tolerance": 2}),
        gold={"file": file, "decl": decl, "traversal_files": chain},
    )


def _lc(tid: str, repo: str, prompt: str, predicate: dict, traversal: list[str]) -> Task:
    """Large-context enumeration; `gold.members` is recomputed from `predicate` at build."""
    return Task(
        id=tid,
        category="large_context",
        repo=repo,
        prompt=prompt,
        schema=MEMBERS_SCHEMA,
        grader=Grader("set_match", {"field": "members", "mode": "equal", "lower": True}),
        gold={"lc_predicate": predicate, "traversal_files": traversal},
    )


def _diff(tid: str, repo: str, prompt: str, edits: list[dict]) -> Task:
    """Diff-review; the patch and `gold.symbols`/`traversal_files` are generated from `edits`."""
    return Task(
        id=tid,
        category="diff_review",
        repo=repo,
        prompt=prompt,
        schema=SYMBOLS_SCHEMA,
        grader=Grader("set_match", {"field": "symbols", "mode": "equal", "lower": True}),
        gold={"diff_spec": {"edits": edits}},
    )


def _edit(tid: str, repo: str, prompt: str, cmd: str, check: str, solution: list[dict], traversal: list[str]) -> Task:
    """Targeted edit graded by a self-contained, offline `test_run`; pristine must fail, solution pass."""
    return Task(
        id=tid,
        category="targeted_edit",
        repo=repo,
        prompt=prompt + EDIT_SUFFIX,
        schema=EDIT_SCHEMA,
        grader=Grader("test_run", {"cmd": cmd, "timeout_s": 120}),
        gold={"check": check, "solution_edits": solution, "traversal_files": traversal},
    )


def _intent(tid: str, repo: str, prompt: str, file: str, traversal: list[str]) -> Task:
    """Intent search: name the module implementing a whole-repo concept (file_match gold)."""
    return Task(
        id=tid,
        category="intent_search",
        repo=repo,
        prompt=prompt + " Respond with the file path relative to the repo root.",
        schema=FILE_SCHEMA,
        grader=Grader("file_match", {}),
        gold={"file": file, "traversal_files": traversal},
    )


def navigation_tasks() -> list[Task]:
    return [
        _nav("nav-tornado-find-handler", "tornado", "tornado/web.py", "def find_handler(",
             "In this web framework, which method of the `Application` class selects the message "
             "delegate that will handle an incoming request?"),
        _nav("nav-tornado-static-get", "tornado", "tornado/web.py",
             "async def get(self, path: str, include_body: bool = True) -> None:",
             "In this web framework, where is the `get` method of the static-file handler declared "
             "(the handler that serves files off disk)?"),
        _nav("nav-tornado-set-signed-cookie", "tornado", "tornado/web.py", "def set_signed_cookie(",
             "In this web framework, where is the request handler's method that sets a "
             "cryptographically signed cookie declared?"),
        _nav("nav-click-command", "click", "src/click/core.py", "class Command(BaseCommand):",
             "In this command-line library, where is the `Command` class declared?"),
        _nav("nav-click-option", "click", "src/click/core.py", "class Option(Parameter):",
             "In this command-line library, where is the `Option` class declared?"),
        _nav("nav-click-resolve-command", "click", "src/click/core.py", "def resolve_command(",
             "In this command-line library, where is the method that resolves a command-line token "
             "into a subcommand declared?"),
    ]


def trace_tasks() -> list[Task]:
    return [
        _trace("trace-tornado-write-headers", "tornado", "tornado/http1connection.py", "def write_headers(",
               "In this web framework, when a request handler finishes producing a response, the "
               "framework flushes it to the client. Following that path, which method on the "
               "underlying HTTP/1.x connection actually writes the response status line and headers "
               "out to the socket?",
               ["tornado/web.py", "tornado/http1connection.py"]),
        _trace("trace-tornado-ws-handshake", "tornado", "tornado/websocket.py", "async def _accept_connection(",
               "In this web framework, when an HTTP request is routed to a WebSocket handler and its "
               "`get` method runs, which method negotiates the subprotocol and extensions and writes "
               "the 101 Switching Protocols response?",
               ["tornado/web.py", "tornado/websocket.py"]),
        _trace("trace-tornado-parse-body", "tornado", "tornado/httputil.py", "def parse_body_arguments(",
               "In this web framework, a handler reads a POSTed form field through its body-argument "
               "accessor. Tracing back where those body arguments come from, which function actually "
               "parses the raw request body bytes into that argument mapping?",
               ["tornado/web.py", "tornado/httputil.py"]),
        _trace("trace-tornado-target-delegate", "tornado", "tornado/routing.py", "def get_target_delegate(",
               "In this web framework, when the application routes an incoming request and one of its "
               "URL rules matches, which method builds the message delegate that will actually run "
               "the matched target?",
               ["tornado/web.py", "tornado/routing.py"]),
    ]


def large_context_tasks() -> list[Task]:
    def method_prompt(file: str, method: str) -> str:
        return (
            f"In `{file}`, enumerate every public class defined at module scope (public = the name does "
            f"not start with an underscore; top-level only, not nested) whose own body defines a method "
            f"named `{method}`. A class counts only if it declares `{method}` directly in its body — not "
            f"if it merely inherits it from a base class. List each qualifying class name exactly once."
        )

    return [
        _lc("lc-click-get-command", "click", method_prompt("src/click/core.py", "get_command"),
            {"kind": "py_method", "file": "src/click/core.py", "target": "get_command"},
            ["src/click/core.py"]),
        _lc("lc-click-to-info-dict", "click", method_prompt("src/click/core.py", "to_info_dict"),
            {"kind": "py_method", "file": "src/click/core.py", "target": "to_info_dict"},
            ["src/click/core.py"]),
        _lc("lc-click-invoke", "click", method_prompt("src/click/core.py", "invoke"),
            {"kind": "py_method", "file": "src/click/core.py", "target": "invoke"},
            ["src/click/core.py"]),
        _lc("lc-tornado-prepare", "tornado", method_prompt("tornado/web.py", "prepare"),
            {"kind": "py_method", "file": "tornado/web.py", "target": "prepare"},
            ["tornado/web.py"]),
        _lc("lc-tornado-initialize", "tornado", method_prompt("tornado/web.py", "initialize"),
            {"kind": "py_method", "file": "tornado/web.py", "target": "initialize"},
            ["tornado/web.py"]),
        _lc("lc-tornado-handler-subclasses", "tornado",
            "Across `tornado/web.py` and `tornado/websocket.py`, enumerate every public class defined "
            "at module scope whose declared base classes include `RequestHandler` (written either as "
            "`RequestHandler` or as a dotted path ending in `.RequestHandler`). List each qualifying "
            "class name exactly once.",
            {"kind": "py_subclass", "base": "RequestHandler", "files": ["tornado/web.py", "tornado/websocket.py"]},
            ["tornado/web.py", "tornado/websocket.py"]),
    ]


def diff_review_tasks() -> list[Task]:
    return [
        _diff("diff-tornado-response", "tornado", DIFF_PROMPT, [
            {"file": "tornado/web.py",
             "find": "        self._reason = httputil.responses[200]",
             "replace": '        self._reason = httputil.responses.get(200, "OK")'},
            {"file": "tornado/web.py",
             "find": "            self._reason = escape.native_str(reason)",
             "replace": "            self._reason = escape.native_str(reason).strip()"},
            {"file": "tornado/web.py",
             "find": "        return self._status_code",
             "replace": "        return int(self._status_code)"},
        ]),
        _diff("diff-click-help", "click", DIFF_PROMPT, [
            {"file": "src/click/core.py",
             "find": "            text = make_default_short_help(self.help, limit)",
             "replace": "            text = make_default_short_help(self.help, limit).rstrip()"},
            {"file": "src/click/core.py",
             "find": "        rv = [self.options_metavar] if self.options_metavar else []",
             "replace": "        rv = list([self.options_metavar]) if self.options_metavar else []"},
            {"file": "src/click/core.py",
             "find": "        all_names = set(ctx.help_option_names)",
             "replace": "        all_names = set(ctx.help_option_names or [])"},
        ]),
        _diff("diff-tornado-routing", "tornado", DIFF_PROMPT, [
            {"file": "tornado/web.py",
             "find": "        self._headers.add(name, self._convert_header_value(value))",
             "replace": "        self._headers.add(str(name), self._convert_header_value(value))"},
            {"file": "tornado/web.py",
             "find": "            del self._headers[name]",
             "replace": "            self._headers.pop(name, None)"},
            {"file": "tornado/routing.py",
             "find": "            target_params = rule.matcher.match(request)",
             "replace": "            target_params = rule.matcher.match(request) or None"},
        ]),
    ]


def targeted_edit_tasks() -> list[Task]:
    return [
        _edit(
            "edit-tornado-httperror-default", "tornado",
            "When an HTTP error is raised in this web framework without an explicit status code, it "
            "currently defaults to HTTP 500. Change that default so it is HTTP 502 instead. Leave the "
            "behavior when an explicit status code is passed unchanged.",
            'PYTHONPATH=. python3 -c "import tornado.web as w; '
            "assert w.HTTPError().status_code == 502; assert w.HTTPError(404).status_code == 404\"",
            "HTTPError().status_code == 502",
            [{"file": "tornado/web.py",
              "find": "        status_code: int = 500,\n        log_message: Optional[str] = None,",
              "replace": "        status_code: int = 502,\n        log_message: Optional[str] = None,"}],
            ["tornado/web.py"],
        ),
        _edit(
            "edit-tornado-supported-methods", "tornado",
            "Make the base request handler in this web framework advertise support for the HTTP "
            "`TRACE` method in addition to every method it already lists as supported.",
            'PYTHONPATH=. python3 -c "import tornado.web as w; '
            "m = w.RequestHandler.SUPPORTED_METHODS; assert 'TRACE' in m; assert 'GET' in m and 'POST' in m\"",
            "'TRACE' in RequestHandler.SUPPORTED_METHODS",
            [{"file": "tornado/web.py",
              "find": '    SUPPORTED_METHODS = ("GET", "HEAD", "POST", "DELETE", "PATCH", "PUT", "OPTIONS")',
              "replace": '    SUPPORTED_METHODS = ("GET", "HEAD", "POST", "DELETE", "PATCH", "PUT", "OPTIONS", "TRACE")'}],
            ["tornado/web.py"],
        ),
        _edit(
            "edit-click-batch-partial", "click",
            "In this command-line library, the `batch` helper groups an iterable into fixed-size "
            "tuples but silently drops any leftover items that do not fill a complete final group. "
            "Change it so a final, smaller group containing the leftover items is included in the "
            "result. Keep the behavior for evenly-divisible inputs unchanged.",
            'PYTHONPATH=src python3 -c "from click.core import batch as b; '
            "assert b([1,2,3,4,5],2) == [(1,2),(3,4),(5,)], b([1,2,3,4,5],2); assert b([1,2,3,4],2) == [(1,2),(3,4)]\"",
            "batch keeps the trailing partial group",
            [{"file": "src/click/core.py",
              "find": "    return list(zip(*repeat(iter(iterable), batch_size)))",
              "replace": "    items = list(iterable)\n"
                         "    return [tuple(items[i : i + batch_size]) for i in range(0, len(items), batch_size)]"}],
            ["src/click/core.py"],
        ),
        _edit(
            "edit-click-short-help-limit", "click",
            "In this command-line library, a command's short help string is shortened to a default "
            "maximum length when it has to be derived from the long help text. Raise that default "
            "maximum length from 45 to 60 characters. Leave the behavior when a caller passes an "
            "explicit limit unchanged.",
            'PYTHONPATH=src python3 -c "import click; import inspect; '
            "src = inspect.getsource(click.Command.get_short_help_str); "
            "assert 'limit: int = 60' in src, src\"",
            "get_short_help_str default limit is 60",
            [{"file": "src/click/core.py",
              "find": "    def get_short_help_str(self, limit: int = 45) -> str:",
              "replace": "    def get_short_help_str(self, limit: int = 60) -> str:"}],
            ["src/click/core.py"],
        ),
    ]


def intent_search_tasks() -> list[Task]:
    return [
        _intent("intent-tornado-eventloop", "tornado",
                "In this web framework, which module owns the machinery that registers callbacks, "
                "timers, and file-descriptor readiness for asynchronous work?",
                "tornado/ioloop.py",
                ["tornado/ioloop.py", "tornado/iostream.py", "tornado/gen.py", "tornado/concurrent.py"]),
        _intent("intent-tornado-http1", "tornado",
                "In this web framework, which module owns the low-level protocol adapter that consumes "
                "request bytes from sockets and emits responses for the server stack?",
                "tornado/http1connection.py",
                ["tornado/http1connection.py", "tornado/httpserver.py", "tornado/httputil.py", "tornado/tcpserver.py"]),
        _intent("intent-click-parser", "click",
                "In this command-line library, when command invocation turns raw argv tokens into "
                "option and argument values, which source file owns that conversion machinery?",
                "src/click/parser.py",
                ["src/click/core.py", "src/click/parser.py"]),
    ]


def non_regression_tasks() -> list[Task]:
    """Control family (`ccx_helps=False`): repo-free reasoning where ccx cannot help.

    Each task uses synonym groups: a correct answer must hit >=1 term in EVERY group, so the
    grader rejects off-topic answers yet accepts paraphrases. Excluded from every headline; the
    empty workdir carries no repo and no traversal floor.
    """
    specs = [
        ("nonreg-binsearch", "Explain in two sentences how binary search works on a sorted array.",
         [["sort"], ["half", "halve", "middle", "mid", "divide"]]),
        ("nonreg-hashmap", "In two sentences, what is a hash map and what is its average lookup time?",
         [["hash", "dictionary", "key"], ["o(1)", "constant", "amortized", "average"]]),
        ("nonreg-recursion", "Define recursion in one sentence and give the term for its stopping condition.",
         [["itself", "self"], ["base case", "base-case", "stopping", "terminat"]]),
        ("nonreg-bigo", "In one sentence, what does Big-O notation describe about an algorithm?",
         [["time", "operation", "step", "memory", "space", "resource"], ["grow", "scale", "input", "size"]]),
    ]
    return [
        Task(
            id=tid,
            category="non_regression",
            repo="empty",
            prompt=prompt,
            schema=ANSWER_SCHEMA,
            grader=Grader("keywords", {"field": "answer"}),
            gold={"groups": groups},
            ccx_helps=False,
        )
        for tid, prompt, groups in specs
    ]


def headline_tasks() -> list[Task]:
    """Every floor-clearing headline task, in a stable family order."""
    return (
        navigation_tasks()
        + trace_tasks()
        + large_context_tasks()
        + diff_review_tasks()
        + targeted_edit_tasks()
        + intent_search_tasks()
    )


def all_tasks() -> list[Task]:
    return headline_tasks() + non_regression_tasks()
