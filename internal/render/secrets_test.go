package render

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

const (
	rawAWSKey     = "AKIAIOSFODNN7EXAMPLE"                                                    //nolint:gosec // AWS's documented example key id, not a credential
	secretsFooter = "# 1 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n" //nolint:gosec // footer text, not a credential
)

func TestFinalizeReadMasksByDefault(t *testing.T) {
	in := "KEY = \"" + rawAWSKey + "\"\n"
	got, err := Finalize(backend.OpRead, in, backend.Args{Path: "main.go"})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	want := "KEY = \"AKIA…[masked:aws-access-token]\"\n" + secretsFooter
	if got != want {
		t.Errorf("Finalize(OpRead) = %q, want %q", got, want)
	}
}

func TestFinalizeReadRevealSecretsPassesRaw(t *testing.T) {
	in := "KEY = \"" + rawAWSKey + "\"\n"
	got, err := Finalize(backend.OpRead, in, backend.Args{Path: "main.go", RevealSecrets: true})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != in {
		t.Errorf("Finalize(OpRead, RevealSecrets) = %q, want %q", got, in)
	}
}

func TestFinalizeReadNoFindingsNoFooter(t *testing.T) {
	in := "package render\n\nfunc noop() {}\n"
	got, err := Finalize(backend.OpRead, in, backend.Args{Path: "main.go"})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != in {
		t.Errorf("Finalize(OpRead) = %q, want byte-identical input %q", got, in)
	}
}

// TestFinalizeReadMasksBeforeCap pins the mask-before-cap ordering: the kept
// prefix is masked text, a second secret past the budget cut never leaks, and
// the footer counts both findings because masking ran over the whole output.
func TestFinalizeReadMasksBeforeCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("KEY = \"" + rawAWSKey + "\"\n")
	for range 200 {
		b.WriteString("filler line of ordinary content\n")
	}
	b.WriteString("TAIL = \"" + rawAWSKey + "\"\n")

	got, err := Finalize(backend.OpRead, b.String(), backend.Args{Path: "main.go", Budget: 20})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if strings.Contains(got, rawAWSKey) {
		t.Errorf("raw secret leaked into capped output %q", got)
	}
	if !strings.HasPrefix(got, "KEY = \"AKIA…[masked:aws-access-token]\"\n") {
		t.Errorf("kept prefix is not masked text: %q", got)
	}
	if !strings.Contains(got, "tokens omitted") {
		t.Errorf("overflow footer missing from %q", got)
	}
	want := "# 2 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("secrets footer missing or not last: %q", got)
	}
}
