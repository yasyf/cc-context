package chunk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const corpusDir = "../testdata/corpus"

type goldenChunk struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type goldenFile struct {
	FilePath       string `json:"file_path"`
	Language       string `json:"language"`
	Bytes          int    `json:"bytes"`
	Classification string `json:"classification"`
	ChunkCount     int    `json:"chunk_count"`
}

type goldenDoc struct {
	Chunks []goldenChunk `json:"chunks"`
	Files  []goldenFile  `json:"files"`
}

func loadGoldens(t *testing.T) goldenDoc {
	t.Helper()
	raw, err := os.ReadFile("../testdata/goldens/chunks.json")
	if err != nil {
		t.Fatalf("read goldens: %v", err)
	}
	var doc goldenDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode goldens: %v", err)
	}
	return doc
}

func sortChunks(cs []goldenChunk) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Path != cs[j].Path {
			return cs[i].Path < cs[j].Path
		}
		if cs[i].StartLine != cs[j].StartLine {
			return cs[i].StartLine < cs[j].StartLine
		}
		return cs[i].EndLine < cs[j].EndLine
	})
}

// TestChunkCorpusGoldens is the parity gate: chunking every corpus file must
// reproduce semble 0.5.2's exact chunk boundaries (testdata/goldens/chunks.json).
func TestChunkCorpusGoldens(t *testing.T) {
	doc := loadGoldens(t)

	var got []goldenChunk
	for _, f := range doc.Files {
		content, err := os.ReadFile(filepath.Join(corpusDir, f.FilePath))
		if err != nil {
			t.Fatalf("read corpus %s: %v", f.FilePath, err)
		}
		for _, c := range Chunk(f.FilePath, content) {
			got = append(got, goldenChunk{Path: c.Path, StartLine: c.StartLine, EndLine: c.EndLine})
		}
	}

	want := append([]goldenChunk(nil), doc.Chunks...)
	sortChunks(got)
	sortChunks(want)

	if len(got) != len(want) {
		t.Errorf("chunk count = %d, want %d", len(got), len(want))
	}
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			t.Errorf("chunk[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	for i := n; i < len(got); i++ {
		t.Errorf("unexpected extra chunk: %+v", got[i])
	}
	for i := n; i < len(want); i++ {
		t.Errorf("missing chunk: %+v", want[i])
	}
}

// TestMidLineSplitContent pins semble's character-granular packing: on
// web/sessionStore.ts the packer splits line 7 mid-line, so two chunks share
// that line. The committed goldens store only line spans (lossy at this split),
// so the byte-exact boundary is asserted through chunk content.
func TestMidLineSplitContent(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(corpusDir, "web/sessionStore.ts"))
	if err != nil {
		t.Fatal(err)
	}
	chunks := Chunk("web/sessionStore.ts", content)
	if len(chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(chunks))
	}
	if chunks[0].EndLine != 7 || chunks[1].StartLine != 7 {
		t.Fatalf("split lines = %d/%d, want both on line 7", chunks[0].EndLine, chunks[1].StartLine)
	}
	if !strings.HasSuffix(chunks[0].Content, "export class SessionStore") {
		t.Errorf("chunk[0] content tail = %q, want it to end at the mid-line split before {", tail(chunks[0].Content))
	}
	if !strings.HasPrefix(chunks[1].Content, "{") {
		t.Errorf("chunk[1] content head = %q, want it to begin at {", head(chunks[1].Content))
	}
}

func head(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

// TestChunkCorpusClassification checks each file's chunk count against the
// golden, covering the file gates and the data/unsupported exclusions.
func TestChunkCorpusClassification(t *testing.T) {
	doc := loadGoldens(t)
	for _, f := range doc.Files {
		content, err := os.ReadFile(filepath.Join(corpusDir, f.FilePath))
		if err != nil {
			t.Fatalf("read corpus %s: %v", f.FilePath, err)
		}
		got := len(Chunk(f.FilePath, content))
		if got != f.ChunkCount {
			t.Errorf("%s (%s): chunk count = %d, want %d [%s]",
				f.FilePath, f.Language, got, f.ChunkCount, f.Classification)
		}
	}
}
