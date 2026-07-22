package rank

import "regexp"

// Ported verbatim from semble/tokens.py (semble 0.5.2).

// tokenRe matches identifier-like runs; the leading class forbids a digit start.
var tokenRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

// SplitIdentifier splits a single identifier into sub-tokens via
// camelCase/snake_case, returning the lowered whole token plus any sub-parts
// (only when there are at least two parts). Mirrors semble/tokens.py
// split_identifier: "HandlerStack" → ["handlerstack","handler","stack"],
// "my_func" → ["my_func","my","func"], "simple" → ["simple"].
func SplitIdentifier(token string) []string {
	lower := toLowerASCII(token)
	var parts []string
	if containsByte(token, '_') {
		// snake_case: split on underscore, dropping empties.
		for _, p := range splitByte(lower, '_') {
			if p != "" {
				parts = append(parts, p)
			}
		}
	} else {
		// camelCase / PascalCase, via the semble _CAMEL_RE find-all.
		for _, m := range camelFindAll(token) {
			parts = append(parts, toLowerASCII(m))
		}
	}
	if len(parts) >= 2 {
		return append([]string{lower}, parts...)
	}
	return []string{lower}
}

// Tokenize splits text into lowercase identifier tokens for BM25 indexing,
// expanding compound identifiers into sub-tokens. Mirrors semble/tokens.py
// tokenize.
func Tokenize(text string) []string {
	raw := tokenRe.FindAllString(text, -1)
	if len(raw) == 0 {
		return nil
	}
	result := make([]string, 0, len(raw))
	for _, tok := range raw {
		result = append(result, SplitIdentifier(tok)...)
	}
	return result
}

// camelFindAll reproduces re.findall(_CAMEL_RE, token) for
// _CAMEL_RE = [A-Z]+(?=[A-Z][a-z])|[A-Z]?[a-z]+|[A-Z]+|[0-9]+ (semble/tokens.py).
// RE2 has no lookahead, so the alternation is evaluated by hand with the same
// leftmost-first, non-overlapping semantics. Input is ASCII (the caller only
// passes matches of tokenRe with no underscore), so byte indexing is safe.
func camelFindAll(s string) []string {
	var out []string
	n := len(s)
	i := 0
	for i < n {
		c := s[i]
		switch {
		case isUpperASCII(c):
			// Maximal uppercase run [i, j).
			j := i
			for j < n && isUpperASCII(s[j]) {
				j++
			}
			// alt1: [A-Z]+(?=[A-Z][a-z]) — only satisfiable at run length-1,
			// i.e. run ≥2 uppercase followed by a lowercase.
			if j-i >= 2 && j < n && isLowerASCII(s[j]) {
				out = append(out, s[i:j-1])
				i = j - 1
				continue
			}
			// alt2: [A-Z]?[a-z]+ — the leading uppercase then a lowercase run.
			if i+1 < n && isLowerASCII(s[i+1]) {
				k := i + 1
				for k < n && isLowerASCII(s[k]) {
					k++
				}
				out = append(out, s[i:k])
				i = k
				continue
			}
			// alt3: [A-Z]+ — the whole run.
			out = append(out, s[i:j])
			i = j
		case isLowerASCII(c):
			// alt2 without the optional leading uppercase: [a-z]+.
			k := i
			for k < n && isLowerASCII(s[k]) {
				k++
			}
			out = append(out, s[i:k])
			i = k
		case isDigitASCII(c):
			// alt4: [0-9]+.
			k := i
			for k < n && isDigitASCII(s[k]) {
				k++
			}
			out = append(out, s[i:k])
			i = k
		default:
			i++
		}
	}
	return out
}

func isUpperASCII(b byte) bool { return b >= 'A' && b <= 'Z' }
func isLowerASCII(b byte) bool { return b >= 'a' && b <= 'z' }
func isDigitASCII(b byte) bool { return b >= '0' && b <= '9' }

func toLowerASCII(s string) string {
	var changed bool
	for i := 0; i < len(s); i++ {
		if isUpperASCII(s[i]) {
			changed = true
			break
		}
	}
	if !changed {
		return s
	}
	b := []byte(s)
	for i := range b {
		if isUpperASCII(b[i]) {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func containsByte(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

func splitByte(s string, c byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
