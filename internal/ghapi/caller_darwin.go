package ghapi

import (
	"bytes"
	"encoding/binary"
	"os"

	"golang.org/x/sys/unix"
)

func parentArgv() []string {
	raw, err := unix.SysctlRaw("kern.procargs2", os.Getppid())
	if err != nil || len(raw) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	argv := make([]string, 0, argc)
	for len(argv) < argc && len(rest) > 0 {
		arg, tail, _ := bytes.Cut(rest, []byte{0})
		argv = append(argv, string(arg))
		rest = tail
	}
	return argv
}
