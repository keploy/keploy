package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ReplayResult holds the results of replaying a captured connection.
type ReplayResult struct {
	ConnectionID   uint64         `json:"connection_id"`
	Protocol       Protocol       `json:"protocol"`
	SrcAddr        string         `json:"src_addr"`
	DstAddr        string         `json:"dst_addr"`
	PacketsSent    int            `json:"packets_sent"`
	PacketsRecv    int            `json:"packets_recv"`
	BytesSent      int64          `json:"bytes_sent"`
	BytesRecv      int64          `json:"bytes_recv"`
	Matched        bool           `json:"matched"`
	Errors         []string       `json:"errors,omitempty"`
	Duration       time.Duration  `json:"duration"`
	ByteMismatches []ByteMismatch `json:"byte_mismatches,omitempty"`
}

// ByteMismatch records where replayed traffic differed from captured traffic.
type ByteMismatch struct {
	PacketIndex int    `json:"packet_index"`
	Direction   string `json:"direction"`
	Expected    int    `json:"expected_bytes"`
	Actual      int    `json:"actual_bytes"`
	Offset      int    `json:"first_diff_offset"` // byte offset of first difference; if contents match up to the shorter length and only lengths differ, this equals min(Expected, Actual)
}

// ReplaySummary aggregates results across all replayed connections.
type ReplaySummary struct {
	CaptureFile   string          `json:"capture_file"`
	TotalConns    int             `json:"total_connections"`
	ReplayedConns int             `json:"replayed_connections"`
	MatchedConns  int             `json:"matched_connections"`
	FailedConns   int             `json:"failed_connections"`
	SkippedConns  int             `json:"skipped_connections"`
	TotalDuration time.Duration   `json:"total_duration"`
	Results       []*ReplayResult `json:"results"`
}

// Replayer replays captured network traffic against a running proxy.
type Replayer struct {
	logger    *zap.Logger
	proxyAddr string // address of the proxy to replay against
	timeout   time.Duration
}

// NewReplayer creates a new replay engine.
func NewReplayer(logger *zap.Logger, proxyAddr string, timeout time.Duration) *Replayer {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Replayer{
		logger:    logger,
		proxyAddr: proxyAddr,
		timeout:   timeout,
	}
}

// ReplayFile replays all connections from a capture file.
func (r *Replayer) ReplayFile(ctx context.Context, path string) (*ReplaySummary, error) {
	reader, err := NewReader(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open capture file: %w", err)
	}
	defer reader.Close()

	cf, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to read capture file: %w", err)
	}

	return r.ReplayCaptureFile(ctx, cf, path)
}

// ReplayCaptureFile replays all connections from a parsed capture file.
func (r *Replayer) ReplayCaptureFile(ctx context.Context, cf *CaptureFile, path string) (*ReplaySummary, error) {
	conns := cf.GetConnections()

	summary := &ReplaySummary{
		CaptureFile: path,
		TotalConns:  len(conns),
	}

	start := time.Now()

	// Sort connections by open time so replay starts them in capture order,
	// though individual connections are replayed concurrently.
	sortedConns := make([]*ConnectionTimeline, 0, len(conns))
	for _, ct := range conns {
		sortedConns = append(sortedConns, ct)
	}
	sort.Slice(sortedConns, func(i, j int) bool {
		return sortedConns[i].OpenedAt.Before(sortedConns[j].OpenedAt)
	})

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, ct := range sortedConns {
		if ctx.Err() != nil {
			break
		}

		// Skip connections without data packets
		if len(ct.Packets) == 0 {
			mu.Lock()
			summary.SkippedConns++
			mu.Unlock()
			continue
		}

		// NOTE: The isTLS flag indicates the original transport was encrypted,
		// but the captured bytes are already decrypted plaintext (the proxy does
		// MITM TLS termination before the capture point). So TLS connections
		// contain the same application-layer data as non-TLS connections and
		// can be replayed, compared, and analyzed normally.

		wg.Add(1)
		go func(ct *ConnectionTimeline) {
			defer wg.Done()

			result := r.replayConnection(ctx, ct)

			mu.Lock()
			summary.Results = append(summary.Results, result)
			summary.ReplayedConns++
			if result.Matched {
				summary.MatchedConns++
			} else {
				summary.FailedConns++
			}
			mu.Unlock()
		}(ct)
	}

	wg.Wait()
	summary.TotalDuration = time.Since(start)

	return summary, nil
}

