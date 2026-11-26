package wire

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

const RESET = 0x00

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
	// ServerMariaDBCaps and ClientMariaDBCaps are the MariaDB extended
	// capabilities each side sent: the greeting's
	// (HandshakeV10Packet.MariaDBCapabilities) and the client's
	// HandshakeResponse41 or SSLRequest's. MetadataFraming reads them.
	ServerMariaDBCaps uint32
	ClientMariaDBCaps uint32
	// FramingAssumed is set while ClientCaps is assumed rather than read off
	// the client's HandshakeResponse41 or SSLRequest (a connection joined
	// mid-stream, with no SSLRequest of its own captured): the recorder
	// settles whether CLIENT_DEPRECATE_EOF holds from the first packet that
	// tells, and clears it.
	FramingAssumed bool

	// LongDataParams records which parameters of a prepared statement had
	// their value streamed ahead of time with COM_STMT_SEND_LONG_DATA
	// (stmtID -> set of parameter ids).
	//
	// It has to be tracked out of band because the wire gives no other clue:
	// a long-sent parameter is simply ABSENT from the COM_STMT_EXECUTE
	// payload while still being non-NULL in the null bitmap, so a decoder
	// that reads a value for every non-NULL parameter walks off the end of
	// the packet and rejects the command. That is what dropped every
	// streamed-BLOB EXECUTE from the recording (#4262).
	//
	// The server discards long data when the statement executes, resets, or
	// closes, so the entry is cleared at all three points.
	LongDataParams map[uint32]map[uint16]bool

	// StmtParamTypes holds, per prepared statement (by ID), the types of
	// its placeholders from its last COM_STMT_EXECUTE that sent them
	// (new_params_bind_flag 1): an EXECUTE with the flag 0 sends none, and
	// the server, and the decoder, read its values with these
	// (preparedstmt.DecodeStmtExecute). A COM_STMT_CLOSE forgets them.
	StmtParamTypes map[uint32][]mysql.Parameter

	// StmtIDToQuery maps the connection's prepared statements, by ID, to
	// their queries. Replay sets it when a COM_STMT_PREPARE matches; record
	// sets it when a COM_STMT_PREPARE_OK answers one, and records the query
	// on each command that names the statement (mysql.MetaQuery).
	StmtIDToQuery map[uint32]string
	// Statement ID counter for generating unique statement IDs during replay
	NextStmtID uint32

	// InTx, Bound, BoundWindow, AutocommitOff and AutocommitOffAtConnect
	// are replay's binding of the live connection's open transaction to the
	// connection it was recorded on, which the replayer keeps from the
	// status of the answers it replayed (its connection rule). InTx is
	// whether the live connection is in a transaction: the latest answer
	// said one is open, or that its autocommit is off. Bound is the
	// recording that answered the transaction's latest command after its
	// start, and BoundWindow the start of the test window it answered in
	// (zero under exact serving). AutocommitOff is whether the live
	// session's autocommit is off, and AutocommitOffAtConnect whether it was
	// at connect (the greeting's status), which a reset brings back.
	InTx                   bool
	Bound                  *models.Mock
	BoundWindow            time.Time
	AutocommitOff          bool
	AutocommitOffAtConnect bool

	// Database tracks the connection's active MySQL schema (initialized from
	// HandshakeResponse41.Database and updated on successful COM_INIT_DB or
	// USE <db> COM_QUERY) so recorded and replayed read-through queries stay
	// isolated per database/tenant. DatabaseUnknown fails closed whenever the
	// connection joined mid-stream, ran COM_CHANGE_USER, executed a multi-
	// statement batch, or ran an unparseable USE statement.
	Database        string
	DatabaseUnknown bool
	// DestPort holds the destination TCP port of the MySQL server for replay
	// read-through key scoping (extracted from dst host:port, omitting the
	// ephemeral pod IP).
	DestPort string
}

const CLIENT_DEPRECATE_EOF = 0x01000000

