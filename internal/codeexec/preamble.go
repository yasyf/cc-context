package codeexec

import (
	"sort"
	"strings"
)

// ToolSig describes one host function for the discovery preamble.
type ToolSig struct {
	Name      string
	Signature string
	Summary   string
}

var staticTools = []ToolSig{
	{"search", `search(query, repo=None, mode=None, lang=None, k=0)`, "semantic/structural/literal code search; prefer over grep for where/how questions"},
	{"read", `read(path, section=None, full=False)`, "read a file by section/heading or whole"},
	{"grep", `grep(text, glob=None, scope=None, paths=None, regex=False, ignore_case=False, word=False, expand=0)`, "literal or regex text search across code and files"},
	{"outline", `outline(path, items=None, match=None, lang=None)`, "signatures+line numbers for a file or dir"},
	{"symbol", `symbol(name, scope=None, full=False)`, "definition, callers, callees, siblings, tests of a symbol"},
	{"find", `find(glob, scope=None)`, "list files matching a glob with token counts"},
	{"related", `related(location)`, "code semantically related to a file:line"},
	{"deps", `deps(path, scope=None)`, "imports of a file and their resolved targets"},
	{"diff", `diff(source="uncommitted", scope=None)`, "VCS-aware diff (uncommitted|staged|<ref>)"},
	{"overview", `overview()`, "repository structure and entry points"},
	{"web_outline", `web_outline(url)`, "heading tree of a web page with stable section refs"},
	{"web_read", `web_read(url, section=None, full=False)`, "read a web page by section ref or whole"},
	{"web_search", `web_search(url, query, k=0)`, "top-k chunks of a page relevant to a question, with cites"},
	{"sh", `sh(cmd)`, "run a shell command, returns combined output (host-trust, not sandboxed)"},
}

const preambleText = `Write a short Python script that composes the host functions below and returns
ONLY the distilled result — intermediate output stays in the sandbox and never
enters context. The script's final expression is the result (dicts/lists come
back as TOON or JSON); use print() for extra diagnostics.

Every host function is async — await it, and run independent calls concurrently
with asyncio.gather(...). Allowed Python: async/await and dict/list/str plus the
modules re, json, datetime, asyncio. NOT allowed: classes, match, or importing
anything else. Keep it procedural. Import each module on its own line (no
"import a, b"). Top-level return is illegal — wrap logic in async def main() and
end the script with asyncio.run(main()).

Example:
  import asyncio
  async def main():
      raw = await grep("RunDiffCLI", glob="*.go")
      return [ln for ln in raw.splitlines() if "func " in ln]
  asyncio.run(main())

Host functions:`

// Preamble renders the sandbox's Python-subset rules and the available
// host-function signatures for the discovery tool. extra holds the per-session
// reflected functions, appended after the static set.
func Preamble(extra []ToolSig) string {
	var b strings.Builder
	b.WriteString(preambleText)
	b.WriteString("\n")
	for _, t := range staticTools {
		writeSig(&b, t)
	}
	if len(extra) > 0 {
		sorted := append([]ToolSig(nil), extra...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		b.WriteString("\nReflected MCP tools (this session):\n")
		for _, t := range sorted {
			writeSig(&b, t)
		}
	}
	return b.String()
}

func writeSig(b *strings.Builder, t ToolSig) {
	b.WriteString("  ")
	b.WriteString(t.Signature)
	b.WriteString("\n      ")
	b.WriteString(t.Summary)
	b.WriteString("\n")
}
