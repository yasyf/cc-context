package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

func TestLoadNegatedUnknownExtension(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*\n!special.kjs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", 400) + "\n"
	second := strings.Repeat("b", 400) + "\n"
	content := first + second + "tail\n"
	if err := os.WriteFile(filepath.Join(repo, "special.kjs"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	emb := &countingEmbedder{}
	idx, err := Load(context.Background(), emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []semsearch.Chunk{
		{Path: "special.kjs", StartLine: 1, EndLine: 1, Content: first},
		{Path: "special.kjs", StartLine: 2, EndLine: 3, Content: second + "tail\n"},
	}
	if !reflect.DeepEqual(idx.Chunks, want) {
		t.Errorf("Chunks = %#v, want %#v", idx.Chunks, want)
	}
	if idx.TotalFiles != 1 || idx.Reindexed != 1 {
		t.Errorf("TotalFiles=%d Reindexed=%d, want 1 and 1", idx.TotalFiles, idx.Reindexed)
	}
	if emb.encoded != len(want) {
		t.Errorf("embedded %d texts, want %d", emb.encoded, len(want))
	}
}

// TestIndexDecodesMaximalSubsequences pins that production indexing decodes raw
// bytes with Python's errors="replace" semantics — one U+FFFD per maximal
// invalid subsequence — instead of Go's strings.ToValidUTF8, which collapses a
// contiguous invalid run into a single U+FFFD.
func TestIndexDecodesMaximalSubsequences(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*\n!bad.kjs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "hi " + the invalid UTF-8 surrogate \xed\xa0\x80 + " there".
	if err := os.WriteFile(filepath.Join(repo, "bad.kjs"), []byte("hi \xed\xa0\x80 there"), 0o600); err != nil {
		t.Fatal(err)
	}

	emb := &countingEmbedder{}
	idx, err := Load(context.Background(), emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// python3 -c 'print(b"hi \xed\xa0\x80 there".decode("utf-8","replace"))' → "hi ��� there".
	const want = "hi ��� there"
	if len(idx.Chunks) != 1 {
		t.Fatalf("Chunks = %#v, want exactly one chunk", idx.Chunks)
	}
	if got := idx.Chunks[0].Content; got != want {
		t.Errorf("chunk content = %q, want %q (one U+FFFD per maximal invalid subsequence)", got, want)
	}
}
