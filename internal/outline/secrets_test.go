package outline

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

const (
	rawAWSKey    = "AKIAIOSFODNN7EXAMPLE"                                          //nolint:gosec // AWS's documented example key id, not a credential
	rawEnvSecret = "AWARD_WALLET_API_KEY=3fd9a8c1e5b7024d6f8e9a0c1b2d3e4f5a6b7c8d" //nolint:gosec // synthetic fixture value, not a credential
)

// pemFixture builds a file whose 50-line private-key block outruns the 40-line
// head window (END on line 50), followed by trailing prose lines through raw
// line 50+trailing.
func pemFixture(trailing int) string {
	var b strings.Builder
	b.WriteString("-----BEGIN PRIVATE KEY-----\n")
	for i := 2; i < 50; i++ {
		fmt.Fprintf(&b, "cGVtYm9keXJvd251bWJlciUwNGQlMDRkJTA0ZA%04d\n", i)
	}
	b.WriteString("-----END PRIVATE KEY-----\n")
	for i := 1; i <= trailing; i++ {
		fmt.Fprintf(&b, "after row %04d\n", i)
	}
	return b.String()
}

// TestFallbackMasksWindowedMultilineSecret proves the head window masks the
// FULL source before windowing: a private key whose END marker sits past the
// 40-line window still masks — the visible prefix never leaks — the window
// folds to the masked stub, and the continuation pointer stays in raw-line
// coordinates. RevealSecrets windows the raw lines as before.
func TestFallbackMasksWindowedMultilineSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(pemFixture(65)), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, ids, err := Fallback(path, backend.Args{})
	if err != nil {
		t.Fatalf("Fallback: %v", err)
	}
	if strings.Contains(got, "cGVtYm9keXJvd") {
		t.Errorf("head window leaked private-key body lines:\n%s", got)
	}
	if !strings.Contains(got, "----…[masked:private-key]") {
		t.Errorf("head window missing the folded masked stub:\n%s", got)
	}
	if !reflect.DeepEqual(ids, []string{"private-key"}) {
		t.Errorf("Fallback ids = %v, want [private-key]", ids)
	}
	// 115 raw lines fold to 66 display lines; the 40-line window covers the
	// masked block (raw 1-50) plus 39 trailing rows, so the continuation resumes
	// at raw line 90.
	if !strings.Contains(got, "— 115 lines,") {
		t.Errorf("header must keep the raw line count:\n%s", got)
	}
	if !strings.Contains(got, "## "+path+":1-89#") {
		t.Errorf("window span must cover raw lines 1-89:\n%s", got)
	}
	if !strings.Contains(got, "… continue: ccx code read "+path+" --section 90-115\n") {
		t.Errorf("continuation pointer must resume at raw line 90:\n%s", got)
	}

	raw, rawIDs, err := Fallback(path, backend.Args{RevealSecrets: true})
	if err != nil {
		t.Fatalf("Fallback(reveal): %v", err)
	}
	if rawIDs != nil {
		t.Errorf("Fallback(reveal) ids = %v, want nil", rawIDs)
	}
	if !strings.Contains(raw, "-----BEGIN PRIVATE KEY-----") || !strings.Contains(raw, "cGVtYm9keXJvd") {
		t.Errorf("reveal render missing the raw key prefix:\n%s", raw)
	}
	if !strings.Contains(raw, "… continue: ccx code read "+path+" --section 41-115\n") {
		t.Errorf("reveal continuation must window raw lines 1-40:\n%s", raw)
	}
}

// TestFallbackMasksSecrets proves the fallback head window masks detected
// secrets in the outlined file's path context — the generic-api-key rule fires
// only under the env-shaped fixture — returning the fired rule ids, while
// RevealSecrets and a secret-free file come back byte-identical with no ids.
// The masked expectation is derived from the raw render, so header drift cannot
// mask a leak.
func TestFallbackMasksSecrets(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		secret  string
		masked  string
		wantIDs []string
	}{
		{
			name:    "env head window masked",
			file:    ".env",
			content: rawEnvSecret + "\n",
			secret:  rawEnvSecret,
			masked:  "AWAR…[masked:generic-api-key]",
			wantIDs: []string{"generic-api-key"},
		},
		{
			name:    "aws key outside an env path masked",
			file:    "notes.txt",
			content: "key = \"" + rawAWSKey + "\"\n",
			secret:  rawAWSKey,
			masked:  "AKIA…[masked:aws-access-token]",
			wantIDs: []string{"aws-access-token"},
		},
		{
			name:    "no findings byte-identical",
			file:    "notes.txt",
			content: "plain text\nno credentials here\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.file)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			raw, rawIDs, err := Fallback(path, backend.Args{RevealSecrets: true})
			if err != nil {
				t.Fatalf("Fallback(reveal): %v", err)
			}
			if rawIDs != nil {
				t.Errorf("Fallback(reveal) ids = %v, want nil", rawIDs)
			}
			if tt.secret != "" && !strings.Contains(raw, tt.secret) {
				t.Fatalf("raw render missing the fixture secret:\n%s", raw)
			}

			got, ids, err := Fallback(path, backend.Args{})
			if err != nil {
				t.Fatalf("Fallback: %v", err)
			}
			want := raw
			if tt.secret != "" {
				want = strings.Replace(raw, tt.secret, tt.masked, 1)
			}
			if got != want {
				t.Errorf("Fallback masked mismatch\n got: %q\nwant: %q", got, want)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("Fallback ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}
