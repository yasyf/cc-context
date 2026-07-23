package ripgrep

import (
	"context"
	"fmt"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

const (
	rawAWSKey    = "AKIAIOSFODNN7EXAMPLE"                                          //nolint:gosec // AWS's documented example key id, not a credential
	rawEnvSecret = "AWARD_WALLET_API_KEY=3fd9a8c1e5b7024d6f8e9a0c1b2d3e4f5a6b7c8d" //nolint:gosec // synthetic fixture value, not a credential

	pemBegin = "-----BEGIN PRIVATE KEY-----"
	pemBody1 = "bm90YXJlYWxrZXlqdXN0YWZpeHR1cmVmb3J0ZXN0aW5nMDAx" //nolint:gosec // synthetic fixture value, not a credential
	pemBody2 = "bm90YXJlYWxrZXlqdXN0YWZpeHR1cmVmb3J0ZXN0aW5nMDAy" //nolint:gosec // synthetic fixture value, not a credential
	pemEnd   = "-----END PRIVATE KEY-----"
)

// rgEventLine renders one ripgrep --json event (typ "match" or "context") for a
// fake runner's canned output.
func rgEventLine(typ, path, text string, num int) string {
	return fmt.Sprintf(`{"type":%q,"data":{"path":{"text":%q},"lines":{"text":%q},"line_number":%d,"submatches":[]}}`+"\n",
		typ, path, text+"\n", num)
}

// TestRunMasksSecrets proves the grep surface masks each file's contiguous
// match+context block as one text with its own file's path context — the
// generic-api-key rule fires only under an env-shaped path, and a multiline
// private key spanning match and context lines masks as one folded frame — and
// masks the header's query echo pathlessly, appending the shared
// masked-secrets footer after the cap, while --reveal-secrets and secret-free
// output pass through byte-identical.
func TestRunMasksSecrets(t *testing.T) {
	tests := []struct {
		name        string
		args        backend.Args
		out         string
		want        string
		wantNoMatch bool
	}{
		{
			name: "private key across match and context lines masked as one block",
			args: backend.Args{Query: "BEGIN PRIVATE KEY", After: 3},
			out: rgEventLine("match", "nope/key.pem", pemBegin, 1) +
				rgEventLine("context", "nope/key.pem", pemBody1, 2) +
				rgEventLine("context", "nope/key.pem", pemBody2, 3) +
				rgEventLine("context", "nope/key.pem", pemEnd, 4),
			want: "# grep: \"BEGIN PRIVATE KEY\" — 1 matches in 1 files\n\n### nope/key.pem:1\n" +
				"→ [1] ----…[masked:private-key]\n" +
				"# 1 secret(s) masked (private-key) — --reveal-secrets prints raw\n",
		},
		{
			name: "query echoing a secret is masked in the header",
			args: backend.Args{Query: rawAWSKey},
			out:  rgEventLine("match", "nope/config.go", "KEY = \""+rawAWSKey+"\"", 2),
			want: "# grep: \"AKIA…[masked:aws-access-token]\" — 1 matches in 1 files\n\n### nope/config.go:2\n" +
				"→ [2] KEY = \"AKIA…[masked:aws-access-token]\"\n" +
				"# 2 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n",
		},
		{
			name: "query echo masked on no-match",
			args: backend.Args{Query: rawAWSKey},
			out:  "",
			want: "# grep: \"AKIA…[masked:aws-access-token]\" — no matches\n" +
				"# 1 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n",
			wantNoMatch: true,
		},
		{
			name: "reveal-secrets echoes the query raw",
			args: backend.Args{Query: rawAWSKey, RevealSecrets: true},
			out:  rgEventLine("match", "nope/config.go", "KEY = \""+rawAWSKey+"\"", 2),
			want: "# grep: \"" + rawAWSKey + "\" — 1 matches in 1 files\n\n### nope/config.go:2\n" +
				"→ [2] KEY = \"" + rawAWSKey + "\"\n",
		},
		{
			name: "env-shaped match line masked with footer",
			args: backend.Args{Query: "API_KEY"},
			out:  rgEventLine("match", "nope/.env", rawEnvSecret, 1),
			want: "# grep: \"API_KEY\" — 1 matches in 1 files\n\n### nope/.env:1\n" +
				"→ [1] AWAR…[masked:generic-api-key]\n" +
				"# 1 secret(s) masked (generic-api-key) — --reveal-secrets prints raw\n",
		},
		{
			name: "aws key on a context line masked",
			args: backend.Args{Query: "foo", Expand: 1},
			out: rgEventLine("context", "nope/main.go", "KEY = \""+rawAWSKey+"\"", 2) +
				rgEventLine("match", "nope/main.go", "foo here", 3),
			want: "# grep: \"foo\" — 1 matches in 1 files\n\n### nope/main.go:3\n" +
				"  [2] KEY = \"AKIA…[masked:aws-access-token]\"\n" +
				"→ [3] foo here\n" +
				"# 1 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n",
		},
		{
			name: "reveal-secrets passes raw with no footer",
			args: backend.Args{Query: "API_KEY", RevealSecrets: true},
			out:  rgEventLine("match", "nope/.env", rawEnvSecret, 1),
			want: "# grep: \"API_KEY\" — 1 matches in 1 files\n\n### nope/.env:1\n→ [1] " + rawEnvSecret + "\n",
		},
		{
			name: "generic-api-key stays raw outside an env-shaped path",
			args: backend.Args{Query: "API_KEY"},
			out:  rgEventLine("match", "nope/config.go", rawEnvSecret, 4),
			want: "# grep: \"API_KEY\" — 1 matches in 1 files\n\n### nope/config.go:4\n→ [4] " + rawEnvSecret + "\n",
		},
		{
			name: "no findings byte-identical with no footer",
			args: backend.Args{Query: "foo"},
			out:  rgEventLine("match", "nope/a.go", "foo one", 3),
			want: "# grep: \"foo\" — 1 matches in 1 files\n\n### nope/a.go:3\n→ [3] foo one\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := func(context.Context, string, []string) (string, error) { return tt.out, nil }
			got, found, err := run(context.Background(), engineRipgrep, "bin", tt.args, fake)
			if err != nil {
				t.Fatalf("run() err = %v", err)
			}
			if found != !tt.wantNoMatch {
				t.Errorf("run() found = %v, want %v", found, !tt.wantNoMatch)
			}
			if got != tt.want {
				t.Errorf("run()\n got = %q\nwant = %q", got, tt.want)
			}
		})
	}
}
