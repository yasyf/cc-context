package render

import (
	"fmt"
	"os"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/secrets"
)

// Finalize shapes op's raw backend output for the caller. Search and related
// output is reshaped from raw semble JSON and anchored; read output is
// secret-masked before capping (unless a.RevealSecrets) with a footer naming the
// fired rules; OpWebRead passes through (web.Run applies its own byte-exact
// budget+offset before Finalize sees it); every other op caps through Cap. Symbol,
// deps, grep, diff, and outline never reach here — they self-anchor and cap in
// their own packages. The anchor.Files cache is built fresh per call (the MCP proxy
// is resident, so a cached line table would resolve against pre-edit content).
func Finalize(op backend.Op, out string, a backend.Args) (string, error) {
	switch op {
	case backend.OpSearch, backend.OpRelated:
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("finalize: resolve cwd: %w", err)
		}
		reshaped, err := SembleResults(out, anchor.NewFiles(cwd))
		if err != nil {
			return "", err
		}
		return Cap(reshaped, a.Budget), nil
	case backend.OpRead:
		if a.RevealSecrets {
			return Cap(out, a.Budget), nil
		}
		masked, ids := secrets.Mask(out, a.Path)
		return withSecretsFooter(Cap(masked, a.Budget), ids), nil
	case backend.OpWebRead:
		return out, nil
	default:
		return Cap(out, a.Budget), nil
	}
}

// withSecretsFooter appends the one-line masked-secrets notice after capping, so
// it is never truncated away; ids are the fired rules in span order, deduped for
// the notice. No ids means no footer.
func withSecretsFooter(out string, ids []string) string {
	if len(ids) == 0 {
		return out
	}
	seen := make(map[string]bool, len(ids))
	var uniq []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return fmt.Sprintf("%s# %d secret(s) masked (%s) — --reveal-secrets prints raw\n", out, len(ids), strings.Join(uniq, ", "))
}
