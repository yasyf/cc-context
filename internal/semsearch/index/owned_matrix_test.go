package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

type ownedMatrixProcessReceipt struct {
	N             int
	Work          OwnedAnswerWork
	Allocated     uint64
	Allocations   uint64
	LoadNanos     int64
	CanonicalRead int64
	Results       [][]semsearch.Result
}

func TestOwnedAnswersMappedWarmProcess(t *testing.T) {
	for _, n := range []int{32, 10000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			corpus, source := ownedAnswerFixture(n)
			root := t.TempDir()
			idx, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, &ownedAnswerChunker{}, "mapped-process")
			if err != nil {
				t.Fatal(err)
			}
			if err := idx.Close(); err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(root, "ccx", "*", "*.matrix"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("matrix paths=%v err=%v", paths, err)
			}
			copyRoot := t.TempDir()
			relative, err := filepath.Rel(root, paths[0])
			if err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(copyRoot, relative)
			if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copyPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(executable, "-test.run=^TestOwnedAnswersMappedWarmChild$", "-test.v") // #nosec G204 -- os.Executable selects this test binary and the single-test selector is literal
			child.Env = append(os.Environ(), "CCX_OWNED_MATRIX_CHILD_ROOT="+copyRoot, "CCX_OWNED_MATRIX_CHILD_N="+strconv.Itoa(n))
			output, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("mapped child failed: %v\n%s", err, output)
			}
			var receipts []ownedMatrixProcessReceipt
			for line := range strings.SplitSeq(string(output), "\n") {
				if value, ok := strings.CutPrefix(line, "OWNED_ANSWER_FRESH_PROCESS "); ok {
					var receipt ownedMatrixProcessReceipt
					if err := json.Unmarshal([]byte(value), &receipt); err != nil {
						t.Fatal(err)
					}
					receipts = append(receipts, receipt)
				}
			}
			if len(receipts) != 1 {
				t.Fatalf("child receipts=%d\n%s", len(receipts), output)
			}
			receipt := receipts[0]
			if receipt.N != n || receipt.CanonicalRead != 0 || receipt.Work.MaterializationSQLRows != 0 || receipt.Work.MaterializedChunks != 0 || receipt.Work.DecodedValues != 0 || receipt.Work.PublishedBytes != 0 || receipt.Work.MappedBytes != len(data) {
				t.Fatalf("warm child rebuilt storage: %+v", receipt)
			}
			if databases, err := filepath.Glob(filepath.Join(copyRoot, "ccx", "*", "index.sqlite")); err != nil || len(databases) != 0 {
				t.Fatalf("warm child opened a replacement working database: %v %v", databases, err)
			}
			documents := slices.Clone(corpus.Documents)
			slices.SortFunc(documents, func(a, b OwnedAnswerDocument) int { return strings.Compare(a.Path, b.Path) })
			var chunks []semsearch.Chunk
			var vectors [][]float32
			chunker := &ownedAnswerChunker{}
			for _, document := range documents {
				chunks = append(chunks, chunker.ChunkFile(t.Context(), document.Path, "markdown", source.texts[document.ID])...)
				vectors = append(vectors, detVec(source.texts[document.ID], 8))
			}
			queries := []string{"alpha beta alpha", "0000001", "nomatch"}
			if len(receipt.Results) != len(queries) {
				t.Fatalf("child result count=%d", len(receipt.Results))
			}
			for i, query := range queries {
				want := rank.Rank(query, detVec(query, 8), chunks, vectors, rank.Options{TopK: 5})
				for j := range want {
					want[j].Content = ""
				}
				if !reflect.DeepEqual(receipt.Results[i], want) {
					t.Fatalf("fresh-process rank differs n=%d query=%q", n, query)
				}
			}
			t.Logf("OWNED_ANSWER_FRESH_PROCESS_COST n=%d mapped_bytes=%d allocated_bytes=%d allocations=%d load_ns=%d dense_values=%d", n, receipt.Work.MappedBytes, receipt.Allocated, receipt.Allocations, receipt.LoadNanos, n*8)
		})
	}
}

func TestOwnedAnswersMappedWarmChild(t *testing.T) {
	root := os.Getenv("CCX_OWNED_MATRIX_CHILD_ROOT")
	if root == "" {
		return
	}
	n, err := strconv.Atoi(os.Getenv("CCX_OWNED_MATRIX_CHILD_N"))
	if err != nil {
		t.Fatal(err)
	}
	corpus, source := ownedAnswerFixture(n)
	source.err = errors.New("warm child requested canonical text")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	idx, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, &ownedAnswerChunker{}, "mapped-process")
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ownedMatrixProcessReceipt{N: n, Work: idx.Work, Allocated: after.TotalAlloc - before.TotalAlloc, Allocations: after.Mallocs - before.Mallocs, LoadNanos: elapsed.Nanoseconds(), CanonicalRead: source.reads.Load()}
	for _, query := range []string{"alpha beta alpha", "0000001", "nomatch"} {
		results := rank.Answers(query, detVec(query, 8), idx, 5)
		for i := range results {
			results[i].FilePath = strings.Clone(results[i].FilePath)
		}
		receipt.Results = append(receipt.Results, results)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("OWNED_ANSWER_FRESH_PROCESS %s\n", data)
}

func TestOwnedAnswersMappedReaderSurvivesPublication(t *testing.T) {
	corpus, source := ownedAnswerFixture(32)
	root := t.TempDir()
	emb := &countingEmbedder{}
	chunker := &ownedAnswerChunker{}
	old, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, chunker, "mapped-reader")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := old.Close(); err != nil {
			t.Error(err)
		}
	}()
	oldVector := slices.Clone(old.Vector(0))
	oldPath := strings.Clone(old.Chunk(0).Path)
	corpus.Documents = slices.Clone(corpus.Documents)
	corpus.Generation = "mapped-next"
	document := &corpus.Documents[len(corpus.Documents)-1]
	document.TextDigest = contentHash("changed canonical source")
	source.texts[document.ID] = "changed canonical source"
	next, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, chunker, "mapped-reader")
	if err != nil {
		t.Fatal(err)
	}
	nextVector := slices.Clone(next.Vector(0))
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if slices.Equal(nextVector, oldVector) {
		t.Fatal("changed document reused its old vector")
	}
	if old.Chunk(0).Path != oldPath || !slices.Equal(old.Vector(0), oldVector) {
		t.Fatal("new publication changed an open immutable mapping")
	}
}
