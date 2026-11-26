package http

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const (
	storeMocksBatchMaxMocks = 500
	storeMocksBatchMaxBytes = 8 << 20 // 8 MiB uncompressed target per batch
	storeMocksBatchTimeout  = 120 * time.Second
	storeMocksAbortTimeout  = 5 * time.Second
	storeMocksMaxRetries    = 5
	storeMocksMaxRestarts   = 3
)

// storeMocksRetryBaseDelay is the base exponential backoff delay between
// transient batch retries (capped at storeMocksRetryMaxDelay). Variable so
// unit tests can run retry scenarios without sleeping seconds.
var (
	storeMocksRetryBaseDelay = 250 * time.Millisecond
	storeMocksRetryMaxDelay  = 4 * time.Second
)

type storeMocksCaps struct {
	encoding   string
	batched    bool
	instanceID string
}

// probeStoreMocksCaps queries GET /agent/health once to learn whether the
// agent supports zstd compression and the batched-v1 storemocks protocol, along
// with its process instance ID.
func (a *AgentClient) probeStoreMocksCaps(ctx context.Context) storeMocksCaps {
	pctx, cancel := context.WithTimeout(ctx, storeMocksEncodingProbeTimeout)
	defer cancel()
	req, err := newAgentRequest(pctx, http.MethodGet, fmt.Sprintf("%s/health", a.conf.AgentURI()), nil)
	if err != nil {
		return storeMocksCaps{}
	}
	res, err := doAgent(&a.client, req)
	if err != nil {
		a.logger.Debug("could not probe agent storemocks capabilities; falling back to legacy uncompressed stream", zap.Error(err))
		return storeMocksCaps{}
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		return storeMocksCaps{}
	}
	var caps storeMocksCaps
	if acceptsEncoding(res.Header.Values("Accept-Encoding"), models.MockStreamEncodingZstd) {
		caps.encoding = models.MockStreamEncodingZstd
	}
	if strings.EqualFold(strings.TrimSpace(res.Header.Get(models.StoreMocksProtocolHeader)), models.StoreMocksProtocolBatchedV1) {
		caps.batched = true
	}
	caps.instanceID = res.Header.Get(models.AgentInstanceHeader)
	return caps
}

// estimateMockBytes returns an approximate uncompressed byte size for m so
// batches stay bounded to <= storeMocksBatchMaxBytes without reading payloads
// off disk ahead of time.
func estimateMockBytes(m *models.Mock) int {
	if m == nil {
		return 0
	}
	if n := m.PayloadDiskBytes(); n > 0 {
		return n
	}
	est := 512 + len(m.Name)
	if m.Spec.HTTPReq != nil {
		est += len(m.Spec.HTTPReq.URL) + len(m.Spec.HTTPReq.Body)
	}
	if m.Spec.HTTPResp != nil {
		est += len(m.Spec.HTTPResp.Body)
	}
	if m.Spec.GRPCReq != nil {
		est += len(m.Spec.GRPCReq.Body.DecodedData)
	}
	if m.Spec.GRPCResp != nil {
		est += len(m.Spec.GRPCResp.Body.DecodedData)
	}
	for i := range m.Spec.GenericRequests {
		for j := range m.Spec.GenericRequests[i].Message {
			est += len(m.Spec.GenericRequests[i].Message[j].Data)
		}
	}
	for i := range m.Spec.GenericResponses {
		for j := range m.Spec.GenericResponses[i].Message {
			est += len(m.Spec.GenericResponses[i].Message[j].Data)
		}
	}
	est += len(m.Spec.MongoRequests) * 1024
	est += len(m.Spec.MongoResponses) * 1024
	est += len(m.Spec.MySQLRequests) * 512
	est += len(m.Spec.MySQLResponses) * 512
	return est
}

