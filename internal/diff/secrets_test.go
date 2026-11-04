package diff

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/hunk"
)

const (
	rawAWSKey    = "AKIAIOSFODNN7EXAMPLE"                                          //nolint:gosec // AWS's documented example key id, not a credential
	rawEnvSecret = "AWARD_WALLET_API_KEY=3fd9a8c1e5b7024d6f8e9a0c1b2d3e4f5a6b7c8d" //nolint:gosec // synthetic fixture value, not a credential
)

// TestRenderMasksSecrets proves the diff renderer masks each file's section in
// that file's path context — an env-shaped raw-text section fires the
// generic-api-key rule, a raw-hunks section fires aws-access-token, and a
// renamed file's section masks under the pre-image path too — returning the
// fired rule ids, while reveal and a secret-free diff render byte-identical
// with no ids. The masked expectation is derived from the reveal render, so
// hunk-format drift cannot mask a leak.
func TestRenderMasksSecrets(t *testing.T) {
	before := []byte("a = 1\n")
	after := []byte("a = 1\nkey = \"" + rawAWSKey + "\"\n")

	tests := []struct {
		name    string
		files   []fileReport
		secret  string
		masked  string
		wantIDs []string
	}{
		{
			name:    "env raw text masked",
			files:   []fileReport{{path: ".env", kind: fileKindRawText, raw: "+" + rawEnvSecret + "\n"}},
			secret:  rawEnvSecret,
			masked:  "AWAR…[masked:generic-api-key]",
			wantIDs: []string{"generic-api-key"},
		},
		{
			name: "aws key in raw hunks masked",
			files: []fileReport{{
				path: "config/creds.rb", kind: fileKindRawHunks, ext: ".rb",
				before: before, hunks: hunk.Compute(before, after),
			}},
			secret:  rawAWSKey,
			masked:  "AKIA…[masked:aws-access-token]",
			wantIDs: []string{"aws-access-token"},
		},
		{
			// The post-rename path is not env-shaped, so only the second masking
			// pass — under the pre-image path — fires the env-gated rule.
			name: "renamed env file masked under the pre-image path",
			files: []fileReport{{
				path: "config/settings.txt", renamedFrom: ".env", kind: fileKindRawHunks, ext: ".txt",
				before: before, hunks: hunk.Compute(before, []byte("a = 1\n"+rawEnvSecret+"\n")),
			}},
			secret:  rawEnvSecret,
			masked:  "AWAR…[masked:generic-api-key]",
			wantIDs: []string{"generic-api-key"},
		},
		{
			name:  "no findings byte-identical",
			files: []fileReport{{path: "notes.txt", kind: fileKindRawText, raw: "+plain line\n"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := diffModel{label: "uncommitted", files: tt.files}
			raw, rawIDs := render(m, false, true)
			if rawIDs != nil {
				t.Errorf("render(reveal) ids = %v, want nil", rawIDs)
			}
			if tt.secret != "" && !strings.Contains(raw, tt.secret) {
				t.Fatalf("reveal render missing the fixture secret:\n%s", raw)
			}

			got, ids := render(m, false, false)
			want := raw
			if tt.secret != "" {
				want = strings.Replace(raw, tt.secret, tt.masked, 1)
			}
			if got != want {
				t.Errorf("render masked mismatch\n got: %q\nwant: %q", got, want)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("render ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}
