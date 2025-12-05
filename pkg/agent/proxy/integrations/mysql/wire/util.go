package wire

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

const RESET = 0x00

// PreparedStmtEntry tracks the lifecycle of a single prepared statement
type PreparedStmtEntry struct {
	StmtID     uint32 // Statement ID assigned during replay
	Query      string // The prepared SQL query (stored as-is, normalized during lookups)
	PreparedAt uint32 // Cycle number when prepared
	ClosedAt   int32  // Cycle number when closed (-1 if still active)
}

// MaxPreparedStmtHistorySize is the threshold at which old closed entries are pruned
// to prevent unbounded memory growth during long-lived connections
const MaxPreparedStmtHistorySize = 1000

// PreparedStmtHistory maintains full history of prepared statements
// to handle ID reuse scenarios (prepare→close→prepare cycles).
// When the history exceeds MaxPreparedStmtHistorySize, old closed entries are pruned.
type PreparedStmtHistory struct {
	Entries      []PreparedStmtEntry // Append-only history of all preparations
	QueryIndex   map[string][]int    // normalized query -> indices in Entries
	CurrentCycle uint32              // Monotonically increasing prepare counter
}

// NewPreparedStmtHistory creates a new history tracker
func NewPreparedStmtHistory() *PreparedStmtHistory {
	return &PreparedStmtHistory{
		Entries:      make([]PreparedStmtEntry, 0),
		QueryIndex:   make(map[string][]int),
		CurrentCycle: 0,
	}
}

// RecordPrepare logs a new prepared statement
func (h *PreparedStmtHistory) RecordPrepare(stmtID uint32, query string) {
	h.CurrentCycle++
	entry := PreparedStmtEntry{
		StmtID:     stmtID,
		Query:      query,
		PreparedAt: h.CurrentCycle,
		ClosedAt:   -1, // Still active
	}
	idx := len(h.Entries)
	h.Entries = append(h.Entries, entry)

	// Update query index (case-insensitive, trimmed)
	normalizedQuery := strings.TrimSpace(strings.ToLower(query))
	h.QueryIndex[normalizedQuery] = append(h.QueryIndex[normalizedQuery], idx)

	// Prune old closed entries if history grows too large
	if len(h.Entries) > MaxPreparedStmtHistorySize {
		h.pruneClosedEntries()
	}
}

// pruneClosedEntries removes old closed entries to prevent unbounded memory growth.
// It keeps all active entries and the most recent half of closed entries.
func (h *PreparedStmtHistory) pruneClosedEntries() {
	// Separate active and closed entries
	var activeEntries []PreparedStmtEntry
	var closedEntries []PreparedStmtEntry

	for _, e := range h.Entries {
		if e.ClosedAt == -1 {
			activeEntries = append(activeEntries, e)
		} else {
			closedEntries = append(closedEntries, e)
		}
	}

	// Keep only the most recent half of closed entries
	keepCount := len(closedEntries) / 2
	if keepCount < len(closedEntries) {
		closedEntries = closedEntries[len(closedEntries)-keepCount:]
	}

	// Rebuild entries and index
	h.Entries = make([]PreparedStmtEntry, 0, len(activeEntries)+len(closedEntries))
	h.QueryIndex = make(map[string][]int)

	// Add closed entries first (older), then active entries (newer)
	for _, e := range closedEntries {
		idx := len(h.Entries)
		h.Entries = append(h.Entries, e)
		normalizedQuery := strings.TrimSpace(strings.ToLower(e.Query))
		h.QueryIndex[normalizedQuery] = append(h.QueryIndex[normalizedQuery], idx)
	}
	for _, e := range activeEntries {
		idx := len(h.Entries)
		h.Entries = append(h.Entries, e)
		normalizedQuery := strings.TrimSpace(strings.ToLower(e.Query))
		h.QueryIndex[normalizedQuery] = append(h.QueryIndex[normalizedQuery], idx)
	}
}

// RecordClose marks a prepared statement as closed at the current cycle
func (h *PreparedStmtHistory) RecordClose(stmtID uint32) {
	// Find the most recent active entry with this stmtID and mark it closed at current cycle
	for i := len(h.Entries) - 1; i >= 0; i-- {
		if h.Entries[i].StmtID == stmtID && h.Entries[i].ClosedAt == -1 {
			h.Entries[i].ClosedAt = int32(h.CurrentCycle)
			break
		}
	}
}

// GetActiveEntryByQuery returns the most recent active entry for a query
func (h *PreparedStmtHistory) GetActiveEntryByQuery(query string) *PreparedStmtEntry {
	normalizedQuery := strings.TrimSpace(strings.ToLower(query))
	indices, ok := h.QueryIndex[normalizedQuery]
	if !ok {
		return nil
	}
	// Return the most recent active entry
	for i := len(indices) - 1; i >= 0; i-- {
		entry := &h.Entries[indices[i]]
		if entry.ClosedAt == -1 {
			return entry
		}
	}
	return nil
}

// GetPrepareCountForQuery returns how many times a query has been prepared
func (h *PreparedStmtHistory) GetPrepareCountForQuery(query string) int {
	normalizedQuery := strings.TrimSpace(strings.ToLower(query))
	return len(h.QueryIndex[normalizedQuery])
}

// GetCurrentCycle returns the current prepare cycle number
func (h *PreparedStmtHistory) GetCurrentCycle() uint32 {
	return h.CurrentCycle
}

