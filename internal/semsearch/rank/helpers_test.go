package rank

import (
	"math"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// eps is the tolerance for float comparisons; expected values are semble 0.5.2
// oracle outputs (float64, identical operations) so agreement is exact to well
// within this bound.
const eps = 1e-9

func almostEqual(a, b float64) bool { return math.Abs(a-b) <= eps }

func mkChunk(path string, start int, content string) semsearch.Chunk {
	return semsearch.Chunk{Path: path, StartLine: start, EndLine: start + 1, Content: content}
}
