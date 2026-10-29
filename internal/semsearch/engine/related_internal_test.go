package engine

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

type relatedLimitEmbedder struct{}

func (relatedLimitEmbedder) Dims() int { return 1 }

func (relatedLimitEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1}
	}
	return vectors, nil
}

func TestRelatedHugeK(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	root, err := filepath.Abs(filepath.Join("testdata", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	args := backend.Args{Query: "auth/login.go:4", Path: root, K: 1000}
	want, err := Related(t.Context(), relatedLimitEmbedder{}, args)
	if err != nil || len(want) == 0 {
		t.Fatalf("Related reference = %v, %v; want nonempty results", want, err)
	}
	args.K = math.MaxInt
	got, err := Related(t.Context(), relatedLimitEmbedder{}, args)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Related huge K = %v, %v; want %v, nil", got, err, want)
	}
}
