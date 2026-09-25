package ripgrep

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
)

func TestRegexRetryFailureDoesNotReportNoMatches(t *testing.T) {
	for _, filesOnly := range []bool{false, true} {
		for _, failure := range []error{errors.New("grep stdout exceeded 16 bytes"), context.Canceled, context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("files=%t/%v", filesOnly, failure), func(t *testing.T) {
				calls := 0
				runner := func(context.Context, render.Dir, string, []string) (string, error) {
					calls++
					if calls == 1 {
						return "", nil
					}
					return "", failure
				}
				output, found, err := run(context.Background(), engineRipgrep, "rg", render.Dir(t.TempDir()), backend.Args{Query: "a.*", FilesWithMatches: filesOnly}, runner)
				if !errors.Is(err, failure) || output != "" || found || calls != 2 {
					t.Fatalf("output=%q found=%t calls=%d error=%v, want retry error %v", output, found, calls, err, failure)
				}
			})
		}
	}
}
