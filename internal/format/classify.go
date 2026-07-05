package format

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/toon-format/toon-go"
)

// Classifier thresholds. Each is annotated with its provenance: "measured"
// cites the literature claim it is grounded in; "heuristic" flags a value we
// invented and expect to tune.
const (
	smallPayloadBytes  = 200  // measured: minification alone recovers 37–46% vs pretty JSON; format deltas below benchmark noise under this size
	proseShare         = 0.66 // heuristic: our invention — one prose field holding ≥2/3 of payload bytes marks the payload prose-dominant
	proseMinBytes      = 512  // heuristic: a "prose" field shorter than this is not worth unwrapping
	proseCellChars     = 80   // heuristic: average cell length past which a column reads as prose, not tabular data
	tableTokenPressure = 2000 // measured: markdown tables beat CSV on accuracy (+7.6pp, non-overlapping CIs) at a ~25–29% token premium that is cheap under this size (len/4 estimator)
	toonMinRows        = 100  // measured: the only published row floor for TOON wins; independent evals report TOON underperforming with Claude models on smaller tables
	tronMinRepeat      = 3    // measured: TRON saves 0–27% when shapes repeat and inflates +21% when they do not (arXiv 2605.29676)
	uniformShare       = 0.9  // heuristic: modal key-set share at or above which an array of objects counts as uniform
	heteroShare        = 0.5  // heuristic: modal key-set share below which (≥3 rows) an array counts as heterogeneous
	nestedDepthMin     = 2    // heuristic: repeated shapes must sit at depth ≥2 for TRON's class-table overhead to amortize
)

// analysis holds the shape statistics classify branches on.
type analysis struct {
	compactBytes int // len(compactJSON(v))
	estTokens    int // compactBytes/4 — the render.Cap charsPerToken estimator

	singleString bool // root is one JSON string

	// Dominant prose field: the largest multi-word string field on a root
	// object (raw string bytes, not their JSON-escaped length).
	proseField      string
	proseFieldBytes int
	proseFieldShare float64 // proseFieldBytes / compactBytes

	// Root-array stats. rows counts every element; the modal share is over
	// element fingerprints — objects by key-set, non-objects by JSON kind —
	// so a scalar stream reads uniform, not heterogeneous.
	rows        int
	modalShare  float64
	allObjects  bool
	allScalar   bool // every cell of every object row is an IR scalar
	proseColumn bool // some column averages > proseCellChars chars or holds an embedded newline
	hasNulls    bool // some object cell is null
	uniform     bool // allObjects && rows ≥ 2 && modalShare ≥ uniformShare && allScalar
	hetero      bool // rows ≥ 3 && modalShare < heteroShare

	maxDepth  int // deepest container nesting; the root container sits at depth 0
	maxRepeat int // occurrences of the most-repeated ≥2-prop key-set fingerprint at depth ≥ nestedDepthMin
}

// analyze computes the shape statistics for an IR value.
func analyze(v any) analysis {
	var a analysis
	a.compactBytes = len(compactJSON(v))
	a.estTokens = a.compactBytes / 4

	switch t := v.(type) {
	case string:
		a.singleString = true
	case toon.Object:
		a.proseField, a.proseFieldBytes = classifyProseField(t)
		if a.proseFieldBytes > 0 {
			a.proseFieldShare = float64(a.proseFieldBytes) / float64(a.compactBytes)
		}
	case []any:
		classifyArrayStats(&a, t)
	}

	counts := make(map[string]int)
	classifyWalk(v, 0, &a.maxDepth, counts)
	for _, n := range counts {
		a.maxRepeat = max(a.maxRepeat, n)
	}
	return a
}

// classify returns candidate formats for v in priority order — first match
// wins — plus the analysis it branched on. The FormatAuto arm encodes the
// candidates in order and picks the smallest that passes the byte-net
// invariant len(out) <= len(compactJSON(v)); compact JSON is always the
// implicit last contender. That byte-net is the chart's step-7 "avoid full
// output compression unless benchmarked" guard — a per-payload eval standing
// in for accuracy benchmarks we can't run inline.
//
// Branches against the user's 8-step format chart:
//
//  1. Size floor (pre-chart): compact JSON under smallPayloadBytes → JSON.
//     Format deltas are below benchmark noise at this size; minification
//     alone is the cheapest win in the whole chart.
//  2. Prose-dominant (chart step 2): the payload is a single JSON string, or
//     one prose-like field of ≥ proseMinBytes holds ≥ proseShare of payload
//     bytes → prose unwrap.
//  3. Uniform array of scalar-celled objects (chart steps 3+4): a prose
//     column → JSONL then markdown (CSV/TOON degrade on prose cells);
//     estimated tokens under tableTokenPressure → markdown (accuracy beats
//     the ~25–29% token premium at this size); null cells under token
//     pressure → TOON then markdown (TOON's ~ handles nulls, CSV cannot
//     distinguish null from empty string); otherwise the CSV/TSV shootout,
//     with TOON entering only at ≥ toonMinRows.
//  4. Repeated nested shapes (chart step 5): some ≥2-prop key-set fingerprint
//     repeating ≥ tronMinRepeat times at depth ≥ nestedDepthMin → TRON; the
//     repeat gate plus the byte-net covers TRON's +21% inflation failure mode
//     on non-repeating shapes.
//  5. Heterogeneous array (≥3 rows, modal fingerprint share < heteroShare) or
//     a folded NDJSON stream of mixed shapes → JSONL (self-delimiting,
//     per-line schema; honestly unbenchmarked).
//  6. Everything else → minified JSON. YAML rejected on measurement: +21–27%
//     tokens vs minified JSON with only model-conditional accuracy wins.
//
// Chart steps deliberately N/A: step 1 (machine-validated generation) — read-
// side only, we format tool output for the model to read, not model output
// for machines to validate; step 3's HTML sub-branch — JSON-derived tables
// cannot have merged cells/hierarchy, and HTML costs ~3× tokens; step 8
// (file-native grep context) — we format transient tool output, not files.
// A future refinement out of v1 scope: the HYVE-style hybrid split (majority
// shape → table, stragglers → JSONL) when one shape covers ~75% of a mixed
// array.
func classify(v any) ([]Format, analysis) {
	a := analyze(v)
	switch {
	case a.compactBytes < smallPayloadBytes:
		return []Format{FormatJSON}, a
	case a.singleString || (a.proseFieldBytes >= proseMinBytes && a.proseFieldShare >= proseShare):
		return []Format{FormatProse}, a
	case a.uniform:
		switch {
		case a.proseColumn:
			return []Format{FormatJSONL, FormatMarkdown}, a
		case a.estTokens < tableTokenPressure:
			return []Format{FormatMarkdown}, a
		case a.hasNulls:
			return []Format{FormatTOON, FormatMarkdown}, a
		default:
			candidates := []Format{FormatCSV, FormatTSV}
			if a.rows >= toonMinRows {
				candidates = append(candidates, FormatTOON)
			}
			return candidates, a
		}
	case a.maxRepeat >= tronMinRepeat:
		return []Format{FormatTRON}, a
	case a.hetero:
		return []Format{FormatJSONL}, a
	default:
		return []Format{FormatJSON}, a
	}
}

