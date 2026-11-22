package bodydigest

import (
	"crypto/rand"
	"testing"
)

// The record and serve costs of a large body (a recorded one can be as large
// as pkg.MaxDecompressedSize): run with -bench . -benchmem.

const benchSize = 32 << 20

func benchBody(b *testing.B, n int) []byte {
	b.Helper()
	body := make([]byte, n)
	if _, err := rand.Read(body); err != nil {
		b.Fatal(err)
	}
	return body
}

var benchNote string

// A response with no digest header.
func BenchmarkNoteNoDigestHeader(b *testing.B) {
	body := benchBody(b, benchSize)
	h := map[string]string{"Content-Type": "application/octet-stream", "Content-Length": "33554432"}
	b.SetBytes(benchSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchNote = Note(h, body, body)
	}
}

// An S3 multipart object: an ETag of no form keploy computes.
func BenchmarkNoteMultipartETag(b *testing.B) {
	body := benchBody(b, benchSize)
	h := map[string]string{"ETag": `"0123456789abcdef0123456789abcdef-12"`}
	b.SetBytes(benchSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchNote = Note(h, body, body)
	}
}

// The same, gzip on the wire.
func BenchmarkNoteMultipartETagEncoded(b *testing.B) {
	body := benchBody(b, benchSize)
	wire := benchBody(b, benchSize/2)
	h := map[string]string{"ETag": `"0123456789abcdef0123456789abcdef-12"`}
	b.SetBytes(benchSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchNote = Note(h, body, wire)
	}
}

// One serve of an object with an MD5 ETag and an x-amz-checksum-crc64nvme
// noted: each recomputed over the body sent.
func BenchmarkApplyPerServe(b *testing.B) {
	body := string(benchBody(b, benchSize))
	note := "etag=etag/md5/s;x-amz-checksum-crc64nvme=amz/crc64nvme"
	b.SetBytes(benchSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := map[string][]string{"Etag": {"x"}, "X-Amz-Checksum-Crc64nvme": {"y"}}
		Apply(note, h, body, body)
	}
}
