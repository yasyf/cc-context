package symbol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

const rawAWSKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // AWS's documented example key id, not a credential

// writeSecretScope writes a one-file scope whose var initializer embeds a
// detectable AWS key, so the card's signature and --body lines both carry it.
func writeSecretScope(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := "package cfg\n\nvar apiKey = \"" + rawAWSKey + "\"\n\nfunc Use() string {\n\treturn apiKey\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "cfg.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

// TestRunMasksSecrets proves the symbol card masks the defining file's secret in
// its signature and body lines and reports the fired rule ids, while
// RevealSecrets passes both through raw and a secret-free card fires nothing.
func TestRunMasksSecrets(t *testing.T) {
	requireBins(t)
	tests := []struct {
		name     string
		args     backend.Args
		wantText string
		bansText string
		wantIDs  bool
	}{
		{
			name:     "signature and body masked",
			args:     backend.Args{Query: "apiKey", Body: true},
			wantText: "AKIA…[masked:aws-access-token]",
			bansText: rawAWSKey,
			wantIDs:  true,
		},
		{
			name:     "reveal-secrets passes raw",
			args:     backend.Args{Query: "apiKey", Body: true, RevealSecrets: true},
			wantText: rawAWSKey,
			bansText: "[masked:",
		},
		{
			name:     "no findings fires nothing",
			args:     backend.Args{Query: "Use", Body: true},
			wantText: "return apiKey",
			bansText: "[masked:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.Scope = writeSecretScope(t)
			out, ids, err := Run(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(out, tt.wantText) {
				t.Errorf("card missing %q:\n%s", tt.wantText, out)
			}
			if strings.Contains(out, tt.bansText) {
				t.Errorf("card leaked %q:\n%s", tt.bansText, out)
			}
			if !tt.wantIDs {
				if ids != nil {
					t.Errorf("ids = %v, want nil", ids)
				}
				return
			}
			if len(ids) == 0 {
				t.Fatal("ids empty, want fired aws-access-token rules")
			}
			for _, id := range ids {
				if id != "aws-access-token" {
					t.Errorf("unexpected rule id %q in %v", id, ids)
				}
			}
		})
	}
}

// writeRefScope writes a three-file scope: the defining file whose doc comment
// embeds a detectable AWS key, a caller file whose reference row embeds one,
// and a no-outline-rules text file whose definition-shaped line embeds one for
// the degraded lane.
func writeRefScope(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"cfg.go":    "package cfg\n\n// apiKey default: " + rawAWSKey + "\nvar apiKey = \"placeholder\"\n\nfunc Use() string {\n\treturn apiKey\n}\n",
		"caller.go": "package cfg\n\nvar wired = apiKey // fallback " + rawAWSKey + "\n",
		"creds.txt": "const SECRETTOKEN = \"" + rawAWSKey + "\"\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

// TestRunMasksDocCallerAndDegradedRows proves the card's doc paragraph, each
// caller reference row (in its own file's path context), and the degraded-match
// rows all mask their secrets, and that RevealSecrets passes each through raw.
func TestRunMasksDocCallerAndDegradedRows(t *testing.T) {
	requireBins(t)
	tests := []struct {
		name     string
		args     backend.Args
		wantText string
	}{
		{
			name:     "doc comment masked",
			args:     backend.Args{Query: "apiKey"},
			wantText: "apiKey default: AKIA…[masked:aws-access-token]",
		},
		{
			name:     "caller reference row masked in its file's context",
			args:     backend.Args{Query: "apiKey", Callers: true},
			wantText: "fallback AKIA…[masked:aws-access-token]",
		},
		{
			name:     "degraded definition-shaped row masked",
			args:     backend.Args{Query: "SECRETTOKEN"},
			wantText: "const SECRETTOKEN = \"AKIA…[masked:aws-access-token]\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.Scope = writeRefScope(t)
			out, ids, err := Run(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if strings.Contains(out, rawAWSKey) {
				t.Errorf("card leaked the raw secret:\n%s", out)
			}
			if !strings.Contains(out, tt.wantText) {
				t.Errorf("card missing %q:\n%s", tt.wantText, out)
			}
			if len(ids) == 0 {
				t.Error("ids empty, want fired aws-access-token rules")
			}

			reveal := tt.args
			reveal.RevealSecrets = true
			out, ids, err = Run(context.Background(), reveal)
			if err != nil {
				t.Fatalf("Run(reveal): %v", err)
			}
			if !strings.Contains(out, rawAWSKey) {
				t.Errorf("reveal card missing the raw secret:\n%s", out)
			}
			if strings.Contains(out, "[masked:") {
				t.Errorf("reveal card still masked:\n%s", out)
			}
			if ids != nil {
				t.Errorf("reveal ids = %v, want nil", ids)
			}
		})
	}
}
