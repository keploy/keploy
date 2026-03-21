package capture

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AnalysisReport provides a human-readable analysis of a capture file.
type AnalysisReport struct {
	FilePath    string
	Metadata    FileMetadata
	Duration    time.Duration
	Connections []*ConnectionSummary
	Protocols   map[Protocol]int
	TotalBytes  int64
	Errors      []string
}

// ConnectionSummary summarizes a single connection from the capture.
type ConnectionSummary struct {
	ID             uint64
	SrcAddr        string
	DstAddr        string
	Protocol       Protocol
	IsTLS          bool
	Duration       time.Duration
	ClientBytes    int64
	ServerBytes    int64
	PacketCount    int
	FirstPacketAt  time.Time
	LastPacketAt   time.Time
	HasErrors      bool
	ErrorMessages  []string
}

// Analyze produces a detailed analysis of a capture file.
func Analyze(path string) (*AnalysisReport, error) {
	reader, err := NewReader(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open capture file: %w", err)
	}
	defer reader.Close()

	cf, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to read capture file: %w", err)
	}

	return AnalyzeCaptureFile(cf, path), nil
}

// AnalyzeCaptureFile analyzes a parsed capture file.
func AnalyzeCaptureFile(cf *CaptureFile, path string) *AnalysisReport {
	report := &AnalysisReport{
		FilePath:  path,
		Metadata:  cf.Metadata,
		Protocols: make(map[Protocol]int),
	}

	conns := cf.GetConnections()

	var minTime, maxTime time.Time

	for _, ct := range conns {
		cs := &ConnectionSummary{
			ID:       ct.ConnectionID,
			SrcAddr:  ct.SrcAddr,
			DstAddr:  ct.DstAddr,
			Protocol: ct.Protocol,
			IsTLS:    ct.IsTLS,
		}

		if !ct.OpenedAt.IsZero() {
			cs.FirstPacketAt = ct.OpenedAt
			if minTime.IsZero() || ct.OpenedAt.Before(minTime) {
				minTime = ct.OpenedAt
			}
		}
		if !ct.ClosedAt.IsZero() {
			cs.LastPacketAt = ct.ClosedAt
			if maxTime.IsZero() || ct.ClosedAt.After(maxTime) {
				maxTime = ct.ClosedAt
			}
		}

		for _, pkt := range ct.Packets {
			cs.PacketCount++
			switch pkt.Direction {
			case DirClientToProxy, DirProxyToDest:
				cs.ClientBytes += int64(len(pkt.Payload))
			case DirDestToProxy, DirProxyToClient:
				cs.ServerBytes += int64(len(pkt.Payload))
			}
			report.TotalBytes += int64(len(pkt.Payload))

			if pkt.Timestamp.Before(cs.FirstPacketAt) || cs.FirstPacketAt.IsZero() {
				cs.FirstPacketAt = pkt.Timestamp
			}
			if pkt.Timestamp.After(cs.LastPacketAt) {
				cs.LastPacketAt = pkt.Timestamp
			}
			if minTime.IsZero() || pkt.Timestamp.Before(minTime) {
				minTime = pkt.Timestamp
			}
			if pkt.Timestamp.After(maxTime) {
				maxTime = pkt.Timestamp
			}
		}

		if !cs.FirstPacketAt.IsZero() && !cs.LastPacketAt.IsZero() {
			cs.Duration = cs.LastPacketAt.Sub(cs.FirstPacketAt)
		}

		if len(ct.Errors) > 0 {
			cs.HasErrors = true
			cs.ErrorMessages = ct.Errors
			report.Errors = append(report.Errors, ct.Errors...)
		}

		report.Protocols[ct.Protocol]++
		report.Connections = append(report.Connections, cs)
	}

	// Sort connections by first packet time
	sort.Slice(report.Connections, func(i, j int) bool {
		return report.Connections[i].FirstPacketAt.Before(report.Connections[j].FirstPacketAt)
	})

	if !minTime.IsZero() && !maxTime.IsZero() {
		report.Duration = maxTime.Sub(minTime)
	}

	return report
}

// FormatReport formats an analysis report as a human-readable string.
func FormatReport(report *AnalysisReport) string {
	var sb strings.Builder

	sb.WriteString("═══════════════════════════════════════════════════\n")
	sb.WriteString("  Keploy Network Capture Analysis Report\n")
	sb.WriteString("═══════════════════════════════════════════════════\n\n")

	sb.WriteString(fmt.Sprintf("  File:       %s\n", report.FilePath))
	sb.WriteString(fmt.Sprintf("  Mode:       %s\n", report.Metadata.Mode))
	sb.WriteString(fmt.Sprintf("  Created:    %s\n", report.Metadata.CreatedAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("  OS/Arch:    %s/%s\n", report.Metadata.OS, report.Metadata.Arch))
	if report.Metadata.Hostname != "" {
		sb.WriteString(fmt.Sprintf("  Hostname:   %s\n", report.Metadata.Hostname))
	}
	sb.WriteString(fmt.Sprintf("  Duration:   %s\n", report.Duration))
	sb.WriteString(fmt.Sprintf("  Total Data: %s\n", formatBytes(report.TotalBytes)))
	sb.WriteString(fmt.Sprintf("  Connections: %d\n", len(report.Connections)))

	// Protocol breakdown
	sb.WriteString("\n─── Protocol Breakdown ───────────────────────────\n")
	for proto, count := range report.Protocols {
		sb.WriteString(fmt.Sprintf("  %-12s %d connections\n", proto.String()+":", count))
	}

	// Connection details
	sb.WriteString("\n─── Connection Details ───────────────────────────\n")
	for i, cs := range report.Connections {
		if i > 0 {
			sb.WriteString("  ─────────────────────────────────────\n")
		}
		sb.WriteString(fmt.Sprintf("  [%d] Connection #%d\n", i+1, cs.ID))
		sb.WriteString(fmt.Sprintf("       Protocol:  %s", cs.Protocol.String()))
		if cs.IsTLS {
			sb.WriteString(" (TLS)")
		}
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("       Source:    %s\n", cs.SrcAddr))
		sb.WriteString(fmt.Sprintf("       Dest:      %s\n", cs.DstAddr))
		sb.WriteString(fmt.Sprintf("       Duration:  %s\n", cs.Duration))
		sb.WriteString(fmt.Sprintf("       Packets:   %d\n", cs.PacketCount))
		sb.WriteString(fmt.Sprintf("       Client→:   %s\n", formatBytes(cs.ClientBytes)))
		sb.WriteString(fmt.Sprintf("       ←Server:   %s\n", formatBytes(cs.ServerBytes)))

		if cs.HasErrors {
			sb.WriteString("       ERRORS:\n")
			for _, e := range cs.ErrorMessages {
				sb.WriteString(fmt.Sprintf("         - %s\n", e))
			}
		}
	}

	// Errors summary
	if len(report.Errors) > 0 {
		sb.WriteString("\n─── Errors ──────────────────────────────────────\n")
		for i, e := range report.Errors {
			sb.WriteString(fmt.Sprintf("  [%d] %s\n", i+1, e))
		}
	}

	sb.WriteString("\n═══════════════════════════════════════════════════\n")

	return sb.String()
}

// formatBytes formats a byte count into a human-readable string.
func formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
}
