package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
)

// stagedStoreMocksService is the optional service capability backing the
// chunked, resumable /agent/storemocks/{begin,batch,commit,abort} endpoints.
type stagedStoreMocksService interface {
	BeginStoreMocks(ctx context.Context, req models.StoreMocksBeginReq) (string, error)
	StoreMocksBatch(ctx context.Context, stagingID string, seq int, header models.MockBatchHeader, dec *gob.Decoder) (models.StoreMocksBatchResp, error)
	CommitStoreMocks(ctx context.Context, stagingID string) error
	AbortStoreMocks(ctx context.Context, stagingID string) error
}

func writeBatchedJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	if utils.Version != "" {
		w.Header().Set(models.AgentVersionHeader, utils.Version)
	}
	w.Header().Set("Content-Type", "application/json")
	render.Status(r, status)
	render.JSON(w, r, payload)
}

// StoreMocksBegin opens a staged mock upload session and returns its stagingID.
func (a *Agent) StoreMocksBegin(w http.ResponseWriter, r *http.Request) {
	svc, ok := a.svc.(stagedStoreMocksService)
	if !ok {
		writeBatchedJSON(w, r, http.StatusNotImplemented, models.StoreMocksBeginResp{
			ErrorMsg: "storemocks: service does not support batched uploads",
		})
		return
	}

	var req models.StoreMocksBeginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBatchedJSON(w, r, http.StatusBadRequest, models.StoreMocksBeginResp{
			ErrorMsg: fmt.Sprintf("storemocks begin: decode request: %v", err),
		})
		return
	}

	id, err := svc.BeginStoreMocks(r.Context(), req)
	if err != nil {
		writeBatchedJSON(w, r, http.StatusBadRequest, models.StoreMocksBeginResp{
			ErrorMsg: err.Error(),
		})
		return
	}

	writeBatchedJSON(w, r, http.StatusOK, models.StoreMocksBeginResp{
		IsSuccess: true,
		StagingID: id,
	})
}

// StoreMocksBatch receives one bounded zstd-compressed (or identity) gob batch
// identified by query parameters ?id=<stagingID>&seq=<k>.
func (a *Agent) StoreMocksBatch(w http.ResponseWriter, r *http.Request) {
	svc, ok := a.svc.(stagedStoreMocksService)
	if !ok {
		writeBatchedJSON(w, r, http.StatusNotImplemented, models.StoreMocksBatchResp{
			ErrorMsg: "storemocks: service does not support batched uploads",
		})
		return
	}

	stagingID := r.URL.Query().Get("id")
	if stagingID == "" {
		writeBatchedJSON(w, r, http.StatusBadRequest, models.StoreMocksBatchResp{
			ErrorMsg: "storemocks batch: missing id query parameter",
		})
		return
	}
	seqStr := r.URL.Query().Get("seq")
	seq, err := strconv.Atoi(seqStr)
	if err != nil || seq < 0 {
		writeBatchedJSON(w, r, http.StatusBadRequest, models.StoreMocksBatchResp{
			StagingID: stagingID,
			ErrorMsg:  fmt.Sprintf("storemocks batch: invalid seq %q", seqStr),
		})
		return
	}

	body, err := storeMocksBody(r)
	if err != nil {
		w.Header().Set("Accept-Encoding", models.MockStreamEncodingZstd)
		writeBatchedJSON(w, r, http.StatusUnsupportedMediaType, models.StoreMocksBatchResp{
			StagingID: stagingID,
			Seq:       seq,
			ErrorMsg:  err.Error(),
		})
		return
	}
	defer func() { _ = body.Close() }()

	dec := gob.NewDecoder(body)
	var header models.MockBatchHeader
	if err := dec.Decode(&header); err != nil {
		writeBatchedJSON(w, r, http.StatusBadRequest, models.StoreMocksBatchResp{
			StagingID: stagingID,
			Seq:       seq,
			ErrorMsg:  fmt.Sprintf("storemocks batch %d: decode batch header: %v", seq, err),
		})
		return
	}

	resp, err := svc.StoreMocksBatch(r.Context(), stagingID, seq, header, dec)
	// Drain any remaining unread bytes (for example on an idempotent duplicate
	// batch) so the HTTP/1.1 keep-alive connection stays clean instead of
	// resetting the client's socket.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 16<<20))
	if err != nil {
		resp.StagingID = stagingID
		resp.Seq = seq
		resp.IsSuccess = false
		resp.ErrorMsg = err.Error()

		var conflict *agent.StoreMocksSeqConflictError
		switch {
		case errors.Is(err, agent.ErrStoreMocksStageNotFound):
			writeBatchedJSON(w, r, http.StatusNotFound, resp)
		case errors.As(err, &conflict):
			resp.ExpectedSeq = conflict.ExpectedSeq
			writeBatchedJSON(w, r, http.StatusConflict, resp)
		default:
			writeBatchedJSON(w, r, http.StatusInternalServerError, resp)
		}
		return
	}

	writeBatchedJSON(w, r, http.StatusOK, resp)
}

// StoreMocksCommit finalizes and atomically publishes the staged mock corpus.
func (a *Agent) StoreMocksCommit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	svc, ok := a.svc.(stagedStoreMocksService)
	if !ok {
		respondAgent(w, r, http.StatusNotImplemented, errors.New("storemocks: service does not support batched uploads"))
		return
	}

	stagingID := r.URL.Query().Get("id")
	if stagingID == "" {
		respondAgent(w, r, http.StatusBadRequest, errors.New("storemocks commit: missing id query parameter"))
		return
	}

	if err := svc.CommitStoreMocks(r.Context(), stagingID); err != nil {
		switch {
		case errors.Is(err, agent.ErrStoreMocksStageNotFound):
			respondAgent(w, r, http.StatusNotFound, err)
		case errors.Is(err, agent.ErrStoreMocksIncomplete):
			respondAgent(w, r, http.StatusConflict, err)
		default:
			respondAgent(w, r, http.StatusInternalServerError, err)
		}
		return
	}

	if s, ok := a.svc.(interface{ ServesExactConcurrent() bool }); ok && provenswitch.On() && s.ServesExactConcurrent() {
		w.Header().Set(models.ExactConcurrentHeader, models.ExactConcurrentServes)
	}
	respondAgent(w, r, http.StatusOK, nil)
}

// StoreMocksAbort cleans up an active staging session (best-effort, idempotent).
func (a *Agent) StoreMocksAbort(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	svc, ok := a.svc.(stagedStoreMocksService)
	if !ok {
		respondAgent(w, r, http.StatusNotImplemented, errors.New("storemocks: service does not support batched uploads"))
		return
	}

	stagingID := r.URL.Query().Get("id")
	if err := svc.AbortStoreMocks(r.Context(), stagingID); err != nil {
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}
	respondAgent(w, r, http.StatusOK, nil)
}
