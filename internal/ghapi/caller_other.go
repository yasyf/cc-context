//go:build !darwin

package ghapi

import (
	"bytes"
	"os"
	"strconv"
)

func parentArgv() []string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getppid()) + "/cmdline")
	if err != nil {
		return nil
	}
	var argv []string
	for _, arg := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
		argv = append(argv, string(arg))
	}
	return argv
}
