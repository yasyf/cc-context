package embed

// ModelPin pins a model2vec model to an exact HuggingFace commit, so the
// downloaded weights — and therefore every produced vector — are reproducible.
// Files are the three assets the WASM engine loads, in no required order.
type ModelPin struct {
	Repo     string
	Revision string
	Files    [3]WeightFile
}

// WeightFile is one model asset with its pinned lowercase-hex sha256, verified
// after every read and download so a corrupt or wrong-revision byte stream
// fails loud.
type WeightFile struct {
	Name   string
	SHA256 string
}

// CodePin pins the code-search model to an exact HuggingFace commit. Revision is
// the main-branch commit SHA resolved from the HuggingFace API on 2026-07-22;
// the checksums were computed from that revision the same day. The parallel
// internal/semsearch/testdata/pins.json lane is the eventual source of truth;
// this value may be reconciled against it once that file lands.
var CodePin = ModelPin{
	Repo:     "minishlab/potion-code-16M-v2",
	Revision: "e9d2a44ca6a05ac6685f3b23709ea57eb7352d5b",
	Files: [3]WeightFile{
		{"config.json", "148e5691a6fcc553437156859701fba017a1ba5d340b170f17e0f3668fb861a7"},
		{"tokenizer.json", "107bbdcbad4bff1d299b7a4c3a2fb17c52890688b7dd0e4c9deab79d3c4f3d45"},
		{"model.safetensors", "75cf7a6c2171b230ad19b1e7d8e0b1aee86da5a02af8e7cacedd9921d227623c"},
	},
}
