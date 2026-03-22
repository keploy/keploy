package capture

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Reader reads packets from a .kpcap capture file.
type Reader struct {
	file     *os.File
	header   FileHeader
	metadata FileMetadata
}

// NewReader opens a capture file for reading.
func NewReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open capture file %s: %w", path, err)
	}

	r := &Reader{file: f}
	if err := r.readHeader(); err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to read capture file header: %w", err)
	}

	return r, nil
}

// readHeader reads and validates the file header.
func (r *Reader) readHeader() error {
	// Read magic bytes
	if _, err := io.ReadFull(r.file, r.header.Magic[:]); err != nil {
		return fmt.Errorf("failed to read magic bytes: %w", err)
	}
	expectedMagic := [8]byte{}
	copy(expectedMagic[:], MagicBytes)
	if r.header.Magic != expectedMagic {
		return fmt.Errorf("invalid capture file: bad magic bytes (got %x, want %x)", r.header.Magic, expectedMagic)
	}

	// Read version
	if err := binary.Read(r.file, binary.LittleEndian, &r.header.Version); err != nil {
		return fmt.Errorf("failed to read version: %w", err)
	}
	if r.header.Version != CurrentVersion {
		return fmt.Errorf("unsupported capture file version %d (expected %d)", r.header.Version, CurrentVersion)
	}

	// Read mode
	if err := binary.Read(r.file, binary.LittleEndian, &r.header.Mode); err != nil {
		return fmt.Errorf("failed to read mode: %w", err)
	}

	// Read created timestamp
	if err := binary.Read(r.file, binary.LittleEndian, &r.header.CreatedAt); err != nil {
		return fmt.Errorf("failed to read created timestamp: %w", err)
	}

	// Read metadata length
	if err := binary.Read(r.file, binary.LittleEndian, &r.header.MetadataLen); err != nil {
		return fmt.Errorf("failed to read metadata length: %w", err)
	}

	// Validate metadata length (prevent OOM from corrupt files)
	if r.header.MetadataLen > 1*1024*1024 { // 1MB max for metadata
		return fmt.Errorf("metadata too large: %d bytes (max 1MB)", r.header.MetadataLen)
	}

	// Read metadata JSON
	metadataBytes := make([]byte, r.header.MetadataLen)
	if _, err := io.ReadFull(r.file, metadataBytes); err != nil {
		return fmt.Errorf("failed to read metadata: %w", err)
	}

	if err := json.Unmarshal(metadataBytes, &r.metadata); err != nil {
		return fmt.Errorf("failed to parse metadata JSON: %w", err)
	}

	return nil
}

// Header returns the file header.
func (r *Reader) Header() FileHeader {
	return r.header
}

// Metadata returns the file metadata.
func (r *Reader) Metadata() FileMetadata {
	return r.metadata
}