// rememberParamTypes notes the types params were sent with, for the
// statement's EXECUTEs that send none (StmtParamTypes). It keeps no value,
// and no name: a flag-0 EXECUTE sends none. The server keeps the types of the
// statement's placeholders only, not of the query attributes sent after them
// (a flag-0 EXECUTE may still carry NULL ones), so neither does this.
func (d *DecodeContext) rememberParamTypes(stmtID uint32, params []mysql.Parameter) {
	if ps := d.PreparedStatements[stmtID]; ps != nil && int(ps.NumParams) < len(params) {
		params = params[:ps.NumParams]
	}
	types := make([]mysql.Parameter, len(params))
	for i, p := range params {
		types[i] = mysql.Parameter{Type: p.Type, Unsigned: p.Unsigned}
	}
	if d.StmtParamTypes == nil {
		d.StmtParamTypes = make(map[uint32][]mysql.Parameter)
	}
	d.StmtParamTypes[stmtID] = types
}

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

// MetadataFraming returns which of MariaDB's optional result-set metadata
// fields the connection's responses carry: those of the extended
// capabilities both sides sent, when both speak MariaDB's extensions
// (CLIENT_MYSQL clear on both). A MySQL server or client gets none.
func (d *DecodeContext) MetadataFraming() mysql.MetadataFraming {
	if d.ServerCaps&mysql.CLIENT_MYSQL != 0 || d.effectiveClientCaps()&mysql.CLIENT_MYSQL != 0 {
		return 0
	}
	caps := d.ServerMariaDBCaps & d.ClientMariaDBCaps
	var f mysql.MetadataFraming
	if caps&mysql.MARIADB_CLIENT_EXTENDED_METADATA != 0 {
		f |= mysql.FramingExtendedMetadata
	}
	if caps&mysql.MARIADB_CLIENT_CACHE_METADATA != 0 {
		f |= mysql.FramingMetadataFollows
	}
	return f
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
	// COM_QUIT is answered only by the server closing the socket. Leaving it
	// out made the async recorder queue it as a response-bearing command,
	// where it is never paired and, worse, keeps the queue non-empty — which
	// suppresses the heldResp guard for any genuinely orphaned response that
	// follows it.
	case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA), mysql.CommandStatusToString(mysql.COM_QUIT):
		return true
	default:
		return false
	}
}

// DestPortFromCfg returns the destination TCP port string from dstCfg,
// preferring dstCfg.Port over dstCfg.Addr so record and replay derive the same
// port even when AddrFabricated is set (and destinationOf returns "").
func DestPortFromCfg(dstCfg *models.ConditionalDstCfg) string {
	if dstCfg == nil {
		return ""
	}
	if dstCfg.Port > 0 {
		return strconv.FormatUint(uint64(dstCfg.Port), 10)
	}
	if addr := strings.TrimSpace(dstCfg.Addr); addr != "" {
		if _, port, err := net.SplitHostPort(addr); err == nil && strings.TrimSpace(port) != "" {
			return strings.TrimSpace(port)
		}
	}
	return ""
}

// SetDestPortFromCfg records the destination TCP port from dstCfg when
// DestPort is not already set.
func (d *DecodeContext) SetDestPortFromCfg(dstCfg *models.ConditionalDstCfg) {
	if d == nil || d.DestPort != "" {
		return
	}
	if port := DestPortFromCfg(dstCfg); port != "" {
		d.DestPort = port
	}
}

// SetDestPortFromAddr records the destination TCP port from addr (host:port)
// when DestPort is not already set.
func (d *DecodeContext) SetDestPortFromAddr(addr string) {
	if d == nil || d.DestPort != "" || strings.TrimSpace(addr) == "" {
		return
	}
	if _, port, err := net.SplitHostPort(strings.TrimSpace(addr)); err == nil && strings.TrimSpace(port) != "" {
		d.DestPort = strings.TrimSpace(port)
	}
}

// MarkDatabaseUnknown marks the connection's active MySQL database unknown
// fail-closed until an explicit COM_INIT_DB or clean USE <db> establishes it.
func (d *DecodeContext) MarkDatabaseUnknown() {
	if d == nil {
		return
	}
	d.Database = ""
	d.DatabaseUnknown = true
}

