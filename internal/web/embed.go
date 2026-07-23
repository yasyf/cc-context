package web

import (
	"context"
	"sync"

	"github.com/yasyf/cc-context/internal/semsearch/embed"
)

// WebPin pins the web-search embedding model to an exact HuggingFace commit, so
// the downloaded weights are reproducible. It matches the revision the oracle
// generates golden_base8m.json from, so the resident engine reproduces Python
// model2vec within parity epsilon.
var WebPin = embed.ModelPin{
	Repo:     "minishlab/potion-base-8M",
	Revision: "bf8b056651a2c21b8d2565580b8569da283cab23",
	Files: [3]embed.WeightFile{
		{Name: "config.json", SHA256: "2a6ac0e9aaa356a68a5688070db78fc3a464fefe85d2f06a1905ce3718687553"},
		{Name: "tokenizer.json", SHA256: "e67e803f624fb4d67dea1c730d06e1067e1b14d830e2c2202569e3ef0f70bb50"},
		{Name: "model.safetensors", SHA256: "f65d0f325faadc1e121c319e2faa41170d3fa07d8c89abd48ca5358d9a223de2"},
	},
}

// EmbedModelID identifies the model as "<repo>@<revision>" for the store's parity
// check: Load discards a cached vector set embedded by a different model. Derived
// from WebPin, it stays byte-identical across the native/Python engine swap, so
// cached page vectors survive the upgrade.
var EmbedModelID = WebPin.Repo + "@" + WebPin.Revision

// UnsupportedReason is the note appended to a BM25-only result when the embedding
// model is unavailable — no cached weights and no network to download them — so
// hybrid ranking cannot run this session.
const UnsupportedReason = "ccx web search runs BM25-only until the embedding model is available (the first run downloads it) — hybrid ranking needs it"

// Embedder embeds texts into L2-normalized vectors, one per text, all of equal
// dimensionality. texts must be non-empty; a text with no in-vocabulary tokens
// embeds to the zero vector.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// engineEmbedder adapts a resident embed.Engine to Embedder, mapping Embed onto
// the engine's Encode. It embeds *embed.Engine so its Close (invoked by
// CloseEmbedder) releases the engine's WASM instance.
type engineEmbedder struct{ *embed.Engine }

// Embed embeds texts via the resident engine's Encode.
func (e engineEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.Encode(ctx, texts)
}

// Resident web embedder: one model2vec engine per process, shared by every web
// search. newWebEmbedder is a var so tests inject a fake instead of the WASM engine.
var (
	webEmbMu       sync.Mutex
	webEmbedder    Embedder
	newWebEmbedder = func(ctx context.Context) (Embedder, error) {
		eng, err := embed.New(ctx, WebPin)
		if err != nil {
			return nil, err
		}
		return engineEmbedder{eng}, nil
	}
)

// sharedEmbedder returns the process's resident web embedder, constructing it on
// first use. A failed construction is not cached, so a later call retries; an
// ErrWeightsUnavailable failure degrades search to BM25-only.
func sharedEmbedder(ctx context.Context) (Embedder, error) {
	webEmbMu.Lock()
	defer webEmbMu.Unlock()
	if webEmbedder != nil {
		return webEmbedder, nil
	}
	e, err := newWebEmbedder(ctx)
	if err != nil {
		return nil, err
	}
	webEmbedder = e
	return e, nil
}

// CloseEmbedder releases the resident web embedder if one was constructed. The
// MCP proxy calls it on shutdown alongside dispatch.CloseEmbedder; a one-shot CLI
// relies on process exit.
func CloseEmbedder(ctx context.Context) error {
	webEmbMu.Lock()
	defer webEmbMu.Unlock()
	if webEmbedder == nil {
		return nil
	}
	closer, ok := webEmbedder.(interface {
		Close(context.Context) error
	})
	webEmbedder = nil
	if !ok {
		return nil
	}
	return closer.Close(ctx)
}

// setEmbedderProvider overrides the resident web-embedder provider and clears the
// cached engine, returning a restore func. Test seam only.
func setEmbedderProvider(p func(context.Context) (Embedder, error)) func() {
	webEmbMu.Lock()
	defer webEmbMu.Unlock()
	prevProvider, prevEmbedder := newWebEmbedder, webEmbedder
	newWebEmbedder, webEmbedder = p, nil
	return func() {
		webEmbMu.Lock()
		defer webEmbMu.Unlock()
		newWebEmbedder, webEmbedder = prevProvider, prevEmbedder
	}
}
