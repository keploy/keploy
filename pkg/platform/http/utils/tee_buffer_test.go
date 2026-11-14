//go:build linux || darwin || windows

package utils

import (
	"bytes"
	"sync"
)

// teeBuffer is a buffer a PTY's output copy writes to while a row reads it.
type teeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *teeBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

func (t *teeBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}
