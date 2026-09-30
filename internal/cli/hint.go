package cli

import (
	"regexp"
	"strings"
)

var expansionRe = regexp.MustCompile(`^~/|\$[A-Za-z_{]`)

// ExpansionHint returns a diagnosis line when one of args carries unexpanded
// shell syntax and err's message names it, so a backend "not found" on '~/x'
// or '$d/x' explains itself while output that merely mentions a ~/ path does
// not.
func ExpansionHint(err error, args []string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, arg := range args {
		if _, value, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(arg, "-") {
			arg = value
		}
		if expansionRe.MatchString(arg) && strings.Contains(msg, arg) {
			return "hint: the path carries unexpanded shell syntax — '~'/'$' inside single quotes never expands; retry with the expanded absolute path"
		}
	}
	return ""
}
