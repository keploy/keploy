package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
)

// zeroReader yields an endless stream of zero bytes — the shape of a
// decompression bomb (tiny compressed, huge inflated).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestDownload_GzipBombCapped pins the mock-download cap (#3867): a server
// response inflating past maxMockDownloadBytes must fail the read with
// pkg.ErrDecompressedTooLarge instead of streaming unbounded data. The
// consumer reads through io.Copy(io.Discard, ...) so the test itself stays
// at constant memory.
func TestDownload_GzipBombCapped(t *testing.T) {
	// Compresses maxMockDownloadBytes+64KiB of zeros to ~1MB, then appends
	// an incompressible 8MiB random tail INSIDE the same gzip stream. The
	// cap fires while that tail is still unread on the wire, so the
	// connection is genuinely half-read — letting the test distinguish a
	// released connection from a leaked one (an all-zeros bomb is consumed
	// to EOF by flate's read-ahead before the cap error surfaces, and a
	// fully-read body is pooled, not closed).
	var bomb bytes.Buffer
	gw, err := gzip.NewWriterLevel(&bomb, gzip.BestSpeed)
	require.NoError(t, err)
	_, err = io.CopyN(gw, zeroReader{}, maxMockDownloadBytes+64*1024)
	require.NoError(t, err)
	_, err = io.CopyN(gw, rand.Reader, 8*1024*1024)
	require.NoError(t, err)
	require.NoError(t, gw.Close())

	// Observe actual connection closure, not just a nil Close() — closing
	// only the gzip reader (not resp.Body) would still return nil.
	connClosed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(bomb.Bytes())
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			connClosed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()

	s := New(server.URL, zap.NewNop())
	r, err := s.Download(context.Background(), "mock", "app", "user", "token")
	require.NoError(t, err)

	_, err = io.Copy(io.Discard, r)
	require.ErrorIs(t, err, pkg.ErrDecompressedTooLarge)

	// The abandoned half-read connection must actually be released: the
	// gzip path returns a ReadCloser whose Close closes resp.Body, and a
	// half-read connection cannot be reused, so the transport must close
	// it — observed as the server's ConnState reaching StateClosed.
	rc, ok := r.(io.Closer)
	require.True(t, ok, "gzip Download path must return an io.Closer")
	require.NoError(t, rc.Close())
	select {
	case <-connClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP connection not closed after Close() — resp.Body is leaking")
	}
}

// TestDownload_GzipSmallPayload verifies a legitimate gzipped download still
// decompresses transparently and completely.
func TestDownload_GzipSmallPayload(t *testing.T) {
	payload := []byte(`{"mocks":"content"}`)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(payload)
	require.NoError(t, err)
	require.NoError(t, gw.Close())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer server.Close()

	s := New(server.URL, zap.NewNop())
	r, err := s.Download(context.Background(), "mock", "app", "user", "token")
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	rc, ok := r.(io.Closer)
	require.True(t, ok, "gzip Download path must return an io.Closer")
	require.NoError(t, rc.Close())
}
