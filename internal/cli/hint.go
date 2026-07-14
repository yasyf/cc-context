package cli

import "regexp"

// expansionRe spots a path token the shell never expanded — a leading ~/ or a $
// opening a variable — inside an error message. A $ before a non-name character
// (a regex end-anchor, a digit) does not count.
var expansionRe = regexp.MustCompile(`(?:^|[\s:'"/])~/|\$[A-Za-z_{]`)

// ExpansionHint returns a diagnosis line when err's message carries unexpanded
// shell syntax, so a backend "not found" on '~/x' or '$d/x' explains itself.
func ExpansionHint(err error) string {
	if err != nil && expansionRe.MatchString(err.Error()) {
		return "hint: the path carries unexpanded shell syntax — '~'/'$' inside single quotes never expands; retry with the expanded absolute path"
	}
	return ""
}
