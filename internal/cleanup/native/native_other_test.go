//go:build !darwin

package native

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestUnsupported(t *testing.T) {
	tests := []struct {
		name string
		call func() error
	}{
		{"Guard", func() error { return Guard(context.Background(), t.TempDir()) }},
		{"Sample", func() error {
			_, err := NewFSEventsSampler().Sample(context.Background())
			return err
		}},
		{"Background", Band{}.Background},
		{"Foreground", Band{}.Foreground},
		{"ProcessUniqueID", func() error {
			_, err := ProcessUniqueID(os.Getpid())
			return err
		}},
		{"Identify", func() error {
			_, err := Identify(os.Getpid())
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, cleanup.ErrUnsupported) {
				t.Errorf("%s() = %v, want cleanup.ErrUnsupported", tt.name, err)
			}
		})
	}
}
