package index

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mapOwnedFile(file *os.File, size int) ([]byte, func() error, error) {
	handle, err := windows.CreateFileMapping(windows.Handle(file.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	address, err := windows.MapViewOfFile(handle, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		return nil, nil, errors.Join(err, windows.CloseHandle(handle))
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(address)), size)
	return data, func() error {
		return errors.Join(windows.UnmapViewOfFile(address), windows.CloseHandle(handle))
	}, nil
}