// ReadPacket reads the next packet from the capture file.
// Returns io.EOF when all packets have been read.
func (r *Reader) ReadPacket() (*Packet, error) {
	pkt := &Packet{}

	// Timestamp
	var tsNano int64
	if err := binary.Read(r.file, binary.LittleEndian, &tsNano); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("failed to read timestamp: %w", err)
	}
	pkt.Timestamp = time.Unix(0, tsNano)

	// ConnectionID
	if err := binary.Read(r.file, binary.LittleEndian, &pkt.ConnectionID); err != nil {
		return nil, fmt.Errorf("failed to read connection ID: %w", err)
	}

	// Type
	var pktType uint8
	if err := binary.Read(r.file, binary.LittleEndian, &pktType); err != nil {
		return nil, fmt.Errorf("failed to read packet type: %w", err)
	}
	pkt.Type = PacketType(pktType)

	// Direction
	var dir uint8
	if err := binary.Read(r.file, binary.LittleEndian, &dir); err != nil {
		return nil, fmt.Errorf("failed to read direction: %w", err)
	}
	pkt.Direction = Direction(dir)

	// Protocol
	var proto uint8
	if err := binary.Read(r.file, binary.LittleEndian, &proto); err != nil {
		return nil, fmt.Errorf("failed to read protocol: %w", err)
	}
	pkt.Protocol = Protocol(proto)

	// Flags
	var flags uint8
	if err := binary.Read(r.file, binary.LittleEndian, &flags); err != nil {
		return nil, fmt.Errorf("failed to read flags: %w", err)
	}
	pkt.IsTLS = (flags & 0x01) != 0

	// SrcAddr
	var srcAddrLen uint16
	if err := binary.Read(r.file, binary.LittleEndian, &srcAddrLen); err != nil {
		return nil, fmt.Errorf("failed to read src addr len: %w", err)
	}
	if srcAddrLen > 0 {
		if srcAddrLen > 0xFFFF { // sanity check — matches Writer's max uint16 length
			return nil, fmt.Errorf("src addr too long: %d bytes", srcAddrLen)
		}
		srcAddrBytes := make([]byte, srcAddrLen)
		if _, err := io.ReadFull(r.file, srcAddrBytes); err != nil {
			return nil, fmt.Errorf("failed to read src addr: %w", err)
		}
		pkt.SrcAddr = string(srcAddrBytes)
	}

	// DstAddr
	var dstAddrLen uint16
	if err := binary.Read(r.file, binary.LittleEndian, &dstAddrLen); err != nil {
		return nil, fmt.Errorf("failed to read dst addr len: %w", err)
	}
	if dstAddrLen > 0 {
		if dstAddrLen > 0xFFFF { // sanity check — matches Writer's max uint16 length
			return nil, fmt.Errorf("dst addr too long: %d bytes", dstAddrLen)
		}
		dstAddrBytes := make([]byte, dstAddrLen)
		if _, err := io.ReadFull(r.file, dstAddrBytes); err != nil {
			return nil, fmt.Errorf("failed to read dst addr: %w", err)
		}
		pkt.DstAddr = string(dstAddrBytes)
	}

	// Payload
	var payloadLen uint32
	if err := binary.Read(r.file, binary.LittleEndian, &payloadLen); err != nil {
		return nil, fmt.Errorf("failed to read payload len: %w", err)
	}
	if payloadLen > MaxPayloadSize {
		return nil, fmt.Errorf("payload too large: %d bytes (max %d)", payloadLen, MaxPayloadSize)
	}
	if payloadLen > 0 {
		pkt.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r.file, pkt.Payload); err != nil {
			return nil, fmt.Errorf("failed to read payload: %w", err)
		}
	}

	return pkt, nil
}

// ReadAll reads all packets from the capture file into memory.
// For very large files, prefer streaming with ReadPacket().
func (r *Reader) ReadAll() (*CaptureFile, error) {
	cf := &CaptureFile{
		Header:   r.header,
		Metadata: r.metadata,
	}

	for {
		pkt, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading packet at index %d: %w", len(cf.Packets), err)
		}
		cf.Packets = append(cf.Packets, pkt)

		if len(cf.Packets) > MaxPacketsInFile {
			return nil, fmt.Errorf("too many packets in file (max %d)", MaxPacketsInFile)
		}
	}

	return cf, nil
}

// Close closes the reader.
func (r *Reader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// Validate performs integrity checks on the capture file.
func Validate(path string) (*ValidationResult, error) {
	r, err := NewReader(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	result := &ValidationResult{
		Path:     path,
		Valid:    true,
		Metadata: r.metadata,
	}

	var packetCount int
	var byteCount int64
	connSeen := make(map[uint64]bool)

	for {
		pkt, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("packet %d: %s", packetCount, err.Error()))
			break
		}

		packetCount++
		byteCount += int64(len(pkt.Payload))

		// Check timestamp sanity
		if pkt.Timestamp.IsZero() || pkt.Timestamp.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("packet %d: suspicious timestamp %s", packetCount, pkt.Timestamp))
		}

		// Warn if data arrives before a CONN_OPEN for this connection.
		// The check must happen before connSeen is updated below.
		if pkt.Type == PacketTypeData && !connSeen[pkt.ConnectionID] {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("packet %d: data for unknown connection %d", packetCount, pkt.ConnectionID))
		}

		// Track connections by their open event; set seen for all packet types
		// so repeated data packets for the same unknown connection don't warn repeatedly.
		connSeen[pkt.ConnectionID] = true
	}

	result.PacketCount = packetCount
	result.ByteCount = byteCount
	result.ConnectionCount = len(connSeen)

	return result, nil
}

// ValidationResult holds the results of a capture file validation.
type ValidationResult struct {
	Path            string       `json:"path"`
	Valid           bool         `json:"valid"`
	PacketCount     int          `json:"packet_count"`
	ByteCount       int64        `json:"byte_count"`
	ConnectionCount int          `json:"connection_count"`
	Metadata        FileMetadata `json:"metadata"`
	Errors          []string     `json:"errors,omitempty"`
	Warnings        []string     `json:"warnings,omitempty"`
}
