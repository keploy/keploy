package routes

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// streamSvc decodes a /storemocks stream mock by mock, as the agent service
// does, and keeps the names so a test can check what arrived.
type streamSvc struct {
	agent.Service // nil: any call other than StoreMocksStream panics loudly
	calls         int
	names         []string
}

func (s *streamSvc) StoreMocksStream(_ context.Context, h models.MockStreamHeader, dec *gob.Decoder) error {
	s.calls++
	for i := 0; i < h.FilteredCount+h.UnfilteredCount; i++ {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			return err
		}
		s.names = append(s.names, m.Name)
	}
	return nil
}

func newStreamAgent(t *testing.T) (*Agent, *streamSvc) {
	svc := &streamSvc{}
	return &Agent{logger: zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)), svc: svc}, svc
}

// mockNames is n mock names. With mockStream's bodies they make a stream larger
// than one zstd block, so the encoder writes its frame header before it knows
// the size and declares its whole window.
func mockNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("mock-%d", i)
	}
	return names
}

func mockStream(t *testing.T, w io.Writer, names []string) {
	t.Helper()
	enc := gob.NewEncoder(w)
	require.NoError(t, enc.Encode(models.MockStreamHeader{FilteredCount: len(names)}))
	for _, n := range names {
		require.NoError(t, enc.Encode(&models.Mock{
			Name: n,
			Kind: models.HTTP,
			Spec: models.MockSpec{HTTPResp: &models.HTTPResp{StatusCode: 200, Body: strings.Repeat(`{"id":1,"ok":true}`, 64)}},
		}))
	}
}

func zstdStream(t *testing.T, window int, names []string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithWindowSize(window))
	require.NoError(t, err)
	mockStream(t, zw, names)
	require.NoError(t, zw.Close())
	return &buf
}

func postStoreMocks(a *Agent, body io.Reader, encoding string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/agent/storemocks", body)
	req.Header.Set("Content-Type", models.StoreMocksStreamContentType)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	a.StoreMocks(rec, req)
	return rec
}

// The client compresses only for an agent whose /health says it can decompress,
// so without this header every upload stays uncompressed.
func TestHealthSaysStoreMocksTakesZstd(t *testing.T) {
	a, _ := newStreamAgent(t)
	rec := httptest.NewRecorder()
	a.Health(rec, httptest.NewRequest(http.MethodGet, "/agent/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, models.MockStreamEncodingZstd, rec.Header().Get("Accept-Encoding"))
}

func TestStoreMocksReadsAZstdStream(t *testing.T) {
	a, svc := newStreamAgent(t)
	names := mockNames(300)
	rec := postStoreMocks(a, zstdStream(t, models.MockStreamZstdWindow, names), models.MockStreamEncodingZstd)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, names, svc.names)
}

// An older client never compresses, and must keep working against this agent.
func TestStoreMocksStillReadsAnUncompressedStream(t *testing.T) {
	a, svc := newStreamAgent(t)
	names := mockNames(3)
	var body bytes.Buffer
	mockStream(t, &body, names)
	rec := postStoreMocks(a, &body, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, names, svc.names)
}

func TestStoreMocksRefusesAnEncodingItCannotRead(t *testing.T) {
	a, svc := newStreamAgent(t)
	rec := postStoreMocks(a, strings.NewReader("not gob"), "br")
	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	require.Equal(t, models.MockStreamEncodingZstd, rec.Header().Get("Accept-Encoding"),
		"a 415 names the encoding the agent does take (RFC 7694)")
	require.Zero(t, svc.calls)
}

// The window is what a zstd stream makes its reader allocate, so the agent
// refuses one larger than the client ever uses rather than allocate it.
func TestStoreMocksRefusesAZstdWindowAboveTheLimit(t *testing.T) {
	a, svc := newStreamAgent(t)
	rec := postStoreMocks(a, zstdStream(t, 4*models.MockStreamZstdWindow, mockNames(300)), models.MockStreamEncodingZstd)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "window")
	require.Zero(t, svc.calls)
}
