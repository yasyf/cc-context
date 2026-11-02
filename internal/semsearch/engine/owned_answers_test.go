package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch/index"
)

type matchAnswerTexts map[string]string

func (s matchAnswerTexts) Text(_ context.Context, id string) (string, error) { return s[id], nil }

type matchAnswerEmbedder struct{}

func (matchAnswerEmbedder) Dims() int { return 2 }

func (matchAnswerEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

func TestMatchAnswersExcludesAfterChunkTopK(t *testing.T) {
	source := matchAnswerTexts{}
	corpus := index.OwnedAnswerCorpus{Binding: "binding", Generation: "generation", Source: source}
	for _, id := range []string{"aaaaaaa", "bbbbbbb", "ccccccc"} {
		text := "# Identical title\n\nalpha beta"
		source[id] = text
		digest := sha256.Sum256([]byte(text))
		corpus.Documents = append(corpus.Documents, index.OwnedAnswerDocument{ID: id, Path: id + ".md", TextDigest: hex.EncodeToString(digest[:])})
	}
	root := t.TempDir()
	got, err := MatchAnswers(t.Context(), matchAnswerEmbedder{}, corpus, root, "alpha", []string{"aaaaaaa"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bbbbbbb"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("postfilter got=%v want=%v", got, want)
	}
	got, err = MatchAnswers(t.Context(), matchAnswerEmbedder{}, corpus, root, "alpha", []string{" AAAAAAA ", "aaaaaaa-extra", "aaaaaaa1"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"aaaaaaa", "bbbbbbb"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("literal excludes got=%v want=%v", got, want)
	}
}