// nextStoreMocksBatch returns the prefix of remaining to send in the next
// batch: at least 1 mock, at most storeMocksBatchMaxMocks mocks, and bounded
// by storeMocksBatchMaxBytes estimated uncompressed bytes.
func nextStoreMocksBatch(remaining []*models.Mock) []*models.Mock {
	if len(remaining) == 0 {
		return nil
	}
	limit := min(len(remaining), storeMocksBatchMaxMocks)
	totalBytes := 0
	n := 0
	for n < limit {
		sz := estimateMockBytes(remaining[n])
		if n > 0 && totalBytes+sz > storeMocksBatchMaxBytes {
			break
		}
		totalBytes += sz
		n++
	}
	return remaining[:n]
}

// encodeStoreMocksBatch encodes MockBatchHeader followed by mocks as a gob
// stream, optionally zstd-compressed.
func encodeStoreMocksBatch(seq int, mocks []*models.Mock, encoding string) ([]byte, int64, error) {
	var wireBuf bytes.Buffer
	var rawBytes atomic.Int64
	var out io.Writer = &wireBuf
	var zw *zstd.Encoder
	if encoding == models.MockStreamEncodingZstd {
		var err error
		zw, err = zstd.NewWriter(&wireBuf,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithWindowSize(models.MockStreamZstdWindow),
			zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, 0, fmt.Errorf("start zstd encoder for batch %d: %w", seq, err)
		}
		out = zw
	}
	enc := gob.NewEncoder(countingWriter{w: out, n: &rawBytes})
	if err := enc.Encode(models.MockBatchHeader{Seq: seq, Count: len(mocks)}); err != nil {
		if zw != nil {
			_ = zw.Close()
		}
		return nil, 0, fmt.Errorf("encode batch %d header: %w", seq, err)
	}
	for i, m := range mocks {
		if err := enc.Encode(m); err != nil {
			if zw != nil {
				_ = zw.Close()
			}
			return nil, 0, fmt.Errorf("encode batch %d mock %d/%d: %w", seq, i+1, len(mocks), err)
		}
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return nil, 0, fmt.Errorf("finish zstd frame for batch %d: %w", seq, err)
		}
	}
	return wireBuf.Bytes(), rawBytes.Load(), nil
}

