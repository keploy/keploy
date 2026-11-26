package routes

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

func encodeBatchBody(t *testing.T, seq int, names []string, compress bool) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var out = &buf
	var zw *zstd.Encoder
	var err error
	if compress {
		zw, err = zstd.NewWriter(&buf, zstd.WithWindowSize(models.MockStreamZstdWindow))
		require.NoError(t, err)
		enc := gob.NewEncoder(zw)
		require.NoError(t, enc.Encode(models.MockBatchHeader{Seq: seq, Count: len(names)}))
		for _, n := range names {
			require.NoError(t, enc.Encode(&models.Mock{Name: n, Kind: models.HTTP}))
		}
		require.NoError(t, zw.Close())
		return out
	}
	enc := gob.NewEncoder(&buf)
	require.NoError(t, enc.Encode(models.MockBatchHeader{Seq: seq, Count: len(names)}))
	for _, n := range names {
		require.NoError(t, enc.Encode(&models.Mock{Name: n, Kind: models.HTTP}))
	}
	return &buf
}

func TestStoreMocksBatchedRoutes_LifecycleAndStatusCodes(t *testing.T) {
	ag := agent.New(zap.NewNop(), nil, nil, nil, nil, config.New())
	r := chi.NewRouter()
	DefaultRoutes{}.New(r, ag, zap.NewNop())

	// /agent/health advertises batched-v1 protocol; X-Keploy-Agent-Instance
	// is emitted only when the proven switch is on.
	hRec := httptest.NewRecorder()
	r.ServeHTTP(hRec, httptest.NewRequest(http.MethodGet, "/agent/health", nil))
	require.Equal(t, http.StatusOK, hRec.Code)
	require.Equal(t, models.StoreMocksProtocolBatchedV1, hRec.Header().Get(models.StoreMocksProtocolHeader))
	if provenswitch.On() {
		require.Equal(t, processInstance, hRec.Header().Get(models.AgentInstanceHeader))
	} else {
		require.Empty(t, hRec.Header().Get(models.AgentInstanceHeader))
	}

	// Begin staging session for 3 mocks.
	beginBody, _ := json.Marshal(models.StoreMocksBeginReq{FilteredCount: 2, UnfilteredCount: 1})
	bRec := httptest.NewRecorder()
	r.ServeHTTP(bRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/begin", bytes.NewReader(beginBody)))
	require.Equal(t, http.StatusOK, bRec.Code)
	var bResp models.StoreMocksBeginResp
	require.NoError(t, json.Unmarshal(bRec.Body.Bytes(), &bResp))
	require.True(t, bResp.IsSuccess)
	require.NotEmpty(t, bResp.StagingID)

	// Unsupported encoding returns 415 with Accept-Encoding: zstd.
	uReq := httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id="+bResp.StagingID+"&seq=0", bytes.NewReader([]byte("bad")))
	uReq.Header.Set("Content-Encoding", "br")
	uRec := httptest.NewRecorder()
	r.ServeHTTP(uRec, uReq)
	require.Equal(t, http.StatusUnsupportedMediaType, uRec.Code)
	require.Equal(t, models.MockStreamEncodingZstd, uRec.Header().Get("Accept-Encoding"))

	// Out-of-order seq=1 before seq=0 returns 409 Conflict with expectedSeq=0.
	cReq := httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id="+bResp.StagingID+"&seq=1", encodeBatchBody(t, 1, []string{"m3"}, true))
	cReq.Header.Set("Content-Encoding", models.MockStreamEncodingZstd)
	cRec := httptest.NewRecorder()
	r.ServeHTTP(cRec, cReq)
	require.Equal(t, http.StatusConflict, cRec.Code)
	var cResp models.StoreMocksBatchResp
	require.NoError(t, json.Unmarshal(cRec.Body.Bytes(), &cResp))
	require.False(t, cResp.IsSuccess)
	require.Equal(t, 0, cResp.ExpectedSeq)

	// Unknown staging ID returns 404.
	nfReq := httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id=no-such-id&seq=0", encodeBatchBody(t, 0, []string{"m1"}, false))
	nfRec := httptest.NewRecorder()
	r.ServeHTTP(nfRec, nfReq)
	require.Equal(t, http.StatusNotFound, nfRec.Code)

	// Valid batch seq=0 (zstd compressed, 2 mocks).
	ok0Req := httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id="+bResp.StagingID+"&seq=0", encodeBatchBody(t, 0, []string{"m1", "m2"}, true))
	ok0Req.Header.Set("Content-Encoding", models.MockStreamEncodingZstd)
	ok0Rec := httptest.NewRecorder()
	r.ServeHTTP(ok0Rec, ok0Req)
	require.Equal(t, http.StatusOK, ok0Rec.Code)

	// Idempotent replay of seq=0 returns 200 OK without duplicating.
	dup0Req := httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id="+bResp.StagingID+"&seq=0", encodeBatchBody(t, 0, []string{"m1", "m2"}, true))
	dup0Req.Header.Set("Content-Encoding", models.MockStreamEncodingZstd)
	dup0Rec := httptest.NewRecorder()
	r.ServeHTTP(dup0Rec, dup0Req)
	require.Equal(t, http.StatusOK, dup0Rec.Code)
	var dupResp models.StoreMocksBatchResp
	require.NoError(t, json.Unmarshal(dup0Rec.Body.Bytes(), &dupResp))
	require.Equal(t, 2, dupResp.Ingested)

	// Incomplete commit returns 409 Conflict.
	incRec := httptest.NewRecorder()
	r.ServeHTTP(incRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/commit?id="+bResp.StagingID, nil))
	require.Equal(t, http.StatusConflict, incRec.Code)

	// Batch seq=1 (uncompressed, 1 mock).
	ok1Rec := httptest.NewRecorder()
	r.ServeHTTP(ok1Rec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id="+bResp.StagingID+"&seq=1", encodeBatchBody(t, 1, []string{"m3"}, false)))
	require.Equal(t, http.StatusOK, ok1Rec.Code)

	// Commit succeeds.
	comRec := httptest.NewRecorder()
	r.ServeHTTP(comRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/commit?id="+bResp.StagingID, nil))
	require.Equal(t, http.StatusOK, comRec.Code)

	// Abort on already-committed/unknown ID is idempotent 200 OK.
	abRec := httptest.NewRecorder()
	r.ServeHTTP(abRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/abort?id="+bResp.StagingID, nil))
	require.Equal(t, http.StatusOK, abRec.Code)

	// Legacy single-stream POST /agent/storemocks still works against the same router.
	legacyRec := httptest.NewRecorder()
	legacyReq := httptest.NewRequest(http.MethodPost, "/agent/storemocks", zstdStream(t, models.MockStreamZstdWindow, []string{"legacy-1"}))
	legacyReq.Header.Set("Content-Type", models.StoreMocksStreamContentType)
	legacyReq.Header.Set("Content-Encoding", models.MockStreamEncodingZstd)
	r.ServeHTTP(legacyRec, legacyReq)
	require.Equal(t, http.StatusOK, legacyRec.Code)
}

// TestStoreMocksBatchedRoutes_AgentInstanceFollowsProvenSwitch guards the
// provenswitch contract enforced by agent-restart-docker.sh: /agent/health and
// all /agent/storemocks/{begin,batch,commit,abort} responses must omit
// X-Keploy-Agent-Instance when the proven switch is off and emit
// processInstance (via answersAs middleware) when it is on.
func TestStoreMocksBatchedRoutes_AgentInstanceFollowsProvenSwitch(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run(provenswitch.Env+"="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			want := ""
			if proven == "1" {
				want = processInstance
			}

			ag := agent.New(zap.NewNop(), nil, nil, nil, nil, config.New())
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, ag, zap.NewNop())

			hRec := httptest.NewRecorder()
			r.ServeHTTP(hRec, httptest.NewRequest(http.MethodGet, "/agent/health", nil))
			require.Equal(t, http.StatusOK, hRec.Code)
			require.Equal(t, models.StoreMocksProtocolBatchedV1, hRec.Header().Get(models.StoreMocksProtocolHeader))
			require.Equal(t, want, hRec.Header().Get(models.AgentInstanceHeader), "health")

			beginBody, _ := json.Marshal(models.StoreMocksBeginReq{FilteredCount: 1})
			bRec := httptest.NewRecorder()
			r.ServeHTTP(bRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/begin", bytes.NewReader(beginBody)))
			require.Equal(t, http.StatusOK, bRec.Code)
			require.Equal(t, want, bRec.Header().Get(models.AgentInstanceHeader), "begin")
			var bResp models.StoreMocksBeginResp
			require.NoError(t, json.Unmarshal(bRec.Body.Bytes(), &bResp))

			batchRec := httptest.NewRecorder()
			r.ServeHTTP(batchRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/batch?id="+bResp.StagingID+"&seq=0", encodeBatchBody(t, 0, []string{"m1"}, false)))
			require.Equal(t, http.StatusOK, batchRec.Code)
			require.Equal(t, want, batchRec.Header().Get(models.AgentInstanceHeader), "batch")

			comRec := httptest.NewRecorder()
			r.ServeHTTP(comRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/commit?id="+bResp.StagingID, nil))
			require.Equal(t, http.StatusOK, comRec.Code)
			require.Equal(t, want, comRec.Header().Get(models.AgentInstanceHeader), "commit")

			abRec := httptest.NewRecorder()
			r.ServeHTTP(abRec, httptest.NewRequest(http.MethodPost, "/agent/storemocks/abort?id="+bResp.StagingID, nil))
			require.Equal(t, http.StatusOK, abRec.Code)
			require.Equal(t, want, abRec.Header().Get(models.AgentInstanceHeader), "abort")
		})
	}
}
