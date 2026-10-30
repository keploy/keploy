package util

import "io"

// PrefixReader prepends a byte slice to an underlying reader.
// Once the prefix has been fully consumed, it drops the backing slice so
// long-lived keep-alive connections do not retain large initial buffers.
type PrefixReader struct {
	prefix []byte
	reader io.Reader
}

func NewPrefixReader(prefix []byte, reader io.Reader) *PrefixReader {
	return &PrefixReader{
		prefix: prefix,
		reader: reader,
	}
}

func (r *PrefixReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		if len(r.prefix) == 0 {
			r.prefix = nil
		}
		return n, nil
	}
	return r.reader.Read(p)
}
