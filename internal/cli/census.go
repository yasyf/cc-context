package cli

import "os"

// workingDir returns the current working directory, or "." if it cannot be read.
func workingDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
