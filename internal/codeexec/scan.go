package codeexec

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

var (
	callSite  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*\(`)
	bareIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

// referenced maps the script's function-call identifiers through funcToServer
// and returns the unique server names, sorted. Method calls (a leading '.')
// don't count; string literals and comments do (the scan runs on the raw
// script, fail-open). It drives pre-warming only: registration never depends on
// it, so a miss just means a colder first call and a spurious hit only warms an
// extra session.
func referenced(script string, funcToServer map[string]string) []string {
	servers := map[string]bool{}
	for _, loc := range callSite.FindAllStringIndex(script, -1) {
		if loc[0] > 0 && script[loc[0]-1] == '.' {
			continue
		}
		if server, ok := funcToServer[script[loc[0]:loc[1]-1]]; ok {
			servers[server] = true
		}
	}
	return slices.Sorted(maps.Keys(servers))
}

// referencesMCP reports whether script names any reflected host function by
// bare identifier — not just call sites, so an aliased call (f = fake_echo)
// still counts. Attribute access (a leading '.') doesn't. The scan is raw
// (fail-open): a literal/comment mention only over-reflects, whereas stripping
// risks a false negative — a paired quote across comments, an f-string
// interpolation — that drops a tool the script does call.
func referencesMCP(script string, servers []ServerSpec) bool {
	if len(servers) == 0 {
		return false
	}
	for _, loc := range bareIdent.FindAllStringIndex(script, -1) {
		if loc[0] > 0 && script[loc[0]-1] == '.' {
			continue
		}
		ident := script[loc[0]:loc[1]]
		for _, spec := range servers {
			if strings.HasPrefix(ident, spec.Prefix+"_") {
				return true
			}
		}
	}
	return false
}
