package native

import (
	"testing"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

func TestCallbackErrnoBoundary(t *testing.T) {
	handle, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := purego.Dlclose(handle); err != nil {
			t.Error(err)
		}
	})
	closeFD, err := purego.Dlsym(handle, "close")
	if err != nil {
		t.Fatal(err)
	}
	callback := served(t, func(_, _, _ uintptr) (uintptr, unix.Errno) {
		return 0, unix.EPERM
	})
	for _, test := range []struct {
		name   string
		symbol uintptr
		result int32
		errno  unix.Errno
	}{
		{"Go callback EPERM", callback, 0, unix.EPERM},
		{"native close EBADF", closeFD, -1, unix.EBADF},
	} {
		t.Run(test.name, func(t *testing.T) {
			for index := range 32 {
				result, errno := call(test.symbol, ^uintptr(0))
				if result != test.result || errno != test.errno {
					t.Fatalf("call %d = (%d, %v), want (%d, %v)", index, result, errno, test.result, test.errno)
				}
			}
		})
	}
}
