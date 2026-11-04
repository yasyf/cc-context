package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const cleanupRateCapKey = "ccx.cleanupRateCap"

func parseCleanupRateCaps(values string) ([]cleanup.RateCap, error) {
	var caps []cleanup.RateCap
	for _, value := range strings.Fields(values) {
		at := strings.LastIndexByte(value, '=')
		if at < 0 {
			return nil, fmt.Errorf("%s must be <absolute-root>=<entries-per-second>, got %q", cleanupRateCapKey, value)
		}
		root := value[:at]
		rate, err := strconv.Atoi(value[at+1:])
		if err != nil || rate <= 0 || !filepath.IsAbs(root) {
			return nil, fmt.Errorf("%s must be <absolute-root>=<entries-per-second>, got %q", cleanupRateCapKey, value)
		}
		caps = append(caps, cleanup.RateCap{Root: filepath.Clean(root), Rate: rate})
	}
	return caps, nil
}
