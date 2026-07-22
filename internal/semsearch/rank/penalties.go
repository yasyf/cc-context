package rank

import (
	"math"
	"regexp"
	"sort"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// Constants and regexes ported verbatim from semble/ranking/penalties.py
// (semble 0.5.2).

const (
	strongPenalty   = 0.3 // _STRONG_PENALTY: test files, compat shims, example/doc code
	moderatePenalty = 0.5 // _MODERATE_PENALTY: re-export / metadata files
	mildPenalty     = 0.7 // _MILD_PENALTY: .d.ts declaration stubs

	fileSaturationThreshold = 1   // _FILE_SATURATION_THRESHOLD
	fileSaturationDecay     = 0.5 // _FILE_SATURATION_DECAY
)

// testFileRe (penalties.py _TEST_FILE_RE): test-file names across languages.
var testFileRe = regexp.MustCompile(
	`(?:^|/)` +
		`(?:` +
		`test_[^/]*\.py` +
		`|[^/]*_test\.py` +
		`|[^/]*_test\.go` +
		`|[^/]*Tests?\.java` +
		`|[^/]*Test\.php` +
		`|[^/]*_spec\.rb` +
		`|[^/]*_test\.rb` +
		`|[^/]*\.test\.[jt]sx?` +
		`|[^/]*\.spec\.[jt]sx?` +
		`|[^/]*Tests?\.kt` +
		`|[^/]*Spec\.kt` +
		`|[^/]*Tests?\.swift` +
		`|[^/]*Spec\.swift` +
		`|[^/]*Tests?\.cs` +
		`|test_[^/]*\.cpp` +
		`|[^/]*_test\.cpp` +
		`|test_[^/]*\.c` +
		`|[^/]*_test\.c` +
		`|[^/]*Spec\.scala` +
		`|[^/]*Suite\.scala` +
		`|[^/]*Test\.scala` +
		`|[^/]*_test\.dart` +
		`|test_[^/]*\.dart` +
		`|[^/]*_spec\.lua` +
		`|[^/]*_test\.lua` +
		`|test_[^/]*\.lua` +
		`|test_helpers?[^/]*\.\w+` +
		`)$`)

// testDirRe (penalties.py _TEST_DIR_RE): test/spec directories.
var testDirRe = regexp.MustCompile(`(?:^|/)(?:tests?|__tests__|spec|testing)(?:/|$)`)

// compatDirRe (penalties.py _COMPAT_DIR_RE): compat/legacy path components.
var compatDirRe = regexp.MustCompile(`(?:^|/)(?:compat|_compat|legacy)(?:/|$)`)

// examplesDirRe (penalties.py _EXAMPLES_DIR_RE): examples/docs path components.
var examplesDirRe = regexp.MustCompile(`(?:^|/)(?:_?examples?|docs?_src)(?:/|$)`)

// typeDefsRe (penalties.py _TYPE_DEFS_RE): TypeScript .d.ts declaration files.
var typeDefsRe = regexp.MustCompile(`\.d\.ts$`)

// reexportFilenames (penalties.py _REEXPORT_FILENAMES): re-export barrels /
// package metadata.
var reexportFilenames = map[string]bool{"__init__.py": true, "package-info.java": true}

// rerankTopk selects the top-k results, applying file-path penalties (when
// penalisePaths) then greedy file-saturation decay, and returns them sorted by
// effective score descending. Mirrors penalties.py rerank_topk.
func rerankTopk(cands []scored, chunks []semsearch.Chunk, topK int, penalisePaths bool) []scored {
	if len(cands) == 0 {
		return nil
	}

	penaltyCache := map[string]float64{}
	penalised := make([]scored, len(cands))
	for i, c := range cands {
		p := c.score
		if penalisePaths {
			fp := chunks[c.idx].Path
			pen, ok := penaltyCache[fp]
			if !ok {
				pen = filePathPenalty(fp)
				penaltyCache[fp] = pen
			}
			p = c.score * pen
		}
		penalised[i] = scored{idx: c.idx, score: p}
	}

	// Stable sort by penalised score descending. cands (hence penalised) is
	// pre-ordered by (start_line, path), so ties keep that order — the substrate
	// standing in for numpy's tie behaviour.
	ranked := make([]scored, len(penalised))
	copy(ranked, penalised)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	fileSelected := map[string]int{}
	var selected []scored
	minSelected := math.Inf(1)
	for _, rc := range ranked {
		penScore := rc.score
		if len(selected) >= topK && penScore <= minSelected {
			break
		}
		fp := chunks[rc.idx].Path
		already := fileSelected[fp]
		eff := penScore
		if already >= fileSaturationThreshold {
			excess := already - fileSaturationThreshold + 1
			eff *= math.Pow(fileSaturationDecay, float64(excess))
		}
		selected = append(selected, scored{idx: rc.idx, score: eff})
		fileSelected[fp] = already + 1
		if len(selected) >= topK {
			minSelected = selected[0].score
			for _, s := range selected[1:] {
				if s.score < minSelected {
					minSelected = s.score
				}
			}
		}
	}

	sort.SliceStable(selected, func(i, j int) bool { return selected[i].score > selected[j].score })
	if len(selected) > topK {
		selected = selected[:topK]
	}
	return selected
}

// filePathPenalty returns the combined multiplicative penalty for a path,
// compounding across families. Mirrors penalties.py _file_path_penalty.
func filePathPenalty(filePath string) float64 {
	normalised := replaceBackslashes(filePath)
	penalty := 1.0
	if testFileRe.MatchString(normalised) || testDirRe.MatchString(normalised) {
		penalty *= strongPenalty
	}
	if reexportFilenames[pathBase(filePath)] {
		penalty *= moderatePenalty
	}
	if compatDirRe.MatchString(normalised) {
		penalty *= strongPenalty
	}
	if examplesDirRe.MatchString(normalised) {
		penalty *= strongPenalty
	}
	if typeDefsRe.MatchString(normalised) {
		penalty *= mildPenalty
	}
	return penalty
}

func replaceBackslashes(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] == '\\' {
			b[i] = '/'
		}
	}
	return string(b)
}