// MayChangeDatabase reports whether req could change the connection's active
// MySQL database (COM_INIT_DB, COM_CHANGE_USER, or a COM_QUERY containing USE,
// multiple statements, or MySQL executable comments). Used on skip/unrecorded
// paths so failing to record such a command always fails closed.
func (d *DecodeContext) MayChangeDatabase(req *mysql.PacketBundle) bool {
	if req == nil {
		return false
	}
	switch msg := req.Message.(type) {
	case *mysql.InitDBPacket, mysql.InitDBPacket, *mysql.ChangeUserPacket, mysql.ChangeUserPacket:
		return true
	case *mysql.QueryPacket:
		if msg == nil {
			return false
		}
		_, isUse, ambiguous, _ := parseMySQLUseDatabase(msg.Query)
		return isUse || ambiguous
	case mysql.QueryPacket:
		_, isUse, ambiguous, _ := parseMySQLUseDatabase(msg.Query)
		return isUse || ambiguous
	default:
		return false
	}
}

// UpdateDatabaseFromCommand updates d.Database / d.DatabaseUnknown when req
// changes or may have changed the connection's active MySQL database. Fails
// closed (DatabaseUnknown=true) on COM_CHANGE_USER, multi-statement queries,
// MySQL executable comments, and unparseable USE statements so cross-tenant
// collisions cannot occur.
func (d *DecodeContext) UpdateDatabaseFromCommand(req, resp *mysql.PacketBundle) {
	if d == nil || req == nil {
		return
	}
	if resp != nil {
		switch resp.Message.(type) {
		case *mysql.ERRPacket, mysql.ERRPacket:
			return
		}
	}
	switch msg := req.Message.(type) {
	case *mysql.ChangeUserPacket, mysql.ChangeUserPacket:
		d.MarkDatabaseUnknown()
	case *mysql.InitDBPacket:
		if msg != nil {
			d.Database = strings.TrimSpace(msg.Schema)
			d.DatabaseUnknown = false
		}
	case mysql.InitDBPacket:
		d.Database = strings.TrimSpace(msg.Schema)
		d.DatabaseUnknown = false
	case *mysql.QueryPacket:
		if msg != nil {
			d.applyUseQuery(msg.Query)
		}
	case mysql.QueryPacket:
		d.applyUseQuery(msg.Query)
	}
}

func (d *DecodeContext) applyUseQuery(query string) {
	db, isUse, ambiguous, ok := parseMySQLUseDatabase(query)
	if ambiguous {
		d.MarkDatabaseUnknown()
		return
	}
	if !isUse {
		return
	}
	if ok {
		d.Database = db
		d.DatabaseUnknown = false
		return
	}
	d.MarkDatabaseUnknown()
}

func isMySQLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

// skipMySQLSpaceAndComments advances past ASCII whitespace and plain MySQL
// comments (/* ... */, #, -- <space>). Returns (rest, ok); ok is false on
// MySQL executable comments (/*! ... */) or unterminated block comments.
func skipMySQLSpaceAndComments(s string) (string, bool) {
	i := 0
	for i < len(s) {
		if isMySQLSpace(s[i]) {
			i++
			continue
		}
		if s[i] == '/' && i+1 < len(s) && s[i+1] == '*' {
			if i+2 < len(s) && s[i+2] == '!' {
				return "", false
			}
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return "", false
			}
			i += 2 + end + 2
			continue
		}
		if s[i] == '#' || (s[i] == '-' && i+1 < len(s) && s[i+1] == '-' && (i+2 >= len(s) || isMySQLSpace(s[i+2]))) {
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return "", true
			}
			i += nl + 1
			continue
		}
		break
	}
	return s[i:], true
}