// isTransientStoreMocksErr reports whether err is a transport drop/reset or
// per-request timeout worth retrying against the same staging session.
func isTransientStoreMocksErr(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection reset by peer",
		"broken pipe",
		"unexpected eof",
		"eof",
		"use of closed network connection",
		"connection refused",
		"i/o timeout",
		"client connection lost",
		"server sent goaway",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func sleepStoreMocksBackoff(ctx context.Context, attempt int) error {
	delay := storeMocksRetryBaseDelay
	for i := 0; i < attempt && delay < storeMocksRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > storeMocksRetryMaxDelay {
		delay = storeMocksRetryMaxDelay
	}
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *AgentClient) storeMocksBatched(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock, caps storeMocksCaps) error {
	start := time.Now()
	all := make([]*models.Mock, 0, len(filtered)+len(unFiltered))
	all = append(all, filtered...)
	all = append(all, unFiltered...)

	var lastRestartReason string
	for restart := 0; restart < storeMocksMaxRestarts; restart++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if restart > 0 {
			a.logger.Warn("restarting batched storemocks upload from begin",
				zap.Int("restartAttempt", restart),
				zap.String("reason", lastRestartReason))
			if err := sleepStoreMocksBackoff(ctx, restart-1); err != nil {
				return err
			}
		}

		stagingID, instanceID, err := a.beginStoreMocks(ctx, len(filtered), len(unFiltered))
		if err != nil {
			return err
		}
		if instanceID == "" {
			instanceID = caps.instanceID
		}

		var totalRaw, totalWire, totalReadBack int64
		var batchesSent, retriesUsed int
		needRestart := false

		for pos, seq := 0, 0; pos < len(all); seq++ {
			if err := ctx.Err(); err != nil {
				a.abortStoreMocksBestEffort(stagingID)
				return err
			}
			chunk := nextStoreMocksBatch(all[pos:])
			hydrated, rb, err := models.WithPayloads(ctx, chunk)
			if rb > 0 {
				totalReadBack += rb
			}
			if err != nil {
				a.abortStoreMocksBestEffort(stagingID)
				return fmt.Errorf("failed to send the mocks to the agent: %w", err)
			}

			wireBytes, rawBytes, err := encodeStoreMocksBatch(seq, hydrated, caps.encoding)
			if err != nil {
				a.abortStoreMocksBestEffort(stagingID)
				return fmt.Errorf("failed to send the mocks to the agent: %w", err)
			}

			restartReason, retries, err := a.sendStoreMocksBatch(ctx, stagingID, seq, wireBytes, caps.encoding, instanceID)
			retriesUsed += retries
			if restartReason != "" {
				lastRestartReason = restartReason
				needRestart = true
				break
			}
			if err != nil {
				a.abortStoreMocksBestEffort(stagingID)
				return err
			}

			totalRaw += rawBytes
			totalWire += int64(len(wireBytes))
			batchesSent++
			pos += len(chunk)
		}

		if needRestart {
			continue
		}

		concurrent, restartReason, retries, err := a.commitStoreMocks(ctx, stagingID, instanceID)
		retriesUsed += retries
		if restartReason != "" {
			lastRestartReason = restartReason
			continue
		}
		if err != nil {
			a.abortStoreMocksBestEffort(stagingID)
			return err
		}

		a.concurrentServing.Store(concurrent)
		a.logger.Info("stored the mocks on the agent",
			zap.String("protocol", models.StoreMocksProtocolBatchedV1),
			zap.Int("mocks", len(all)),
			zap.Int("batches", batchesSent),
			zap.Int("retries", retriesUsed),
			zap.Int("restarts", restart),
			zap.Int64("payloadsReadBack", totalReadBack),
			zap.Int64("bytes", totalRaw),
			zap.Int64("sentBytes", totalWire),
			zap.String("encoding", encodingName(caps.encoding)),
			zap.Duration("took", time.Since(start)))
		return nil
	}

	return fmt.Errorf("storemocks failed after %d staging restarts: %s", storeMocksMaxRestarts, lastRestartReason)
}

