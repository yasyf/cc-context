package web

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/lookpath"
)

var _ Embedder = UVEmbedder{}

// requireUV skips a test that spawns the real embedding driver when uv is
// absent.
func requireUV(t *testing.T) {
	t.Helper()
	if !Supported() {
		t.Skip(UnsupportedReason)
	}
}

func TestParseEmbedVectors(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		texts   int
		want    [][]float32
		wantErr string
	}{
		{"valid", `{"dims":2,"vectors":[[1,0],[0.5,0.5]]}`, 2, [][]float32{{1, 0}, {0.5, 0.5}}, ""},
		{"count mismatch", `{"dims":2,"vectors":[[1,0]]}`, 2, nil, "returned 1 vectors for 2 texts"},
		{"dims mismatch", `{"dims":3,"vectors":[[1,0],[0,1]]}`, 2, nil, "vector 0 has 2 dims, want 3"},
		{"ragged", `{"dims":2,"vectors":[[1,0],[1]]}`, 2, nil, "vector 1 has 1 dims, want 2"},
		{"garbage", `not json`, 1, nil, "decode embedding response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEmbedVectors([]byte(tt.data), tt.texts)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseEmbedVectors = nil error, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q missing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEmbedVectors error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseEmbedVectors = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWarmMarker(t *testing.T) {
	dir := t.TempDir()
	path, warm := warmMarker(dir)
	if warm {
		t.Fatal("warm = true with no marker")
	}
	if path != filepath.Join(dir, ".embed-warm") {
		t.Fatalf("marker path = %q, want it under %q", path, dir)
	}
	if err := os.WriteFile(path, []byte("minishlab/potion-base-8M@stalepin"), 0o600); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}
	if _, warm := warmMarker(dir); warm {
		t.Error("warm = true with a stale model pin")
	}
	if err := os.WriteFile(path, []byte(EmbedModelID), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, warm := warmMarker(dir); !warm {
		t.Error("warm = false with the current model pin")
	}
}

// TestEmbedDriverPathVerifiesContent proves a tampered cached driver is
// rewritten on the next resolve instead of being trusted by filename.
func TestEmbedDriverPathVerifiesContent(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	path, err := embedDriverPath()
	if err != nil {
		t.Fatalf("embedDriverPath error: %v", err)
	}
	if err := os.WriteFile(path, []byte("print('tampered')\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	again, err := embedDriverPath()
	if err != nil {
		t.Fatalf("embedDriverPath after tamper error: %v", err)
	}
	if again != path {
		t.Fatalf("embedDriverPath = %q, want %q", again, path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored driver: %v", err)
	}
	if !bytes.Equal(got, embedDriverSource) {
		t.Error("cached driver not restored to the embedded source after tampering")
	}
}

// hfRefusedDownload reports whether err is HF Hub refusing an unauthenticated
// model download (401/429/CAS) — external infra the integration test skips on.
func hfRefusedDownload(err error) bool {
	msg := err.Error()
	for _, sig := range []string{"401 Unauthorized", "429", "unauthenticated requests", "CAS Client Error"} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// TestEmbedUVMissing proves the launch failure names uv and the pinned
// requirement when uv is off PATH.
func TestEmbedUVMissing(t *testing.T) {
	orig := lookpath.Find
	lookpath.Find = func(string) string { return "" }
	t.Cleanup(func() { lookpath.Find = orig })

	if Supported() {
		t.Fatal("Supported() = true with uv stubbed off PATH")
	}
	_, err := UVEmbedder{}.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("Embed = nil error, want launch failure")
	}
	for _, want := range []string{"uv", model2vecRequirement} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestEmbedIntegration exercises the real driver end to end: 256 dims,
// L2-normalized vectors, determinism within and across calls, and the
// first-success marker. The second call's wall time is the warm cold-start
// the embedTimeout comment records.
func TestEmbedIntegration(t *testing.T) {
	requireUV(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	texts := []string{
		"how do I handle errors in Go",
		"install the package with homebrew",
		"how do I handle errors in Go",
	}
	var e UVEmbedder
	first, err := e.Embed(context.Background(), texts)
	if err != nil {
		if hfRefusedDownload(err) {
			t.Skipf("HF Hub refused the unauthenticated model download (external infra): %v", err)
		}
		t.Fatalf("Embed error: %v", err)
	}
	if len(first) != len(texts) {
		t.Fatalf("Embed returned %d vectors, want %d", len(first), len(texts))
	}
	for i, v := range first {
		if len(v) != 256 {
			t.Fatalf("vector %d has %d dims, want 256", i, len(v))
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if norm := math.Sqrt(sum); math.Abs(norm-1) > 1e-3 {
			t.Errorf("vector %d L2 norm = %v, want 1±1e-3", i, norm)
		}
	}
	if !slices.Equal(first[0], first[2]) {
		t.Error("identical texts embedded to different vectors within one call")
	}

	driver, err := embedDriverPath()
	if err != nil {
		t.Fatalf("embedDriverPath error: %v", err)
	}
	if _, warm := warmMarker(filepath.Dir(driver)); !warm {
		t.Error("first successful Embed left no warm marker")
	}

	start := time.Now()
	second, err := e.Embed(context.Background(), texts)
	warmWall := time.Since(start)
	if err != nil {
		t.Fatalf("second Embed error: %v", err)
	}
	t.Logf("warm embed wall time: %v", warmWall)
	for i := range first {
		if !slices.Equal(first[i], second[i]) {
			t.Errorf("vector %d differs across calls; embedding is not deterministic", i)
		}
	}
}
