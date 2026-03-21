package capture

import (
	"fmt"
	"strings"
)

// CompareResult holds the result of comparing two capture files.
type CompareResult struct {
	FileA          string               `json:"file_a"`
	FileB          string               `json:"file_b"`
	ConnectionDiffs []ConnectionDiff    `json:"connection_diffs"`
	Summary        CompareSummary       `json:"summary"`
}

// CompareSummary gives high-level stats on the comparison.
type CompareSummary struct {
	ConnectionsOnlyInA int `json:"connections_only_in_a"`
	ConnectionsOnlyInB int `json:"connections_only_in_b"`
	ConnectionsInBoth  int `json:"connections_in_both"`
	MatchedConns       int `json:"matched_connections"`
	DiffConns          int `json:"differing_connections"`
}

// ConnectionDiff describes how a connection differs between two captures.
type ConnectionDiff struct {
	Protocol   Protocol `json:"protocol"`
	DstAddr    string   `json:"dst_addr"`
	Status     string   `json:"status"` // "match", "diff", "only_in_a", "only_in_b"
	PacketsA   int      `json:"packets_a"`
	PacketsB   int      `json:"packets_b"`
	BytesA     int64    `json:"bytes_a"`
	BytesB     int64    `json:"bytes_b"`
	Details    string   `json:"details,omitempty"`
}

// Compare two capture files and report differences.
// Connections are matched by destination address and protocol since connection IDs
// won't be the same across different runs.
func Compare(pathA, pathB string) (*CompareResult, error) {
	readerA, err := NewReader(pathA)
	if err != nil {
		return nil, fmt.Errorf("failed to read file A (%s): %w", pathA, err)
	}
	defer readerA.Close()

	readerB, err := NewReader(pathB)
	if err != nil {
		return nil, fmt.Errorf("failed to read file B (%s): %w", pathB, err)
	}
	defer readerB.Close()

	cfA, err := readerA.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse file A: %w", err)
	}

	cfB, err := readerB.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse file B: %w", err)
	}

	return CompareCaptureFiles(cfA, cfB, pathA, pathB), nil
}

// CompareCaptureFiles compares two parsed capture files.
func CompareCaptureFiles(cfA, cfB *CaptureFile, pathA, pathB string) *CompareResult {
	connsA := cfA.GetConnections()
	connsB := cfB.GetConnections()

	// Group connections by a key of "protocol:dstAddr" for matching
	groupA := groupByDestAndProto(connsA)
	groupB := groupByDestAndProto(connsB)

	result := &CompareResult{
		FileA: pathA,
		FileB: pathB,
	}

	seen := make(map[string]bool)

	// Check connections in A
	for key, ctListA := range groupA {
		seen[key] = true
		ctListB, inB := groupB[key]
		if !inB {
			// Only in A
			for _, ct := range ctListA {
				result.ConnectionDiffs = append(result.ConnectionDiffs, ConnectionDiff{
					Protocol: ct.Protocol,
					DstAddr:  ct.DstAddr,
					Status:   "only_in_a",
					PacketsA: len(ct.Packets),
					Details:  fmt.Sprintf("Connection to %s exists in customer capture but not in engineer's", ct.DstAddr),
				})
				result.Summary.ConnectionsOnlyInA++
			}
			continue
		}

		// Match connections: compare the i-th connection in each group
		maxLen := len(ctListA)
		if len(ctListB) > maxLen {
			maxLen = len(ctListB)
		}
		for i := 0; i < maxLen; i++ {
			if i >= len(ctListA) {
				result.ConnectionDiffs = append(result.ConnectionDiffs, ConnectionDiff{
					Protocol: ctListB[i].Protocol,
					DstAddr:  ctListB[i].DstAddr,
					Status:   "only_in_b",
					PacketsB: len(ctListB[i].Packets),
					Details:  fmt.Sprintf("Extra connection #%d to %s in engineer's capture", i+1, ctListB[i].DstAddr),
				})
				result.Summary.ConnectionsOnlyInB++
				continue
			}
			if i >= len(ctListB) {
				result.ConnectionDiffs = append(result.ConnectionDiffs, ConnectionDiff{
					Protocol: ctListA[i].Protocol,
					DstAddr:  ctListA[i].DstAddr,
					Status:   "only_in_a",
					PacketsA: len(ctListA[i].Packets),
					Details:  fmt.Sprintf("Extra connection #%d to %s in customer's capture", i+1, ctListA[i].DstAddr),
				})
				result.Summary.ConnectionsOnlyInA++
				continue
			}

			result.Summary.ConnectionsInBoth++
			diff := compareTimelines(ctListA[i], ctListB[i])
			result.ConnectionDiffs = append(result.ConnectionDiffs, diff)
			if diff.Status == "match" {
				result.Summary.MatchedConns++
			} else {
				result.Summary.DiffConns++
			}
		}
	}

	// Check connections only in B
	for key, ctListB := range groupB {
		if seen[key] {
			continue
		}
		for _, ct := range ctListB {
			result.ConnectionDiffs = append(result.ConnectionDiffs, ConnectionDiff{
				Protocol: ct.Protocol,
				DstAddr:  ct.DstAddr,
				Status:   "only_in_b",
				PacketsB: len(ct.Packets),
				Details:  fmt.Sprintf("Connection to %s exists in engineer's capture but not in customer's", ct.DstAddr),
			})
			result.Summary.ConnectionsOnlyInB++
		}
	}

	return result
}