type DecodeContext struct {
	Mode               models.Mode
	LastOp             *LastOperation
	PreparedStatements map[uint32]*mysql.StmtPrepareOkPacket
	ServerGreetings    *ServerGreetings
	ClientCapabilities uint32
	PluginName         string
	UseSSL             bool
	// Capability flags
	ServerCaps         uint32 // negotiated server caps (from HandshakeV10)
	ClientCaps         uint32 // live client's caps (from HandshakeResponse41)
	RecordedClientCaps uint32 // caps from the recorded config mock
	PreferRecordedCaps bool   // if true, prefer RecordedClientCaps over ClientCaps

	//runtime stmt-id → query mapping set when COM_STMT_PREP matches
	StmtIDToQuery map[uint32]string
	// Statement ID counter for generating unique statement IDs during replay
	NextStmtID uint32
	// StmtHistory tracks full prepared statement lifecycle for ID reuse handling
	StmtHistory *PreparedStmtHistory
}

const CLIENT_DEPRECATE_EOF = 0x01000000

func (d *DecodeContext) effectiveClientCaps() uint32 {
	if d.PreferRecordedCaps && d.RecordedClientCaps != 0 {
		return d.RecordedClientCaps
	}
	return d.ClientCaps
}

func (d *DecodeContext) DeprecateEOF() bool {
	return (d.ServerCaps&CLIENT_DEPRECATE_EOF) != 0 &&
		(d.effectiveClientCaps()&CLIENT_DEPRECATE_EOF) != 0
}

// This map is used to store the last operation that was performed on a connection.
// It helps us to determine the last mysql packet.

type LastOperation struct {
	sync.RWMutex
	operations map[net.Conn]byte
}

func NewLastOpMap() *LastOperation {
	return &LastOperation{
		operations: make(map[net.Conn]byte),
	}
}

func (lo *LastOperation) Load(key net.Conn) (value byte, ok bool) {
	lo.RLock()
	result, ok := lo.operations[key]
	lo.RUnlock()
	return result, ok
}

func (lo *LastOperation) Store(key net.Conn, value byte) {
	lo.Lock()
	lo.operations[key] = value
	lo.Unlock()
}

// This map is used to store the server greetings for each connection.
// It helps us to determine the server version and capabilities.
// Capabilities are helpful in decoding some packets.

type ServerGreetings struct {
	sync.RWMutex
	handshakes map[net.Conn]*mysql.HandshakeV10Packet
}

func NewGreetings() *ServerGreetings {
	return &ServerGreetings{
		handshakes: make(map[net.Conn]*mysql.HandshakeV10Packet),
	}
}

func (sg *ServerGreetings) Load(key net.Conn) (*mysql.HandshakeV10Packet, bool) {
	sg.RLock()
	result, ok := sg.handshakes[key]
	sg.RUnlock()
	return result, ok
}

func (sg *ServerGreetings) Store(key net.Conn, value *mysql.HandshakeV10Packet) {
	sg.Lock()
	sg.handshakes[key] = value
	sg.Unlock()
}

func setPacketInfo(_ context.Context, parsedPacket *mysql.PacketBundle, pkt interface{}, pktType string, clientConn net.Conn, lastOp byte, decodeCtx *DecodeContext) {
	parsedPacket.Header.Type = pktType
	parsedPacket.Message = pkt
	decodeCtx.LastOp.Store(clientConn, lastOp)
}

func GetPluginName(buf interface{}) (string, error) {
	switch v := buf.(type) {
	case *mysql.HandshakeV10Packet:
		return v.AuthPluginName, nil
	case *mysql.AuthSwitchRequestPacket:
		return v.PluginName, nil
	default:
		return "", fmt.Errorf("invalid packet type to get plugin name")
	}
}

func GetCachingSha2PasswordMechanism(data byte) (string, error) {
	switch data {
	case byte(mysql.PerformFullAuthentication):
		return mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication), nil
	case byte(mysql.FastAuthSuccess):
		return mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess), nil
	default:
		einval := fmt.Sprintf("invalid caching_sha2_password mechanism, found:%02x ", data)
		return "", fmt.Errorf("%s", einval)
	}
}

func StringToCachingSha2PasswordMechanism(data string) (mysql.CachingSha2Password, error) {
	switch data {
	case "PerformFullAuthentication":
		return mysql.PerformFullAuthentication, nil
	case "FastAuthSuccess":
		return mysql.FastAuthSuccess, nil
	default:
		einval := fmt.Sprintf("invalid caching_sha2_password mechanism, found:%s ", data)
		return 0, fmt.Errorf("%s", einval)
	}
}

func IsGenericResponsePkt(packet *mysql.PacketBundle) bool {
	if packet == nil {
		return false
	}
	switch packet.Message.(type) {
	case *mysql.OKPacket, *mysql.ERRPacket, *mysql.EOFPacket:
		return true
	default:
		return false
	}
}

func IsNoResponseCommand(command string) bool {
	switch command {
	case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA):
		return true
	default:
		return false
	}
}

// PrintByteArray is only for debugging purpose
func PrintByteArray(name string, b []byte) {
	fmt.Printf("%s:\n", name)
	var i = 1
	for _, byte := range b {
		fmt.Printf(" %02x", byte)
		i++
		if i%16 == 0 {
			fmt.Println()
		}
	}
	fmt.Println()
}
