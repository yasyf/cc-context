package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	cleanupRateCapKey   = "ccx.cleanupRateCap"
	cleanupBusyRateKey  = "ccx.cleanupBusyRate"
	cleanupQuietRootKey = "ccx.cleanupQuietRoot"
)

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

func parseCleanupBusyDeletes(rate, quiet string) (cleanup.BusyDeletes, error) {
	var busy cleanup.BusyDeletes
	if rate = strings.TrimSpace(rate); rate != "" {
		parsed, err := strconv.Atoi(rate)
		if err != nil || parsed <= 0 {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be entries per second, got %q", cleanupBusyRateKey, rate)
		}
		busy.Rate = parsed
	}
	for _, root := range strings.Fields(quiet) {
		if !filepath.IsAbs(root) {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be an absolute directory, got %q", cleanupQuietRootKey, root)
		}
		busy.Quiet = append(busy.Quiet, filepath.Clean(root))
	}
	return busy, nil
}
