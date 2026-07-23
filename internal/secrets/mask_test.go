package secrets

import (
	"reflect"
	"testing"
)

const awsKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // AWS's documented example key id, not a credential

// gcpKey is a 39-byte "AIza"+35 GCP key shape: whole-match (no secretGroup), so
// its trailing delimiter is trimmed before masking.
const gcpKey = "AIzaSyD-a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P" //nolint:gosec // synthetic high-entropy shape, not a credential

func TestMask(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		path    string
		want    string
		wantIDs []string
	}{
		{
			name:    "aws key in go source",
			text:    "package main\n\nconst key = \"" + awsKey + "\"\n",
			path:    "main.go",
			want:    "package main\n\nconst key = \"AKIA…[masked:aws-access-token]\"\n",
			wantIDs: []string{"aws-access-token"},
		},
		{
			name:    "generic api key in env path",
			text:    "AWARD_WALLET_API_KEY=3fd9a8c1e5b7024d6f8e9a0c1b2d3e4f5a6b7c8d\n",
			path:    "/tmp/proj/.env",
			want:    "AWAR…[masked:generic-api-key]\n",
			wantIDs: []string{"generic-api-key"},
		},
		{
			name: "generic api key skipped outside env path",
			text: "AWARD_WALLET_API_KEY=3fd9a8c1e5b7024d6f8e9a0c1b2d3e4f5a6b7c8d\n",
			path: "config.go",
			want: "AWARD_WALLET_API_KEY=3fd9a8c1e5b7024d6f8e9a0c1b2d3e4f5a6b7c8d\n",
		},
		{
			name: "low entropy value not masked",
			text: "PASSWORD=aaaaaaaaaaaa\n",
			path: ".env",
			want: "PASSWORD=aaaaaaaaaaaa\n",
		},
		{
			name:    "repeated secret on one line masks both spans",
			text:    "A=" + awsKey + " B=" + awsKey + "\n",
			path:    "notes.txt",
			want:    "A=AKIA…[masked:aws-access-token] B=AKIA…[masked:aws-access-token]\n",
			wantIDs: []string{"aws-access-token", "aws-access-token"},
		},
		{
			name: "multi-line pem block masked once",
			text: "cfg:\n-----BEGIN PRIVATE KEY-----\n" +
				"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj\n" +
				"MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu\n" +
				"-----END PRIVATE KEY-----\ndone\n",
			path:    "key.pem",
			want:    "cfg:\n----…[masked:private-key]\ndone\n",
			wantIDs: []string{"private-key"},
		},
		{
			name:    "section gutter left intact",
			text:    " 12  AWS_KEY=" + awsKey + "\n 13  NEXT=ok\n",
			path:    "internal/config.go",
			want:    " 12  AWS_KEY=AKIA…[masked:aws-access-token]\n 13  NEXT=ok\n",
			wantIDs: []string{"aws-access-token"},
		},
		{
			name:    "whole-match gcp key keeps its closing quote",
			text:    "const key = \"" + gcpKey + "\"\n",
			path:    "config.go",
			want:    "const key = \"AIza…[masked:gcp-api-key]\"\n",
			wantIDs: []string{"gcp-api-key"},
		},
		{
			name: "no findings returns text byte-identical",
			text: "package render\n\nfunc Cap(s string, budgetTokens int) string {\n\treturn s\n}\n",
			path: "render.go",
			want: "package render\n\nfunc Cap(s string, budgetTokens int) string {\n\treturn s\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ids := Mask(tt.text, tt.path)
			if got != tt.want {
				t.Errorf("Mask() text = %q, want %q", got, tt.want)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("Mask() rule ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

// TestMaskLines proves the line-preserving mask: single-line secrets mask in
// place with an identity Src mapping, a multiline private key spanning lines
// folds them into the span's first line — Src recording the fold so callers
// can re-map their per-line bookkeeping — and text after the fold keeps its
// original line attribution.
func TestMaskLines(t *testing.T) {
	pem := []string{
		"-----BEGIN PRIVATE KEY-----",
		"bm90YXJlYWxrZXlqdXN0YWZpeHR1cmVmb3J0ZXN0aW5nMDAx",
		"bm90YXJlYWxrZXlqdXN0YWZpeHR1cmVmb3J0ZXN0aW5nMDAy",
		"-----END PRIVATE KEY-----",
	}
	tests := []struct {
		name  string
		lines []string
		path  string
		want  []MaskedLine
	}{
		{
			name:  "no findings identity",
			lines: []string{"alpha", "beta"},
			path:  "main.go",
			want:  []MaskedLine{{Text: "alpha", Src: 0}, {Text: "beta", Src: 1}},
		},
		{
			name:  "single-line secret masks in place",
			lines: []string{"a = 1", "key = \"" + awsKey + "\"", "b = 2"},
			path:  "main.go",
			want: []MaskedLine{
				{Text: "a = 1", Src: 0},
				{Text: "key = \"AKIA…[masked:aws-access-token]\"", Src: 1, Rules: []string{"aws-access-token"}},
				{Text: "b = 2", Src: 2},
			},
		},
		{
			name:  "multiline private key folds into its first line",
			lines: append(append([]string{"header"}, pem...), "footer"),
			path:  "key.pem",
			want: []MaskedLine{
				{Text: "header", Src: 0},
				{Text: "----…[masked:private-key]", Src: 1, Rules: []string{"private-key"}},
				{Text: "footer", Src: 5},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskLines(tt.lines, tt.path); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MaskLines() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEnvShaped(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".env", true},
		{"/tmp/proj/.env", true},
		{".env.local", true},
		{"prod.env", true},
		{".envrc", true},
		{"/home/u/.aws/credentials", true},
		{".netrc", true},
		{"main.go", false},
		{"environment.ts", false},
		{"env", false},
		{"envfile.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := envShaped(tt.path); got != tt.want {
				t.Errorf("envShaped(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestMergeSpans(t *testing.T) {
	tests := []struct {
		name string
		in   []span
		want []span
	}{
		{"disjoint spans kept", []span{{0, 5, "a"}, {10, 15, "b"}}, []span{{0, 5, "a"}, {10, 15, "b"}}},
		{"adjacent half-open spans stay separate", []span{{0, 5, "a"}, {5, 10, "b"}}, []span{{0, 5, "a"}, {5, 10, "b"}}},
		{"staggered overlap extends end, first rule id wins", []span{{0, 10, "a"}, {5, 20, "b"}}, []span{{0, 20, "a"}}},
		{"nested span does not shrink the container", []span{{0, 20, "a"}, {5, 10, "b"}}, []span{{0, 20, "a"}}},
		{"equal start keeps the longer, absorbs the shorter", []span{{0, 5, "short"}, {0, 12, "long"}}, []span{{0, 12, "long"}}},
		{"chain of overlaps folds into one", []span{{0, 6, "a"}, {4, 10, "b"}, {9, 15, "c"}}, []span{{0, 15, "a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeSpans(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mergeSpans() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReplacement(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		want   string
	}{
		{"16-byte span keeps a 4-byte stub", "0123456789abcdef", "0123…[masked:r]"},
		{"long span keeps a 4-byte stub", "0123456789abcdefghij", "0123…[masked:r]"},
		{"15-byte span masks whole", "0123456789abcde", "[masked:r]"},
		{"short span masks whole", "0123456789ab", "[masked:r]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replacement(tt.secret, "r"); got != tt.want {
				t.Errorf("replacement(%q) = %q, want %q", tt.secret, got, tt.want)
			}
		})
	}
}
