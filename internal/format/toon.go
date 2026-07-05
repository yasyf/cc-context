package format

import (
	"fmt"

	"github.com/toon-format/toon-go"
)

// Delimiter is the character separating values inside TOON array scopes.
type Delimiter = toon.Delimiter

// The supported array delimiters.
const (
	DelimiterComma = toon.DelimiterComma
	DelimiterTab   = toon.DelimiterTab
	DelimiterPipe  = toon.DelimiterPipe
)

// encodeTOON marshals the IR to TOON with the options' indent and delimiter.
func encodeTOON(v any, opts Options) (string, error) {
	out, err := toon.MarshalString(v,
		toon.WithIndent(opts.Indent),
		toon.WithArrayDelimiter(opts.Delimiter),
		toon.WithDocumentDelimiter(opts.Delimiter),
	)
	if err != nil {
		return "", fmt.Errorf("marshal toon: %w", err)
	}
	return out, nil
}
