package rank

import (
	"regexp"
	"strings"
	"sync"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// Constants and regexes ported verbatim from semble/ranking/boosting.py and
// semble/ranking/weighting.py (semble 0.5.2).

const (
	alphaSymbol = 0.3 // weighting.py _ALPHA_SYMBOL: lean BM25 for exact keyword matching
	alphaNL     = 0.5 // weighting.py _ALPHA_NL: balanced semantic + BM25

	embeddedStemMinLen     = 4    // _EMBEDDED_STEM_MIN_LEN
	embeddedBoostScale     = 0.5  // _EMBEDDED_SYMBOL_BOOST_SCALE
	defBoostMultiplier     = 3.0  // _DEFINITION_BOOST_MULTIPLIER
	stemBoostMultiplier    = 1.0  // _STEM_BOOST_MULTIPLIER
	fileCoherenceBoostFrac = 0.2  // _FILE_COHERENCE_BOOST_FRAC
	stemMatchRatioMin      = 0.10 // _boost_stem_matches match-ratio threshold
)

// symbolQueryRe (boosting.py _SYMBOL_QUERY_RE): namespace-qualified,
// leading-underscore, contains-uppercase/underscore, or starts-uppercase. The
// "\\" alternative matches a literal backslash separator.
var symbolQueryRe = regexp.MustCompile(
	`^(?:` +
		`[A-Za-z_][A-Za-z0-9_]*(?:(?:::|\\|->|\.)[A-Za-z_][A-Za-z0-9_]*)+` +
		`|_[A-Za-z0-9_]*` +
		`|[A-Za-z][A-Za-z0-9]*[A-Z_][A-Za-z0-9_]*` +
		`|[A-Z][A-Za-z0-9]*` +
		`)$`)

// embeddedSymbolRe (boosting.py _EMBEDDED_SYMBOL_RE): PascalCase/camelCase
// identifiers embedded in a NL query; excludes plain words and pure acronyms.
var embeddedSymbolRe = regexp.MustCompile(
	`\b(?:[A-Z][a-z][a-zA-Z0-9]*[A-Z][a-zA-Z0-9]*|[a-z][a-zA-Z0-9]*[A-Z][a-zA-Z0-9]+)\b`)

// defKeywords (boosting.py _DEFINITION_KEYWORDS): case-sensitive definition
// keywords across languages.
var defKeywords = []string{
	"class", "module", "defmodule", "def", "interface", "struct", "enum",
	"trait", "type", "func", "function", "object", "abstract class",
	"data class", "fn", "fun", "package", "namespace", "protocol", "record",
	"typedef",
}

// sqlKeywords (boosting.py _SQL_DEFINITION_KEYWORDS): matched case-insensitively.
var sqlKeywords = []string{"CREATE TABLE", "CREATE VIEW", "CREATE PROCEDURE", "CREATE FUNCTION"}

// stopwords (boosting.py _STOPWORDS): excluded from NL file-stem matching.
var stopwords = func() map[string]bool {
	words := strings.Fields(
		"a an and are as at be by do does for from has have how if in is it not of on or the to was" +
			" what when where which who why with")
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

// IsSymbolQuery reports whether the query looks like a bare symbol or
// namespace-qualified identifier. Mirrors boosting.py is_symbol_query.
func IsSymbolQuery(query string) bool {
	return symbolQueryRe.MatchString(strings.TrimSpace(query))
}

// ResolveAlpha returns the semantic-blend weight, auto-detecting from query type
// when alpha is nil. Mirrors weighting.py resolve_alpha.
func ResolveAlpha(query string, alpha *float64) float64 {
	if alpha != nil {
		return *alpha
	}
	if IsSymbolQuery(query) {
		return alphaSymbol
	}
	return alphaNL
}

// applyQueryBoost applies query-type boosts to candidate scores, returning a new
// slice (symbol and embedded-symbol scans may append non-candidate chunks, kept
// after the candidates in corpus order, mirroring dict insertion). Mirrors
// boosting.py apply_query_boost.
func applyQueryBoost(cands []scored, query string, chunks []semsearch.Chunk) []scored {
	if len(cands) == 0 {
		return cands
	}
	maxScore := maxScoreOf(cands)
	out := make([]scored, len(cands))
	copy(out, cands)

	if IsSymbolQuery(query) {
		out = boostSymbolDefinitions(out, query, maxScore, chunks)
	} else {
		boostStemMatches(out, query, maxScore, chunks)
		out = boostEmbeddedSymbols(out, query, maxScore, chunks)
	}
	return out
}

// boostMultiChunkFiles promotes files with multiple high-scoring chunks by
// boosting each file's top chunk in place. Mirrors boosting.py
// boost_multi_chunk_files.
func boostMultiChunkFiles(cands []scored, chunks []semsearch.Chunk) {
	if len(cands) == 0 {
		return
	}
	maxScore := maxScoreOf(cands)
	if maxScore == 0.0 {
		return
	}
	fileSum := map[string]float64{}
	bestPos := map[string]int{}
	for i := range cands {
		fp := chunks[cands[i].idx].Path
		fileSum[fp] += cands[i].score
		if pos, ok := bestPos[fp]; !ok || cands[i].score > cands[pos].score {
			bestPos[fp] = i
		}
	}
	var maxFileSum float64
	first := true
	for _, s := range fileSum {
		if first || s > maxFileSum {
			maxFileSum, first = s, false
		}
	}
	boostUnit := maxScore * fileCoherenceBoostFrac
	for fp, pos := range bestPos {
		cands[pos].score += boostUnit * fileSum[fp] / maxFileSum
	}
}

func boostSymbolDefinitions(out []scored, query string, maxScore float64, chunks []semsearch.Chunk) []scored {
	symbolName := extractSymbolName(query)
	names := []string{symbolName}
	if trimmed := strings.TrimSpace(query); symbolName != trimmed {
		names = append(names, trimmed)
	}
	boostUnit := maxScore * defBoostMultiplier

	for i := range out {
		if tier := definitionTier(chunks[out[i].idx], names, boostUnit); tier != 0 {
			out[i].score += tier
		}
	}

	inSet := idxSetOf(out)
	symLower := strings.ToLower(symbolName)
	for idx := range chunks {
		if inSet[idx] {
			continue
		}
		if !stemMatches(strings.ToLower(pathStem(chunks[idx].Path)), symLower) {
			continue
		}
		if tier := definitionTier(chunks[idx], names, boostUnit); tier != 0 {
			out = append(out, scored{idx: idx, score: tier})
			inSet[idx] = true
		}
	}
	return out
}

func boostEmbeddedSymbols(out []scored, query string, maxScore float64, chunks []semsearch.Chunk) []scored {
	names := uniqueStrings(embeddedSymbolRe.FindAllString(query, -1))
	if len(names) == 0 {
		return out
	}
	boostUnit := maxScore * defBoostMultiplier * embeddedBoostScale

	for i := range out {
		if tier := definitionTier(chunks[out[i].idx], names, boostUnit); tier != 0 {
			out[i].score += tier
		}
	}

	symbolsLower := make([]string, len(names))
	for i, n := range names {
		symbolsLower[i] = strings.ToLower(n)
	}
	inSet := idxSetOf(out)
	for idx := range chunks {
		if inSet[idx] {
			continue
		}
		stem := strings.ToLower(pathStem(chunks[idx].Path))
		stemNorm := strings.ReplaceAll(stem, "_", "")
		matched := false
		for _, sl := range symbolsLower {
			if stem == sl || stemNorm == sl ||
				(len(stem) >= embeddedStemMinLen && strings.HasPrefix(sl, stem)) ||
				(len(stemNorm) >= embeddedStemMinLen && strings.HasPrefix(sl, stemNorm)) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if tier := definitionTier(chunks[idx], names, boostUnit); tier != 0 {
			out = append(out, scored{idx: idx, score: tier})
			inSet[idx] = true
		}
	}
	return out
}

func boostStemMatches(out []scored, query string, maxScore float64, chunks []semsearch.Chunk) {
	keywords := map[string]bool{}
	for _, w := range tokenRe.FindAllString(query, -1) {
		if len(w) > 2 {
			lw := strings.ToLower(w)
			if !stopwords[lw] {
				keywords[lw] = true
			}
		}
	}
	if len(keywords) == 0 {
		return
	}
	boost := maxScore * stemBoostMultiplier
	pathCache := map[string]map[string]bool{}
	for i := range out {
		fp := chunks[out[i].idx].Path
		parts := pathCache[fp]
		if parts == nil {
			parts = map[string]bool{}
			for _, p := range SplitIdentifier(pathStem(fp)) {
				parts[p] = true
			}
			if pn := pathParentName(fp); pn != "" && pn != "." && pn != "/" && pn != ".." {
				for _, p := range SplitIdentifier(pn) {
					parts[p] = true
				}
			}
			pathCache[fp] = parts
		}
		n := countKeywordMatches(keywords, parts)
		if n > 0 {
			ratio := float64(n) / float64(len(keywords))
			if ratio >= stemMatchRatioMin {
				out[i].score += boost * ratio
			}
		}
	}
}

func countKeywordMatches(keywords, parts map[string]bool) int {
	exact := 0
	for k := range keywords {
		if parts[k] {
			exact++
		}
	}
	if exact == len(keywords) {
		return exact
	}
	n := exact
	for k := range keywords {
		if parts[k] {
			continue
		}
		for p := range parts {
			shorter, longer := k, p
			if len(k) > len(p) {
				shorter, longer = p, k
			}
			if len(shorter) >= 3 && strings.HasPrefix(longer, shorter) {
				n++
				break
			}
		}
	}
	return n
}

// extractSymbolName returns the final identifier of a possibly
// namespace-qualified query. Mirrors boosting.py _extract_symbol_name.
func extractSymbolName(query string) string {
	for _, sep := range []string{"::", "\\", "->", "."} {
		if strings.Contains(query, sep) {
			parts := strings.Split(query, sep)
			return parts[len(parts)-1]
		}
	}
	return strings.TrimSpace(query)
}

// definitionTier returns the boost for a chunk defining one of names, applying
// the 1.5× file-stem tier. Mirrors boosting.py _definition_tier.
func definitionTier(chunk semsearch.Chunk, names []string, boostUnit float64) float64 {
	defines := false
	for _, n := range names {
		if chunkDefinesSymbol(chunk.Content, n) {
			defines = true
			break
		}
	}
	if !defines {
		return 0
	}
	stem := strings.ToLower(pathStem(chunk.Path))
	for _, n := range names {
		if stemMatches(stem, strings.ToLower(n)) {
			return boostUnit * 1.5
		}
	}
	return boostUnit * 1.0
}

// stemMatches reports whether stem matches name exactly, snake-normalised, or
// depluralised. Mirrors boosting.py _stem_matches.
func stemMatches(stem, name string) bool {
	stemNorm := strings.ReplaceAll(stem, "_", "")
	return stem == name || stemNorm == name ||
		strings.TrimRight(stem, "s") == name || strings.TrimRight(stemNorm, "s") == name
}

// chunkDefinesSymbol reports whether content defines symbolName, case-sensitive
// for general keywords and case-insensitive for SQL DDL. Mirrors boosting.py
// _chunk_defines_symbol.
func chunkDefinesSymbol(content, symbolName string) bool {
	general, sql := definitionPattern(symbolName)
	return general.MatchString(content) || sql.MatchString(content)
}

// nsPrefix (boosting.py _definition_pattern): optional namespace qualification.
const nsPrefix = `(?:[A-Za-z_][A-Za-z0-9_]*(?:\.|::))*`

// keywordPrefix ports boosting.py _KEYWORD_PREFIX `(?:^|(?<=\s))(?:`. RE2 has no
// lookbehind; the consuming `(?:^|\s)` form is existence-equivalent for the
// boolean definition search (one preceding whitespace is consumed but never
// needed elsewhere).
const keywordPrefix = `(?:^|\s)(?:`

var (
	defKeywordBody = joinEscaped(defKeywords)
	sqlKeywordBody = joinEscaped(sqlKeywords)

	definitionPatternMu    sync.Mutex
	definitionPatternCache = map[string][2]*regexp.Regexp{}
)

// definitionPattern builds (general, sql) definition regexes for a symbol,
// caching by name. Mirrors boosting.py _definition_pattern.
func definitionPattern(symbolName string) (*regexp.Regexp, *regexp.Regexp) {
	definitionPatternMu.Lock()
	defer definitionPatternMu.Unlock()
	if p, ok := definitionPatternCache[symbolName]; ok {
		return p[0], p[1]
	}
	suffix := `)\s+` + nsPrefix + regexp.QuoteMeta(symbolName) + `(?:\s|[<({:\[;]|$)`
	general := regexp.MustCompile(`(?m)` + keywordPrefix + defKeywordBody + suffix)
	sql := regexp.MustCompile(`(?im)` + keywordPrefix + sqlKeywordBody + suffix)
	definitionPatternCache[symbolName] = [2]*regexp.Regexp{general, sql}
	return general, sql
}

func joinEscaped(keywords []string) string {
	escaped := make([]string, len(keywords))
	for i, k := range keywords {
		escaped[i] = regexp.QuoteMeta(k)
	}
	return strings.Join(escaped, "|")
}

func maxScoreOf(cands []scored) float64 {
	max := cands[0].score
	for _, c := range cands[1:] {
		if c.score > max {
			max = c.score
		}
	}
	return max
}

func idxSetOf(cands []scored) map[int]bool {
	set := make(map[int]bool, len(cands))
	for _, c := range cands {
		set[c.idx] = true
	}
	return set
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