// classifyProseField finds the largest string field on obj that reads as
// prose. Prose-like matches proseDominantIndex's test exactly — at least two
// whitespace-separated words — so the classifier never nominates a field the
// prose encoder rejects (a single token with trailing whitespace is not
// prose).
func classifyProseField(obj toon.Object) (key string, size int) {
	for _, f := range obj.Fields {
		s, ok := f.Value.(string)
		if !ok || len(s) <= size || len(strings.Fields(s)) < 2 {
			continue
		}
		key, size = f.Key, len(s)
	}
	return key, size
}

func classifyArrayStats(a *analysis, arr []any) {
	a.rows = len(arr)
	if a.rows == 0 {
		return
	}
	a.allObjects, a.allScalar = true, true

	counts := make(map[string]int)
	colChars := make(map[string]int)
	colCells := make(map[string]int)
	for _, e := range arr {
		obj, ok := e.(toon.Object)
		if !ok {
			a.allObjects = false
			counts[classifyKindFingerprint(e)]++
			continue
		}
		counts[classifyFingerprint(obj)]++
		for _, f := range obj.Fields {
			switch cell := f.Value.(type) {
			case toon.Object, []any:
				a.allScalar = false
			case nil:
				a.hasNulls = true
			case string:
				colChars[f.Key] += len(cell)
				colCells[f.Key]++
				if strings.ContainsRune(cell, '\n') {
					a.proseColumn = true
				}
			}
		}
	}

	modal := 0
	for _, n := range counts {
		modal = max(modal, n)
	}
	a.modalShare = float64(modal) / float64(a.rows)
	for key, cells := range colCells {
		if float64(colChars[key])/float64(cells) > proseCellChars {
			a.proseColumn = true
		}
	}
	a.uniform = a.allObjects && a.rows >= 2 && a.modalShare >= uniformShare && a.allScalar
	a.hetero = a.rows >= 3 && a.modalShare < heteroShare
}

// classifyWalk records the deepest container and counts ≥2-prop key-set
// fingerprints occurring at depth ≥ nestedDepthMin (root container = depth 0).
func classifyWalk(v any, depth int, maxDepth *int, counts map[string]int) {
	switch t := v.(type) {
	case toon.Object:
		*maxDepth = max(*maxDepth, depth)
		if depth >= nestedDepthMin && len(t.Fields) >= 2 {
			counts[classifyFingerprint(t)]++
		}
		for _, f := range t.Fields {
			classifyWalk(f.Value, depth+1, maxDepth, counts)
		}
	case []any:
		*maxDepth = max(*maxDepth, depth)
		for _, e := range t {
			classifyWalk(e, depth+1, maxDepth, counts)
		}
	}
}

// classifyFingerprint is the order-insensitive key-set fingerprint of obj:
// sorted keys emitted as self-delimiting len:key blocks, so no key content —
// commas, NULs, any separator — can forge a block boundary and collide two
// distinct key-sets.
func classifyFingerprint(obj toon.Object) string {
	keys := make([]string, len(obj.Fields))
	for i, f := range obj.Fields {
		keys[i] = f.Key
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteByte(':')
		b.WriteString(k)
	}
	return b.String()
}

// classifyKindFingerprint tags a non-object array element by its JSON kind so
// mixed-type streams read heterogeneous while scalar streams read uniform.
func classifyKindFingerprint(e any) string {
	switch e.(type) {
	case []any:
		return "\x00kind:array"
	case string:
		return "\x00kind:string"
	case bool:
		return "\x00kind:bool"
	case nil:
		return "\x00kind:null"
	case int64, *big.Int, json.Number:
		return "\x00kind:number"
	default:
		panic(fmt.Sprintf("classifyKindFingerprint: unexpected type %T", e))
	}
}
