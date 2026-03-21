package capture

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Writer writes packets to a .kpcap file in a thread-safe manner.
type Writer struct {
	mu   sync.Mutex
	file *os.File
	path string

	packetCount atomic.Uint64
	closed      atomic.Bool
}

// NewWriter creates a new capture file writer.
func NewWriter(path string, mode string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create capture file %s: %w", path, err)
	}

	w := &Writer{
		file: f,
		path: path,
	}

	if err := w.writeHeader(mode); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("failed to write capture file header: %w", err)
	}

	return w, nil
}

// writeHeader writes the file header and metadata.
func (w *Writer) writeHeader(mode string) error {
	// Write the fixed header
	var modeVal uint8
	if mode == "test" {
		modeVal = 1
	}

	header := FileHeader{
		Version:   CurrentVersion,
		Mode:      modeVal,
		CreatedAt: time.Now().UnixNano(),
	}
	copy(header.Magic[:], MagicBytes)

	// Prepare metadata
	hostname, _ := os.Hostname()
	metadata := FileMetadata{
		KeployVersion: "v3",
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Hostname:      hostname,
		Mode:          mode,
		CreatedAt:     time.Now(),
	}

	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	header.MetadataLen = uint32(len(metadataJSON))

	// Write magic
	if _, err := w.file.Write(header.Magic[:]); err != nil {
		return err
	}
	// Write version
	if err := binary.Write(w.file, binary.LittleEndian, header.Version); err != nil {
		return err
	}
	// Write mode
	if err := binary.Write(w.file, binary.LittleEndian, header.Mode); err != nil {
		return err
	}
	// Write created timestamp
	if err := binary.Write(w.file, binary.LittleEndian, header.CreatedAt); err != nil {
		return err
	}
	// Write metadata length
	if err := binary.Write(w.file, binary.LittleEndian, header.MetadataLen); err != nil {
		return err
	}
	// Write metadata JSON
	if _, err := w.file.Write(metadataJSON); err != nil {
		return err
	}

	return nil
}

// WritePacket writes a single packet to the capture file.
// Thread-safe: can be called from multiple goroutines.
//
// Binary format per packet:
//
//	Timestamp    int64    (8 bytes, unix nanoseconds)
//	ConnectionID uint64   (8 bytes)
//	Type         uint8    (1 byte)
//	Direction    uint8    (1 byte)
//	Protocol     uint8    (1 byte)
//	Flags        uint8    (1 byte: bit0=isTLS)
//	SrcAddrLen   uint16   (2 bytes)
//	SrcAddr      []byte   (variable)
//	DstAddrLen   uint16   (2 bytes)
//	DstAddr      []byte   (variable)
//	PayloadLen   uint32   (4 bytes)
//	Payload      []byte   (variable)
func (w *Writer) WritePacket(pkt *Packet) error {
	if w.closed.Load() {
		return fmt.Errorf("capture writer is closed")
	}

	if len(pkt.Payload) > MaxPayloadSize {
		return fmt.Errorf("packet payload too large: %d bytes (max %d)", len(pkt.Payload), MaxPayloadSize)
	}

	count := w.packetCount.Add(1)
	if count > MaxPacketsInFile {
		return fmt.Errorf("capture file packet limit exceeded (%d)", MaxPacketsInFile)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return fmt.Errorf("capture writer file is nil")
	}

	// Timestamp
	if err := binary.Write(w.file, binary.LittleEndian, pkt.Timestamp.UnixNano()); err != nil {
		return fmt.Errorf("failed to write timestamp: %w", err)
	}
	// ConnectionID
	if err := binary.Write(w.file, binary.LittleEndian, pkt.ConnectionID); err != nil {
		return fmt.Errorf("failed to write connection ID: %w", err)
	}
	// Type
	if err := binary.Write(w.file, binary.LittleEndian, uint8(pkt.Type)); err != nil {
		return fmt.Errorf("failed to write packet type: %w", err)
	}
	// Direction
	if err := binary.Write(w.file, binary.LittleEndian, uint8(pkt.Direction)); err != nil {
		return fmt.Errorf("failed to write direction: %w", err)
	}
	// Protocol
	if err := binary.Write(w.file, binary.LittleEndian, uint8(pkt.Protocol)); err != nil {
		return fmt.Errorf("failed to write protocol: %w", err)
	}
	// Flags
	var flags uint8
	if pkt.IsTLS {
		flags |= 0x01
	}
	if err := binary.Write(w.file, binary.LittleEndian, flags); err != nil {
		return fmt.Errorf("failed to write flags: %w", err)
	}

	// SrcAddr
	srcAddrBytes := []byte(pkt.SrcAddr)
	if err := binary.Write(w.file, binary.LittleEndian, uint16(len(srcAddrBytes))); err != nil {
		return fmt.Errorf("failed to write src addr len: %w", err)
	}
	if len(srcAddrBytes) > 0 {
		if _, err := w.file.Write(srcAddrBytes); err != nil {
			return fmt.Errorf("failed to write src addr: %w", err)
		}
	}

	// DstAddr
	dstAddrBytes := []byte(pkt.DstAddr)
	if err := binary.Write(w.file, binary.LittleEndian, uint16(len(dstAddrBytes))); err != nil {
		return fmt.Errorf("failed to write dst addr len: %w", err)
	}
	if len(dstAddrBytes) > 0 {
		if _, err := w.file.Write(dstAddrBytes); err != nil {
			return fmt.Errorf("failed to write dst addr: %w", err)
		}
	}

	// Payload
	if err := binary.Write(w.file, binary.LittleEndian, uint32(len(pkt.Payload))); err != nil {
		return fmt.Errorf("failed to write payload len: %w", err)
	}
	if len(pkt.Payload) > 0 {
		if _, err := w.file.Write(pkt.Payload); err != nil {
			return fmt.Errorf("failed to write payload: %w", err)
		}
	}

	return nil
}

// Sync flushes buffered data to disk.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

// Close closes the writer and flushes all buffered data.
func (w *Writer) Close() error {
	if w.closed.Swap(true) {
		return nil // already closed
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}

	err := w.file.Close()
	w.file = nil
	return err
}

// PacketCount returns the number of packets written.
func (w *Writer) PacketCount() uint64 {
	return w.packetCount.Load()
}
