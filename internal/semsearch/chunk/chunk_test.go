package chunk

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// fakeParser returns a fixed tree, or ok=false to force the line fallback.
type fakeParser struct {
	root node
	ok   bool
}

func (f fakeParser) parse(string, []byte) (node, bool) { return f.root, f.ok }

func TestChunkSourceASTPath(t *testing.T) {
	// A root over minChunkSize with three ~30-byte siblings packs into two
	// chunks under the 750 budget only when tight; here desired is large so it
	// exercises the boundary→Chunk materialization (lines, content).
	src := "line one\nline two\nline three\nline four\n"
	srcLen := uint32(len(src)) //nolint:gosec // fixed test fixture
	root := parent(0, srcLen,
		leaf(0, 18),  // "line one\nline two\n"
		leaf(18, 29), // "line three\n"
		leaf(29, srcLen),
	)
	got := chunkSource(src, "x.fake", "python", fakeParser{root: root, ok: true})
	if len(got) != 1 {
		t.Fatalf("chunk count = %d, want 1 (all siblings fit 750)", len(got))
	}
	if got[0].StartLine != 1 || got[0].EndLine != 4 {
		t.Errorf("lines = %d-%d, want 1-4", got[0].StartLine, got[0].EndLine)
	}
	if got[0].Content != src {
		t.Errorf("content = %q, want whole source", got[0].Content)
	}
}

func TestChunkSourceLineFallback(t *testing.T) {
	// ok=false (no grammar) falls back to line chunking regardless of language.
	src := "a = 1\nb = 2\nc = 3\n"
	got := chunkSource(src, "x.py", "python", fakeParser{ok: false})
	if len(got) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(got))
	}
	if got[0].StartLine != 1 || got[0].EndLine != 3 {
		t.Errorf("lines = %d-%d, want 1-3", got[0].StartLine, got[0].EndLine)
	}
}

// TestChunkOversizedSkipsDecode pins C5: the 1 MB gate is checked before the
// whole content is decoded, so an oversized input is rejected without the large
// intermediate allocation DecodeReplace would make.
func TestChunkOversizedSkipsDecode(t *testing.T) {
	content := []byte(strings.Repeat("x = 1\n", 200_000)) // ~1.2 MB of valid Python
	if got := Chunk("big.py", content); got != nil {
		t.Fatalf("Chunk() = %d chunks, want nil for oversized input", len(got))
	}

	const iters = 20
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range iters {
		Chunk("big.py", content)
	}
	runtime.ReadMemStats(&after)

	perCall := (after.TotalAlloc - before.TotalAlloc) / iters
	if perCall > uint64(len(content))/2 {
		t.Errorf("per-call alloc = %d bytes, want << input size %d: the decode must be skipped before the size gate",
			perCall, len(content))
	}
}

func TestChunkGates(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		content   string
		wantEmpty bool
	}{
		{"over 1MB", "big.py", strings.Repeat("x = 1\n", 200_000), true}, // 1.2 MB
		{"whitespace under 128", "s.py", "  \n\t\n", true},
		{"small with content is indexed", "s.py", "x = 1\n", false},
		{"whitespace over 128 yields nothing", "w.py", strings.Repeat(" ", 200), true},
		{"data language excluded", "d.json", "{\"a\": 1, \"b\": [1, 2, 3], \"c\": true}\n", true},
		{"csv excluded", "d.csv", "a,b,c\n1,2,3\n4,5,6\n", true},
		{"unmapped extension indexed", "notes.unknownext", "some real content here\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Chunk(tt.path, []byte(tt.content))
			if tt.wantEmpty && len(got) != 0 {
				t.Errorf("Chunk() = %d chunks, want 0", len(got))
			}
			if !tt.wantEmpty && len(got) == 0 {
				t.Error("Chunk() = 0 chunks, want at least 1")
			}
		})
	}
}

func TestChunkLanguageDetectionFallback(t *testing.T) {
	first := strings.Repeat("a", 400) + "\n"
	second := strings.Repeat("b", 400) + "\n"
	content := first + second + "tail\n"
	tests := []struct {
		name string
		path string
		want []semsearch.Chunk
	}{
		{
			name: "unknown extension line chunks",
			path: "special.kjs",
			want: []semsearch.Chunk{
				{Path: "special.kjs", StartLine: 1, EndLine: 1, Content: first},
				{Path: "special.kjs", StartLine: 2, EndLine: 3, Content: second + "tail\n"},
			},
		},
		{
			name: "known data language skips",
			path: "special.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Chunk(tt.path, []byte(content))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Chunk() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
