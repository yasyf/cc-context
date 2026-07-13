package outline

import (
	"fmt"
	"os"

	"github.com/yasyf/cc-context/internal/sniff"
)

// BinarySkip returns the skipped-outline row for a binary file.
func BinarySkip(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	mime, binary := sniff.Detect(path)
	if !binary {
		return "", false
	}
	return fmt.Sprintf("%s (binary, %s, %s) [skipped]", path, humanBytes(info.Size()), mime), true
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%dKB", (n+512)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/(1024*1024*1024))
	}
}