// hasMultipleMySQLStatements reports whether query contains a semicolon outside
// quoted strings, backtick identifiers, and SQL comments that is followed by
// any non-whitespace/non-comment token.
func hasMultipleMySQLStatements(query string) bool {
	inSingle, inDouble, inBacktick := false, false, false
	for i := 0; i < len(query); {
		ch := query[i]
		if inSingle {
			if ch == '\\' && i+1 < len(query) {
				i += 2
				continue
			}
			if ch == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					i += 2
					continue
				}
				inSingle = false
			}
			i++
			continue
		}
		if inDouble {
			if ch == '\\' && i+1 < len(query) {
				i += 2
				continue
			}
			if ch == '"' {
				if i+1 < len(query) && query[i+1] == '"' {
					i += 2
					continue
				}
				inDouble = false
			}
			i++
			continue
		}
		if inBacktick {
			if ch == '`' {
				if i+1 < len(query) && query[i+1] == '`' {
					i += 2
					continue
				}
				inBacktick = false
			}
			i++
			continue
		}
		if ch == '\'' {
			inSingle = true
			i++
			continue
		}
		if ch == '"' {
			inDouble = true
			i++
			continue
		}
		if ch == '`' {
			inBacktick = true
			i++
			continue
		}
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			// MySQL executable comments (/*! ... */) can execute USE or
			// multi-statement SQL; fail closed as ambiguous.
			if i+2 < len(query) && query[i+2] == '!' {
				return true
			}
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return true
			}
			i += 2 + end + 2
			continue
		}
		if ch == '#' || (ch == '-' && i+1 < len(query) && query[i+1] == '-' && (i+2 >= len(query) || isMySQLSpace(query[i+2]))) {
			nl := strings.IndexByte(query[i:], '\n')
			if nl < 0 {
				return false
			}
			i += nl + 1
			continue
		}
		if ch == ';' {
			rest, ok := skipMySQLSpaceAndComments(query[i+1:])
			if !ok || rest != "" {
				return true
			}
		}
		i++
	}
	return false
}

// parseMySQLUseDatabase parses a single-statement MySQL USE command (including
// backtick-quoted schema names and comments before, between, or after tokens).
// Reports (schema, isUse, multiStmt, ok).
func parseMySQLUseDatabase(query string) (string, bool, bool, bool) {
	if hasMultipleMySQLStatements(query) {
		return "", false, true, false
	}
	s, ok := skipMySQLSpaceAndComments(query)
	if !ok {
		return "", false, true, false
	}
	if len(s) < 3 || !strings.EqualFold(s[:3], "USE") {
		return "", false, false, false
	}
	if len(s) == 3 {
		return "", true, false, false
	}
	// Require delimiter (whitespace, comment start, or backtick) after USE so
	// identifiers like USER_SETTINGS do not look like USE.
	next := s[3]
	if !isMySQLSpace(next) && next != '`' && next != '/' && next != '#' && next != '-' {
		return "", false, false, false
	}
	rest, ok := skipMySQLSpaceAndComments(s[3:])
	if !ok || rest == "" {
		return "", true, false, false
	}
	var db string
	var after string
	if rest[0] == '`' {
		var b strings.Builder
		closed := false
		for i := 1; i < len(rest); i++ {
			if rest[i] == '`' {
				if i+1 < len(rest) && rest[i+1] == '`' {
					b.WriteByte('`')
					i++
					continue
				}
				after = rest[i+1:]
				closed = true
				break
			}
			b.WriteByte(rest[i])
		}
		if !closed {
			return "", true, false, false
		}
		db = strings.TrimSpace(b.String())
	} else {
		i := 0
		for i < len(rest) && !isMySQLSpace(rest[i]) && rest[i] != ';' && rest[i] != '/' && rest[i] != '#' && rest[i] != '-' {
			i++
		}
		db = strings.TrimSpace(rest[:i])
		after = rest[i:]
	}
	if db == "" {
		return "", true, false, false
	}
	tail, ok := skipMySQLSpaceAndComments(after)
	if !ok {
		return "", true, false, false
	}
	if strings.HasPrefix(tail, ";") {
		tail, ok = skipMySQLSpaceAndComments(tail[1:])
		if !ok {
			return "", true, false, false
		}
	}
	if tail != "" {
		return "", true, false, false
	}
	return db, true, false, true
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
