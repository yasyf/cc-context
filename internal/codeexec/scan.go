package codeexec

import (
	"maps"
	"regexp"
	"slices"
)

var (
	callSite   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*\(`)
	stringLit  = regexp.MustCompile(`(?s)'''.*?'''|""".*?"""|'(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*"`)
	hashToLine = regexp.MustCompile(`#[^\n]*`)
)

// referenced maps the script's function-call identifiers through funcToServer
// and returns the unique server names, sorted. String literals, comments, and
// method calls (a leading '.') don't count. It drives pre-warming only:
// registration never depends on it, so a miss just means a colder first call.
func referenced(script string, funcToServer map[string]string) []string {
	code := hashToLine.ReplaceAllString(stringLit.ReplaceAllString(script, " "), "")
	servers := map[string]bool{}
	for _, loc := range callSite.FindAllStringIndex(code, -1) {
		if loc[0] > 0 && code[loc[0]-1] == '.' {
			continue
		}
		if server, ok := funcToServer[code[loc[0]:loc[1]-1]]; ok {
			servers[server] = true
		}
	}
	return slices.Sorted(maps.Keys(servers))
}
