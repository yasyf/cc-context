//go:build !darwin

package dispatch

import "github.com/yasyf/cc-context/internal/semsearch/index"

type noBand struct{}

func (noBand) Background() error { return nil }

func (noBand) Foreground() error { return nil }

var indexingBand index.Band = noBand{}