func (a *AgentClient) beginStoreMocks(ctx context.Context, filteredCount, unfilteredCount int) (string, string, error) {
	bodyBytes, err := json.Marshal(models.StoreMocksBeginReq{
		FilteredCount:   filteredCount,
		UnfilteredCount: unfilteredCount,
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal storemocks begin request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/storemocks/begin", a.conf.AgentURI())

	for attempt := 0; attempt <= storeMocksMaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepStoreMocksBackoff(ctx, attempt-1); err != nil {
				return "", "", err
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, storeMocksBatchTimeout)
		req, err := newAgentRequest(reqCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			return "", "", fmt.Errorf("create request for storemocks begin: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := doAgent(&a.client, req)
		if err != nil {
			cancel()
			if isTransientStoreMocksErr(ctx, err) && attempt < storeMocksMaxRetries {
				a.logger.Warn("retrying storemocks begin after transport error",
					zap.Int("attempt", attempt+1), zap.Error(err))
				continue
			}
			return "", "", fmt.Errorf("send request for storemocks begin: %w", err)
		}

		respBody, readErr := io.ReadAll(io.LimitReader(res.Body, maxStoreMocksFailBody+1))
		_ = res.Body.Close()
		cancel()
		if readErr != nil {
			if isTransientStoreMocksErr(ctx, readErr) && attempt < storeMocksMaxRetries {
				continue
			}
			return "", "", fmt.Errorf("read storemocks begin response: %w", readErr)
		}

		var resp models.StoreMocksBeginResp
		if decErr := json.Unmarshal(respBody, &resp); decErr != nil {
			return "", "", fmt.Errorf("storemocks begin http %d: decode response: %w", res.StatusCode, decErr)
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 || !resp.IsSuccess {
			if resp.ErrorMsg != "" {
				return "", "", fmt.Errorf("storemocks begin failed (status %d): %s", res.StatusCode, resp.ErrorMsg)
			}
			return "", "", fmt.Errorf("storemocks begin failed (status %d)", res.StatusCode)
		}
		if resp.StagingID == "" {
			return "", "", errors.New("storemocks begin succeeded without returning a stagingId")
		}
		return resp.StagingID, res.Header.Get(models.AgentInstanceHeader), nil
	}
	return "", "", errors.New("storemocks begin exhausted retries")
}

func (a *AgentClient) sendStoreMocksBatch(ctx context.Context, stagingID string, seq int, wireBytes []byte, encoding string, expectedInstance string) (restartReason string, retries int, err error) {
	endpoint := fmt.Sprintf("%s/storemocks/batch?id=%s&seq=%d",
		a.conf.AgentURI(), url.QueryEscape(stagingID), seq)

	for attempt := 0; attempt <= storeMocksMaxRetries; attempt++ {
		if attempt > 0 {
			retries++
			if err := sleepStoreMocksBackoff(ctx, attempt-1); err != nil {
				return "", retries, err
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, storeMocksBatchTimeout)
		req, err := newAgentRequest(reqCtx, http.MethodPost, endpoint, bytes.NewReader(wireBytes))
		if err != nil {
			cancel()
			return "", retries, fmt.Errorf("create request for storemocks batch %d: %w", seq, err)
		}
		req.ContentLength = int64(len(wireBytes))
		req.Header.Set("Content-Type", models.StoreMocksStreamContentType)
		req.Header.Set("Accept", "application/json")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}

		res, err := doAgent(&a.client, req)
		if err != nil {
			cancel()
			if isTransientStoreMocksErr(ctx, err) && attempt < storeMocksMaxRetries {
				a.logger.Warn("retrying storemocks batch after transport error",
					zap.Int("seq", seq), zap.Int("attempt", attempt+1), zap.Error(err))
				continue
			}
			return "", retries, fmt.Errorf("send request for storemocks batch %d: %w", seq, err)
		}

		respInstance := res.Header.Get(models.AgentInstanceHeader)
		respBody, readErr := io.ReadAll(io.LimitReader(res.Body, maxStoreMocksFailBody+1))
		_ = res.Body.Close()
		cancel()

		if readErr != nil {
			if isTransientStoreMocksErr(ctx, readErr) && attempt < storeMocksMaxRetries {
				a.logger.Warn("retrying storemocks batch after response read error",
					zap.Int("seq", seq), zap.Int("attempt", attempt+1), zap.Error(readErr))
				continue
			}
			return "", retries, fmt.Errorf("read storemocks batch %d response: %w", seq, readErr)
		}

		if expectedInstance != "" && respInstance != "" && respInstance != expectedInstance {
			return fmt.Sprintf("agent instance changed from %q to %q during batch %d", expectedInstance, respInstance, seq), retries, nil
		}
		if res.StatusCode == http.StatusNotFound {
			return fmt.Sprintf("staging session %q lost (404) at batch %d", stagingID, seq), retries, nil
		}

		var resp models.StoreMocksBatchResp
		decErr := json.Unmarshal(respBody, &resp)
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if decErr != nil {
				return "", retries, fmt.Errorf("decode storemocks batch %d response: %w", seq, decErr)
			}
			if !resp.IsSuccess {
				if resp.ErrorMsg != "" {
					return "", retries, fmt.Errorf("storemocks batch %d failed: %s", seq, resp.ErrorMsg)
				}
				return "", retries, fmt.Errorf("storemocks batch %d failed with status %d and no message", seq, res.StatusCode)
			}
			return "", retries, nil
		}

		if decErr == nil && resp.ErrorMsg != "" {
			return "", retries, fmt.Errorf("storemocks batch %d failed (status %d): %s", seq, res.StatusCode, resp.ErrorMsg)
		}
		return "", retries, fmt.Errorf("storemocks batch %d failed (status %d): %s", seq, res.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return "", retries, fmt.Errorf("storemocks batch %d exhausted retries", seq)
}

func (a *AgentClient) commitStoreMocks(ctx context.Context, stagingID string, expectedInstance string) (concurrent bool, restartReason string, retries int, err error) {
	endpoint := fmt.Sprintf("%s/storemocks/commit?id=%s", a.conf.AgentURI(), url.QueryEscape(stagingID))

	for attempt := 0; attempt <= storeMocksMaxRetries; attempt++ {
		if attempt > 0 {
			retries++
			if err := sleepStoreMocksBackoff(ctx, attempt-1); err != nil {
				return false, "", retries, err
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, storeMocksBatchTimeout)
		req, err := newAgentRequest(reqCtx, http.MethodPost, endpoint, nil)
		if err != nil {
			cancel()
			return false, "", retries, fmt.Errorf("create request for storemocks commit: %w", err)
		}
		req.Header.Set("Accept", "application/json")

		res, err := doAgent(&a.client, req)
		if err != nil {
			cancel()
			if isTransientStoreMocksErr(ctx, err) && attempt < storeMocksMaxRetries {
				a.logger.Warn("retrying storemocks commit after transport error",
					zap.Int("attempt", attempt+1), zap.Error(err))
				continue
			}
			return false, "", retries, fmt.Errorf("send request for storemocks commit: %w", err)
		}

		respInstance := res.Header.Get(models.AgentInstanceHeader)
		exactConcurrent := res.Header.Get(models.ExactConcurrentHeader) == models.ExactConcurrentServes
		respBody, readErr := io.ReadAll(io.LimitReader(res.Body, maxStoreMocksFailBody+1))
		_ = res.Body.Close()
		cancel()

		if readErr != nil {
			if isTransientStoreMocksErr(ctx, readErr) && attempt < storeMocksMaxRetries {
				continue
			}
			return false, "", retries, fmt.Errorf("read storemocks commit response: %w", readErr)
		}

		if expectedInstance != "" && respInstance != "" && respInstance != expectedInstance {
			return false, fmt.Sprintf("agent instance changed from %q to %q during commit", expectedInstance, respInstance), retries, nil
		}
		if res.StatusCode == http.StatusNotFound {
			return false, fmt.Sprintf("staging session %q lost (404) at commit", stagingID), retries, nil
		}

		var resp models.AgentResp
		decErr := json.Unmarshal(respBody, &resp)
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if decErr != nil {
				return false, "", retries, fmt.Errorf("decode storemocks commit response: %w", decErr)
			}
			if !resp.IsSuccess {
				if resp.ErrorMsg != "" {
					return false, "", retries, fmt.Errorf("storemocks commit failed: %s", resp.ErrorMsg)
				}
				return false, "", retries, fmt.Errorf("storemocks commit failed with status %d and no message", res.StatusCode)
			}
			return exactConcurrent, "", retries, nil
		}

		if decErr == nil && resp.ErrorMsg != "" {
			return false, "", retries, fmt.Errorf("storemocks commit failed (status %d): %s", res.StatusCode, resp.ErrorMsg)
		}
		return false, "", retries, fmt.Errorf("storemocks commit failed (status %d): %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return false, "", retries, errors.New("storemocks commit exhausted retries")
}

func (a *AgentClient) abortStoreMocksBestEffort(stagingID string) {
	if stagingID == "" {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.Background(), storeMocksAbortTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/storemocks/abort?id=%s", a.conf.AgentURI(), url.QueryEscape(stagingID))
	req, err := newAgentRequest(abortCtx, http.MethodPost, endpoint, nil)
	if err != nil {
		return
	}
	res, err := doAgent(&a.client, req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	_ = res.Body.Close()
}
