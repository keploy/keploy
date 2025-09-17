package http

import (
	"bufio"
	"bytes"
	"io"

	"go.uber.org/zap"
)

// Checks if the response is gzipped
func isGZipped(check io.ReadCloser, l *zap.Logger) (bool, *bufio.Reader) {
	bufReader := bufio.NewReader(check)
	peekedBytes, err := bufReader.Peek(2)
	if err != nil && err != io.EOF {
		l.Debug("failed to peek the response", zap.Error(err))
		return false, nil
	}
	if len(peekedBytes) < 2 {
		return false, nil
	}
	if peekedBytes[0] == 0x1f && peekedBytes[1] == 0x8b {
		return true, bufReader
	}
	return false, nil
}

// hasCompleteHeaders checks if the given byte slice contains the complete HTTP headers
func hasCompleteHeaders(httpChunk []byte) bool {
	// Define the sequence for header end: "\r\n\r\n"
	headerEndSequence := []byte{'\r', '\n', '\r', '\n'}

	// Check if the byte slice contains the header end sequence
	return bytes.Contains(httpChunk, headerEndSequence)
}

func encode(buffer []byte) string {
	//Encode the buffer to string
	encoded := string(buffer)
	return encoded
}
func decode(encoded string) ([]byte, error) {
	// decode the string to a buffer.
	data := []byte(encoded)
	return data, nil
}
