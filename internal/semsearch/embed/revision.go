package embed

// Repo and Revision pin the model2vec model to an exact HuggingFace commit, so
// the downloaded weights — and therefore every produced vector — are
// reproducible. Revision is the main-branch commit SHA resolved from the
// HuggingFace API on 2026-07-22.
//
// The parallel internal/semsearch/testdata/pins.json lane is the eventual
// source of truth for the pin; this constant may be reconciled against it once
// that file lands.
const (
	Repo     = "minishlab/potion-code-16M-v2"
	Revision = "e9d2a44ca6a05ac6685f3b23709ea57eb7352d5b"
)
