package vcs

import (
	"os"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	vcstest.Cleanup()
	os.Exit(code)
}
