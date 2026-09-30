//go:build !darwin

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestConnectUnsupported(t *testing.T) {
	ctl, err := Connect(context.Background(), Options{})
	if !errors.Is(err, cleanup.ErrUnsupported) || ctl != nil {
		t.Fatalf("Connect() = %v, %v; want nil, %v", ctl, err, cleanup.ErrUnsupported)
	}
}