// replayConnection replays a single captured connection.
func (r *Replayer) replayConnection(ctx context.Context, ct *ConnectionTimeline) *ReplayResult {
	result := &ReplayResult{
		ConnectionID: ct.ConnectionID,
		Protocol:     ct.Protocol,
		SrcAddr:      ct.SrcAddr,
		DstAddr:      ct.DstAddr,
		Matched:      true,
	}

	start := time.Now()
	defer func() {
		result.Duration = time.Since(start)
	}()

	// Connect to the proxy (or destination for validation)
	dialCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", r.proxyAddr)
	if err != nil {
		result.Matched = false
		result.Errors = append(result.Errors, fmt.Sprintf("failed to connect to %s: %s", r.proxyAddr, err))
		return result
	}
	defer conn.Close()

	// Replay packets in order
	for i, pkt := range ct.Packets {
		if ctx.Err() != nil {
			result.Errors = append(result.Errors, "context cancelled during replay")
			result.Matched = false
			return result
		}

		switch pkt.Direction {
		case DirProxyToDest, DirDestToProxy:
			// These packets record the proxy↔destination leg of the connection
			// (used for analysis/compare). During replay we only exercise the
			// client↔proxy leg, so skip them rather than silently counting them
			// as matched.
			continue

		case DirClientToProxy:
			// Send the captured client data
			if err := conn.SetWriteDeadline(time.Now().Add(r.timeout)); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("packet %d: set write deadline: %s", i, err))
				result.Matched = false
				continue
			}

			n, err := conn.Write(pkt.Payload)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("packet %d: write failed: %s", i, err))
				result.Matched = false
				continue
			}
			result.PacketsSent++
			result.BytesSent += int64(n)

		case DirProxyToClient:
			// Read and compare the proxy's response.
			// TCP may deliver the response in multiple segments, so we loop
			// until we have at least as many bytes as expected or the deadline fires.
			if err := conn.SetReadDeadline(time.Now().Add(r.timeout)); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("packet %d: set read deadline: %s", i, err))
				result.Matched = false
				continue
			}

			want := len(pkt.Payload)
			buf := make([]byte, want)
			var n int
			for n < want {
				nr, err := conn.Read(buf[n:want])
				n += nr
				if err != nil {
					if !errors.Is(err, io.EOF) {
						result.Errors = append(result.Errors, fmt.Sprintf("packet %d: read failed: %s", i, err))
						result.Matched = false
					}
					break
				}
			}
			result.PacketsRecv++
			result.BytesRecv += int64(n)

			// Compare the response
			if n != len(pkt.Payload) || !bytesEqual(buf[:n], pkt.Payload) {
				result.Matched = false
				offset := findFirstDiff(buf[:n], pkt.Payload)
				result.ByteMismatches = append(result.ByteMismatches, ByteMismatch{
					PacketIndex: i,
					Direction:   pkt.Direction.String(),
					Expected:    len(pkt.Payload),
					Actual:      n,
					Offset:      offset,
				})
			}
		}
	}

	return result
}

// bytesEqual compares two byte slices for equality.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// findFirstDiff finds the byte offset of the first difference between two slices.
// Returns -1 if they differ only in length.
func findFirstDiff(actual, expected []byte) int {
	minLen := len(actual)
	if len(expected) < minLen {
		minLen = len(expected)
	}
	for i := 0; i < minLen; i++ {
		if actual[i] != expected[i] {
			return i
		}
	}
	if len(actual) != len(expected) {
		return minLen
	}
	return -1
}
