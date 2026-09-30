package utils

import (
	"io"
	"os"
)

// teeStdout is the process stdout, also copied to tee when one is given.
func teeStdout(tee io.Writer) io.Writer {
	if tee == nil {
		return os.Stdout
	}
	return io.MultiWriter(os.Stdout, tee)
}
