package dispatch

import (
	"github.com/yasyf/cc-context/internal/cleanup/native"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

var indexingBand index.Band = native.Band{}