// compareTimelines compares two connection timelines packet by packet.
func compareTimelines(a, b *ConnectionTimeline) ConnectionDiff {
	diff := ConnectionDiff{
		Protocol: a.Protocol,
		DstAddr:  a.DstAddr,
		PacketsA: len(a.Packets),
		PacketsB: len(b.Packets),
	}

	// Sum bytes
	for _, p := range a.Packets {
		diff.BytesA += int64(len(p.Payload))
	}
	for _, p := range b.Packets {
		diff.BytesB += int64(len(p.Payload))
	}

	// Compare packet counts
	if len(a.Packets) != len(b.Packets) {
		diff.Status = "diff"
		diff.Details = fmt.Sprintf("Packet count differs: %d vs %d", len(a.Packets), len(b.Packets))
		return diff
	}

	// Compare packet-by-packet (direction and payload size)
	for i := 0; i < len(a.Packets); i++ {
		pa, pb := a.Packets[i], b.Packets[i]
		if pa.Direction != pb.Direction {
			diff.Status = "diff"
			diff.Details = fmt.Sprintf("Packet %d direction differs: %s vs %s", i, pa.Direction, pb.Direction)
			return diff
		}
		if len(pa.Payload) != len(pb.Payload) {
			diff.Status = "diff"
			diff.Details = fmt.Sprintf("Packet %d size differs: %d vs %d bytes (direction: %s)", i, len(pa.Payload), len(pb.Payload), pa.Direction)
			return diff
		}
		// Compare actual bytes
		for j := 0; j < len(pa.Payload); j++ {
			if pa.Payload[j] != pb.Payload[j] {
				diff.Status = "diff"
				diff.Details = fmt.Sprintf("Packet %d byte diff at offset %d (direction: %s, size: %d)", i, j, pa.Direction, len(pa.Payload))
				return diff
			}
		}
	}

	// Check errors
	if len(a.Errors) != len(b.Errors) {
		diff.Status = "diff"
		diff.Details = fmt.Sprintf("Error count differs: %d vs %d", len(a.Errors), len(b.Errors))
		return diff
	}

	diff.Status = "match"
	return diff
}

func groupByDestAndProto(conns map[uint64]*ConnectionTimeline) map[string][]*ConnectionTimeline {
	groups := make(map[string][]*ConnectionTimeline)
	for _, ct := range conns {
		key := fmt.Sprintf("%s:%s", ct.Protocol, ct.DstAddr)
		groups[key] = append(groups[key], ct)
	}
	return groups
}

// FormatCompareResult formats a comparison result as human-readable text.
func FormatCompareResult(r *CompareResult) string {
	var sb strings.Builder

	sb.WriteString("═══════════════════════════════════════════════════\n")
	sb.WriteString("  Keploy Capture Comparison Report\n")
	sb.WriteString("═══════════════════════════════════════════════════\n\n")

	sb.WriteString(fmt.Sprintf("  File A (customer): %s\n", r.FileA))
	sb.WriteString(fmt.Sprintf("  File B (engineer): %s\n", r.FileB))
	sb.WriteString(fmt.Sprintf("\n  Connections matched:  %d\n", r.Summary.MatchedConns))
	sb.WriteString(fmt.Sprintf("  Connections differing: %d\n", r.Summary.DiffConns))
	sb.WriteString(fmt.Sprintf("  Only in customer:     %d\n", r.Summary.ConnectionsOnlyInA))
	sb.WriteString(fmt.Sprintf("  Only in engineer:     %d\n", r.Summary.ConnectionsOnlyInB))

	if r.Summary.DiffConns > 0 || r.Summary.ConnectionsOnlyInA > 0 || r.Summary.ConnectionsOnlyInB > 0 {
		sb.WriteString("\n─── Differences ──────────────────────────────────\n")
		for i, d := range r.ConnectionDiffs {
			if d.Status == "match" {
				continue
			}
			sb.WriteString(fmt.Sprintf("\n  [%d] %s → %s  [%s]\n", i+1, d.Protocol, d.DstAddr, d.Status))
			if d.PacketsA > 0 || d.PacketsB > 0 {
				sb.WriteString(fmt.Sprintf("       Packets: %d (customer) vs %d (engineer)\n", d.PacketsA, d.PacketsB))
			}
			if d.BytesA > 0 || d.BytesB > 0 {
				sb.WriteString(fmt.Sprintf("       Bytes:   %d (customer) vs %d (engineer)\n", d.BytesA, d.BytesB))
			}
			if d.Details != "" {
				sb.WriteString(fmt.Sprintf("       Detail:  %s\n", d.Details))
			}
		}
	}

	if r.Summary.MatchedConns > 0 && r.Summary.DiffConns == 0 && r.Summary.ConnectionsOnlyInA == 0 && r.Summary.ConnectionsOnlyInB == 0 {
		sb.WriteString("\n  All connections match byte-for-byte.\n")
	}

	sb.WriteString("\n═══════════════════════════════════════════════════\n")
	return sb.String()
}
