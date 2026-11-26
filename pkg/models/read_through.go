package models

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

const (
	// MetadataReadThroughKey tags a mock in Spec.Metadata as belonging to
	// the deduplicated session read-through tier. Stored in the flat
	// Metadata map so older CLIs/replayers and YAML/JSON/GOB codecs stay
	// backwards-compatible with zero struct size growth.
	MetadataReadThroughKey   = "readThrough"
	MetadataReadThroughValue = "true"

	// DefaultReadThroughMaxEntries bounds the number of unique canonical
	// read keys retained in the session read-through tier per recording
	// or replay session so high-cardinality reads cannot OOM the agent or
	// blow up mocks.yaml.
	DefaultReadThroughMaxEntries = 4096

	// DefaultReadThroughMaxBytes bounds the total payload bytes retained
	// in the session read-through tier per recording session.
	DefaultReadThroughMaxBytes int64 = 32 << 20 // 32 MiB
)

const (
	readThroughUnknown uint8 = iota
	readThroughValid
	readThroughInvalid
)

// IsReadThrough reports whether m is tagged as a session read-through mock
// AND passes the strict read-only classifier. Checking eligibility on read
// provides defense-in-depth: even a hand-edited or corrupted write mock
// carrying readThrough="true" is never served from the read-through tier.
func (m *Mock) IsReadThrough() bool {
	return IsReadThrough(m)
}

// IsReadThrough reports whether m is a valid session read-through mock.
// Uses the ingest-cached validation state (populated once by DeriveLifetime or
// MarkReadThrough) without writing to m so concurrent readers stay race-free.
func IsReadThrough(m *Mock) bool {
	if m == nil || m.Spec.Metadata == nil {
		return false
	}
	if m.Spec.Metadata[MetadataReadThroughKey] != MetadataReadThroughValue {
		return false
	}
	switch m.TestModeInfo.readThroughState {
	case readThroughValid:
		return true
	case readThroughInvalid:
		return false
	}
	_, ok := ReadThroughKey(m)
	return ok
}

// cacheReadThroughState validates a readThrough-tagged mock once at ingest
// (DeriveLifetime) so repeated IsReadThrough checks do not re-parse payloads.
func (m *Mock) cacheReadThroughState() {
	if m == nil || m.Spec.Metadata == nil || m.Spec.Metadata[MetadataReadThroughKey] != MetadataReadThroughValue {
		return
	}
	if _, ok := ReadThroughKey(m); ok {
		m.TestModeInfo.readThroughState = readThroughValid
	} else {
		m.TestModeInfo.readThroughState = readThroughInvalid
	}
}

// MarkReadThrough tags m in Spec.Metadata as a session read-through mock
// iff it is an eligible read-only call with a complete, non-error recorded
// response. Returns false without mutating m if m is a write or ineligible.
func MarkReadThrough(m *Mock) bool {
	if _, ok := ReadThroughKey(m); !ok {
		return false
	}
	if m.Spec.Metadata == nil {
		m.Spec.Metadata = make(map[string]string, 1)
	}
	m.Spec.Metadata[MetadataReadThroughKey] = MetadataReadThroughValue
	m.TestModeInfo.readThroughState = readThroughValid
	return true
}

// ReadThroughMockPrefix and ReadThroughConflictSuffix define the deterministic
// naming contract shared by recorder rescue, disk persistence, and rollover
// carry for deduplicated session read-through mocks and their conflict
// sentinels.
const (
	ReadThroughMockPrefix     = "mock-rt-"
	ReadThroughConflictSuffix = "-conflict"
)

// ReadThroughMockName returns a deterministic mock name derived from a
// canonical ReadThroughKey hex digest (64 bits / 16 hex chars) so deduplicated
// read-through mocks carry stable, collision-resistant names across recording
// and cross-batch seeding.
func ReadThroughMockName(key string) string {
	if len(key) >= 16 {
		return ReadThroughMockPrefix + key[:16]
	}
	return ReadThroughMockPrefix + key
}

// ReadThroughConflictMockName returns the fail-closed conflict sentinel name
// paired with ReadThroughMockName(key).
func ReadThroughConflictMockName(key string) string {
	return ReadThroughMockName(key) + ReadThroughConflictSuffix
}

// ReadThroughConflictBaseName reports whether name is a read-through conflict
// sentinel and returns its paired base mock name.
func ReadThroughConflictBaseName(name string) (string, bool) {
	if !strings.HasPrefix(name, ReadThroughMockPrefix) {
		return "", false
	}
	base, ok := strings.CutSuffix(name, ReadThroughConflictSuffix)
	if !ok || base == ReadThroughMockPrefix || strings.HasSuffix(base, ReadThroughConflictSuffix) {
		return "", false
	}
	return base, true
}

// ReadThroughKey computes the canonical SHA-256 hex key for a recorded
// read-only mock. Returns ("", false) for any write/mutating operation,
// protocol-phase/session/async/undelivered/fault mock, error response,
// incomplete cursor, or mock missing a recorded response.
//
// Never mutates m (checks IsProtocolPhase on a value copy).
func ReadThroughKey(m *Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	if m.IsUndelivered() || m.Kind == ConnectionFailure || m.Kind == RevokedTests || m.IsAsync() || m.Spec.Fault != nil {
		return "", false
	}
	if m.RecordedAsState() {
		return "", false
	}
	if cp := *m; cp.IsProtocolPhase() {
		return "", false
	}
	if !hasValidRecordedReadResponse(m) {
		return "", false
	}
	return ReadThroughRequestKey(m)
}

// ReadThroughRequestKey computes the canonical SHA-256 hex key for a
// read-only dependency request (live or recorded, without requiring a
// recorded response). Only kinds served by replay's read-through tier (Mongo,
// HTTP/1.1, MySQL) are eligible; all other kinds and mutating/unrecognized
// operations return ("", false) fail-closed.
func ReadThroughRequestKey(m *Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	if m.Kind == ConnectionFailure || m.Kind == RevokedTests || m.IsAsync() || m.Spec.Fault != nil {
		return "", false
	}
	switch m.Kind {
	case Mongo:
		return mongoReadThroughKey(m)
	case HTTP:
		if m.Spec.HTTPReq == nil {
			return "", false
		}
		dest := readThroughDest(m, m.Spec.HTTPReq.Header, m.Spec.HTTPReq.URL)
		rawURL := applyReadThroughTLSScheme(m, dest, m.Spec.HTTPReq.URL)
		return readThroughHTTPRequestKey(dest, string(m.Spec.HTTPReq.Method), rawURL, m.Spec.HTTPReq.Header, m.Spec.HTTPReq.Body)
	case MySQL:
		return mysqlReadThroughKey(m)
	default:
		return "", false
	}
}

func applyReadThroughTLSScheme(m *Mock, dest, rawURL string) string {
	if m == nil || m.Spec.Metadata == nil || !strings.EqualFold(m.Spec.Metadata["tls"], "true") {
		return rawURL
	}
	// Check parsed URL scheme rather than searching for "://" anywhere in rawURL,
	// so origin-form URLs carrying "://" in query parameters (e.g. OAuth
	// redirect_uri=https://...) still receive the https:// scheme prefix.
	if u, err := url.Parse(rawURL); err == nil && u.Scheme != "" {
		return rawURL
	}
	if rawURL == "" {
		rawURL = "/"
	} else if rawURL[0] != '/' {
		rawURL = "/" + rawURL
	}
	return "https://" + dest + rawURL
}

func isNilInterfaceOrPtr(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

func hasValidRecordedReadResponse(m *Mock) bool {
	// Fail closed on spilled/truncated responses: without the complete inline
	// response payload we cannot verify status, error flags, or closed cursors.
	if m.HasSpilledResponse() {
		return false
	}
	switch m.Kind {
	case Mongo:
		return isCompleteSuccessMongoResponse(m.Spec.MongoResponses)
	case HTTP:
		return m.Spec.HTTPResp != nil && isCacheableHTTPStatus(m.Spec.HTTPResp.StatusCode)
	case MySQL:
		return isCompleteAutocommitMySQLResultSet(m.Spec.MySQLResponses)
	default:
		return false
	}
}

const (
	mysqlServerStatusInTrans            uint16 = 0x0001
	mysqlServerStatusAutocommit         uint16 = 0x0002
	mysqlServerStatusNoBackslashEscapes uint16 = 0x0200
)

// isCompleteAutocommitMySQLResultSet requires every recorded MySQL response to
// be a result set (rejecting OKPacket from SELECT ... INTO OUTFILE / DML,
// ERRPacket, EOFPacket, and nil) whose wire terminator carries decodable
// server status flags with SERVER_STATUS_IN_TRANS clear,
// SERVER_STATUS_AUTOCOMMIT set (matching replayer/connbind.go, which marks
// connections InTx whenever SERVER_STATUS_IN_TRANS is set or autocommit is
// off), and SERVER_STATUS_NO_BACKSLASH_ESCAPES clear (keeping string-escape
// parsing aligned with standard MySQL mode).
func isCompleteAutocommitMySQLResultSet(resps []mysql.Response) bool {
	if len(resps) == 0 {
		return false
	}
	for _, r := range resps {
		if isNilInterfaceOrPtr(r.Message) {
			return false
		}
		var final *mysql.GenericResponse
		switch msg := r.Message.(type) {
		case *mysql.TextResultSet:
			final = msg.FinalResponse
		case mysql.TextResultSet:
			final = msg.FinalResponse
		case *mysql.BinaryProtocolResultSet:
			final = msg.FinalResponse
		case mysql.BinaryProtocolResultSet:
			final = msg.FinalResponse
		default:
			return false
		}
		status, ok := mysqlResultSetTerminatorStatus(final)
		if !ok || status&mysqlServerStatusInTrans != 0 || status&mysqlServerStatusAutocommit == 0 || status&mysqlServerStatusNoBackslashEscapes != 0 {
			return false
		}
	}
	return true
}

// mysqlResultSetTerminatorStatus decodes the 16-bit server status flags from a
// result set's recorded wire terminator (4-byte header + payload): either a
// protocol-4.1 EOF packet (0xfe, 5-byte payload) or an OK-replacing-EOF packet
// under CLIENT_DEPRECATE_EOF (0xfe, 0x00, 0x00, 2B status, 2B warnings, ...).
// Both place status flags at data[7:9] little-endian. Kept locally in models
// because pkg/models cannot import pkg/agent/proxy/integrations/mysql/utils.
func mysqlResultSetTerminatorStatus(t *mysql.GenericResponse) (uint16, bool) {
	if t == nil || len(t.Data) < 9 || t.Data[4] != 0xFE {
		return 0, false
	}
	payloadLen := uint32(t.Data[0]) | uint32(t.Data[1])<<8 | uint32(t.Data[2])<<16
	if uint32(len(t.Data)-4) < payloadLen {
		return 0, false
	}
	eof41 := payloadLen == 5
	okReplacingEOF := len(t.Data) >= 11 && payloadLen == uint32(len(t.Data)-4) && t.Data[5] == 0x00 && t.Data[6] == 0x00
	if !eof41 && !okReplacingEOF {
		return 0, false
	}
	return binary.LittleEndian.Uint16(t.Data[7:9]), true
}

// isCacheableHTTPStatus accepts 2xx/3xx and 404 (negative cache hits) while
// rejecting transient auth/rate-limit/server errors (401/403/429/5xx).
func isCacheableHTTPStatus(code int) bool {
	return (code >= 200 && code < 400) || code == 404
}

// isCompleteSuccessMongoResponse rejects empty, nil, unparseable, or unknown
// responses, failed responses (missing ok or ok <= 0), and open multi-batch
// cursors (cursor.id != 0 / OP_REPLY.CursorID != 0) fail-closed so transient
// errors or partial cursors whose getMore cannot be answered are never cached.
func isCompleteSuccessMongoResponse(resps []MongoResponse) bool {
	if len(resps) == 0 {
		return false
	}
	validated := 0
	for _, r := range resps {
		if r.Message == nil {
			return false
		}
		var sections []string
		switch msg := r.Message.(type) {
		case *MongoOpMessage:
			if msg == nil || len(msg.Sections) == 0 {
				return false
			}
			sections = msg.Sections
		case MongoOpMessage:
			if len(msg.Sections) == 0 {
				return false
			}
			sections = msg.Sections
		case *MongoOpReply:
			if msg == nil || !mongoOpReplyOK(msg) {
				return false
			}
			validated++
			continue
		case MongoOpReply:
			if !mongoOpReplyOK(&msg) {
				return false
			}
			validated++
			continue
		default:
			return false
		}
		for _, sec := range sections {
			raw, ok := extractMongoOpMsgSingleJSON(sec)
			if !ok {
				return false
			}
			doc, ok := parseOrderedJSONObject(raw)
			if !ok {
				return false
			}
			if !mongoRespDocOK(doc) {
				return false
			}
			validated++
		}
	}
	return validated > 0
}

func mongoOpReplyOK(msg *MongoOpReply) bool {
	// Reject open multi-batch cursors and OP_REPLY error flags
	// (bit 0 = CursorNotFound, bit 1 = QueryFailure).
	if msg.CursorID != 0 || (msg.ResponseFlags&0x03) != 0 {
		return false
	}
	for _, rawDoc := range msg.Documents {
		doc, ok := parseOrderedJSONObject(rawDoc)
		if !ok {
			return false
		}
		for _, f := range doc.fields {
			switch f.key {
			case "ok":
				if !isMongoPositiveOK(f.val) {
					return false
				}
			case "cursor":
				if !mongoCursorClosedOK(f.val) {
					return false
				}
			case "$err", "errmsg":
				return false
			}
		}
	}
	return true
}

func mongoRespDocOK(doc *orderedJSONObj) bool {
	if doc == nil {
		return false
	}
	hasOK := false
	for _, f := range doc.fields {
		switch f.key {
		case "ok":
			if !isMongoPositiveOK(f.val) {
				return false
			}
			hasOK = true
		case "cursor":
			if !mongoCursorClosedOK(f.val) {
				return false
			}
		}
	}
	return hasOK
}

func mongoCursorClosedOK(v any) bool {
	curObj, ok := v.(*orderedJSONObj)
	if !ok {
		return false
	}
	hasID := false
	for _, cf := range curObj.fields {
		if cf.key == "id" {
			if !isMongoZeroNumber(cf.val) {
				return false
			}
			hasID = true
		}
	}
	return hasID
}

func parseMongoNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case *orderedJSONObj:
		for _, f := range x.fields {
			if f.key == "$numberLong" || f.key == "$numberInt" || f.key == "$numberDouble" {
				if s, ok := f.val.(string); ok {
					fv, err := strconv.ParseFloat(s, 64)
					return fv, err == nil
				}
			}
		}
	}
	return 0, false
}

func isMongoZeroNumber(v any) bool {
	f, ok := parseMongoNumber(v)
	return ok && f == 0
}

func isMongoPositiveOK(v any) bool {
	f, ok := parseMongoNumber(v)
	return ok && f > 0
}

func hashReadThroughParts(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{'\x00'})
		}
		_, _ = h.Write([]byte(strconv.Itoa(len(p))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- MongoDB read-only classifier & canonical exact key ---

// mongoVolatileMetaKeys are wire/driver metadata fields that vary across
// invocations without changing query semantics or response shape. Kept strictly
// no looser than mongo/v2's exact matcher; application-chosen options such as
// readConcern.level, writeConcern, comment, allowDiskUse, and apiVersion stay
// significant.
var mongoVolatileMetaKeys = map[string]struct{}{
	"lsid":            {},
	"$clusterTime":    {},
	"$readPreference": {},
	"$client":         {},
	"maxTimeMS":       {},
}

func mongoReadThroughKey(m *Mock) (string, bool) {
	if len(m.Spec.MongoRequests) != 1 {
		return "", false
	}
	// Scope by destination port (omitting ephemeral pod/replica host IPs such
	// as "192.0.2.10:27017" that change across replay pods or DNS fallback)
	// while keeping distinct MongoDB server ports isolated alongside the
	// canonical command's $db and collection.
	port := readThroughDestPort(m.Spec.Metadata)
	msg := m.Spec.MongoRequests[0].Message
	switch v := msg.(type) {
	case *MongoOpMessage:
		if v == nil || len(v.Sections) != 1 || (v.FlagBits&0x02) != 0 || (v.FlagBits&0x10000) != 0 {
			return "", false
		}
		return mongoOpMsgSectionKey(port, v.Sections[0])
	case MongoOpMessage:
		if len(v.Sections) != 1 || (v.FlagBits&0x02) != 0 || (v.FlagBits&0x10000) != 0 {
			return "", false
		}
		return mongoOpMsgSectionKey(port, v.Sections[0])
	case *MongoOpQuery:
		return mongoOpQueryKey(port, v)
	case MongoOpQuery:
		return mongoOpQueryKey(port, &v)
	default:
		return "", false
	}
}

// opQueryDisallowedFlags rejects streaming/cursor OP_QUERY flags:
// TailableCursor (0x02), OplogReplay (0x08), AwaitData (0x20), Exhaust (0x40).
const opQueryDisallowedFlags int32 = 0x02 | 0x08 | 0x20 | 0x40

func mongoOpQueryKey(port string, v *MongoOpQuery) (string, bool) {
	if v == nil || v.FullCollectionName == "" {
		return "", false
	}
	if (v.Flags & opQueryDisallowedFlags) != 0 {
		return "", false
	}
	// Reject legacy $cmd pseudo-collections such as admin.$cmd.sys.unlock or
	// admin.$cmd.sys.killop that execute mutating operations via OP_QUERY.
	if strings.Contains(v.FullCollectionName, ".$cmd.") {
		return "", false
	}
	doc, ok := parseOrderedJSONObject(v.Query)
	if !ok {
		return "", false
	}
	if strings.HasSuffix(v.FullCollectionName, ".$cmd") {
		canon, ok := canonicalizeMongoReadCommand(doc)
		if !ok {
			return "", false
		}
		return hashReadThroughParts("Mongo", port, "OP_QUERY_CMD", v.FullCollectionName, strconv.Itoa(int(v.Flags)), canon), true
	}
	// Legacy OP_QUERY on a normal collection is always a read; preserve
	// key order verbatim (filter embedded docs and $orderby).
	return hashReadThroughParts(
		"Mongo",
		port,
		"OP_QUERY",
		v.FullCollectionName,
		strconv.Itoa(int(v.Flags)),
		strconv.Itoa(int(v.NumberToSkip)),
		strconv.Itoa(int(v.NumberToReturn)),
		encodeOrderedJSONValue(doc, true),
		strings.TrimSpace(v.ReturnFieldsSelector),
	), true
}

func mongoOpMsgSectionKey(port, section string) (string, bool) {
	rawDoc, ok := extractMongoOpMsgSingleJSON(section)
	if !ok {
		return "", false
	}
	doc, ok := parseOrderedJSONObject(rawDoc)
	if !ok {
		return "", false
	}
	canon, ok := canonicalizeMongoReadCommand(doc)
	if !ok {
		return "", false
	}
	return hashReadThroughParts("Mongo", port, "OP_MSG", canon), true
}

// extractMongoOpMsgSingleJSON extracts the JSON command object from a keploy
// MongoOpMessage SectionSingle string (`{ SectionSingle msg: { ... } }`) or
// a raw JSON object (`{ ... }`). Rejects SectionSequence (used by insert/update/delete).
func extractMongoOpMsgSingleJSON(section string) (string, bool) {
	s := strings.TrimSpace(section)
	if strings.HasPrefix(s, "{ SectionSingle msg:") {
		rest := strings.TrimSpace(s[len("{ SectionSingle msg:"):])
		start := strings.IndexByte(rest, '{')
		end := strings.LastIndexByte(rest, '}')
		if start < 0 || end <= start {
			return "", false
		}
		inner := strings.TrimSpace(rest[start:end])
		if strings.HasSuffix(inner, "}") {
			return inner, true
		}
		return rest[start : end+1], true
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return s, true
	}
	return "", false
}

// canonicalizeMongoReadCommand validates that doc's primary command (its first
// BSON/JSON field) is a strictly read-only MongoDB data-plane read command and
// returns a canonical JSON representation preserving nested object key order
// (embedded-document filter equality, sort, $sort, sortBy, $orderby, hint).
func canonicalizeMongoReadCommand(doc *orderedJSONObj) (string, bool) {
	if doc == nil || len(doc.fields) == 0 {
		return "", false
	}
	cmd := doc.fields[0].key
	switch cmd {
	case "find", "count", "distinct":
	case "aggregate":
		if mongoAggregateHasDisallowedStage(doc) {
			return "", false
		}
	default:
		// Strictly fail-closed: insert, update, delete, findAndModify,
		// createIndexes, drop, bulkWrite, etc. are NEVER read-through.
		return "", false
	}
	if _, ok := doc.fields[0].val.(string); !ok {
		return "", false
	}

	filtered := make([]orderedJSONField, 0, len(doc.fields))
	for _, f := range doc.fields {
		// Reject transactional reads and streaming/tailable cursor options fail-closed.
		switch f.key {
		case "txnNumber", "autocommit", "startTransaction",
			"tailable", "awaitData", "oplogReplay", "exhaust":
			return "", false
		}
		if _, volatile := mongoVolatileMetaKeys[f.key]; volatile {
			continue
		}
		if f.key == "readConcern" {
			cleaned, keep := stripReadConcernAfterClusterTime(f.val)
			if !keep {
				continue
			}
			f.val = cleaned
		}
		filtered = append(filtered, f)
	}
	if len(filtered) == 0 {
		return "", false
	}
	// Keep command field first, sort remaining top-level command options
	// canonically, and preserve exact key order inside all nested objects.
	if len(filtered) > 1 {
		rest := filtered[1:]
		sort.Slice(rest, func(i, j int) bool { return rest[i].key < rest[j].key })
	}
	return encodeOrderedJSONObj(&orderedJSONObj{fields: filtered}, true), true
}

func stripReadConcernAfterClusterTime(v any) (any, bool) {
	obj, ok := v.(*orderedJSONObj)
	if !ok {
		return v, true
	}
	out := make([]orderedJSONField, 0, len(obj.fields))
	for _, f := range obj.fields {
		if f.key == "afterClusterTime" {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, false
	}
	return &orderedJSONObj{fields: out}, true
}

func mongoAggregateHasDisallowedStage(doc *orderedJSONObj) bool {
	for _, f := range doc.fields {
		if f.key != "pipeline" {
			continue
		}
		arr, ok := f.val.([]any)
		if !ok {
			return true
		}
		for _, stage := range arr {
			obj, ok := stage.(*orderedJSONObj)
			if !ok {
				return true
			}
			for _, sf := range obj.fields {
				if sf.key == "$out" || sf.key == "$merge" || sf.key == "$changeStream" {
					return true
				}
			}
		}
	}
	return false
}

// --- HTTP/1.1 read-only classifier & canonical exact key ---

var readThroughFlakyHeaders = func() map[string]struct{} {
	m := make(map[string]struct{}, len(FlakyHeaders)+len(ProvenOnlyFlakyHeaders)+10)
	for _, h := range FlakyHeaders {
		m[strings.ToLower(h)] = struct{}{}
	}
	for _, h := range ProvenOnlyFlakyHeaders {
		m[strings.ToLower(h)] = struct{}{}
	}
	// Preserve Authorization and Accept-Encoding so read-through never crosses
	// auth/tenant boundaries or serves mismatched Content-Encoding payloads.
	delete(m, "authorization")
	delete(m, "accept-encoding")
	for _, h := range []string{
		"host", "content-length", "connection", "keep-alive",
		"transfer-encoding", "te", "upgrade",
		"x-trace-id", "x-request-id", "x-correlation-id",
	} {
		m[h] = struct{}{}
	}
	return m
}()

// readThroughHTTPRequestKey builds the canonical SHA-256 hex key for an
// HTTP/1.1 read request. Returns ("", false) for any method other than GET
// or HEAD.
func readThroughHTTPRequestKey(dest, method, rawURL string, headers map[string]string, body string) (string, bool) {
	upperMethod := strings.ToUpper(strings.TrimSpace(method))
	if upperMethod != "GET" && upperMethod != "HEAD" {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	// Use EscapedPath so %2F and / never collide.
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	normDest := strings.ToLower(strings.TrimSpace(dest))
	if normDest == "" {
		normDest = strings.ToLower(strings.TrimSpace(u.Host))
	}

	// Canonical sorted query string; if RawQuery is unparseable (e.g. contains
	// semicolons rejected by Go's ParseQuery), hash RawQuery verbatim instead
	// of silently dropping it.
	var canonQuery string
	if u.RawQuery != "" {
		if q, qerr := url.ParseQuery(u.RawQuery); qerr != nil {
			canonQuery = "raw:" + u.RawQuery
		} else {
			qKeys := make([]string, 0, len(q))
			for k := range q {
				qKeys = append(qKeys, k)
			}
			sort.Strings(qKeys)
			var qBuf strings.Builder
			for i, k := range qKeys {
				if i > 0 {
					qBuf.WriteByte('&')
				}
				vals := q[k]
				for j, v := range vals {
					if j > 0 {
						qBuf.WriteByte('&')
					}
					qBuf.WriteString(url.QueryEscape(k))
					qBuf.WriteByte('=')
					qBuf.WriteString(url.QueryEscape(v))
				}
			}
			canonQuery = qBuf.String()
		}
	}

	// Canonical non-flaky headers (preserves auth/tenant/encoding isolation
	// while deduplicating volatile tracing/request-id headers).
	normHeaders := make(map[string]string, len(headers))
	for k, v := range headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" {
			continue
		}
		if _, flaky := readThroughFlakyHeaders[lk]; flaky {
			continue
		}
		normHeaders[lk] = strings.TrimSpace(v)
	}
	hKeys := make([]string, 0, len(normHeaders))
	for k := range normHeaders {
		hKeys = append(hKeys, k)
	}
	sort.Strings(hKeys)
	var hBuf strings.Builder
	for i, k := range hKeys {
		if i > 0 {
			hBuf.WriteByte('\n')
		}
		hBuf.WriteString(k)
		hBuf.WriteByte(':')
		hBuf.WriteString(normHeaders[k])
	}

	return hashReadThroughParts(string(HTTP), scheme, normDest, upperMethod, path, canonQuery, hBuf.String(), body), true
}

func readThroughDest(m *Mock, headers map[string]string, rawURL string) string {
	for k, v := range headers {
		if strings.EqualFold(k, "Host") && strings.TrimSpace(v) != "" {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return strings.ToLower(strings.TrimSpace(u.Host))
	}
	if m.Spec.Metadata != nil {
		if d := strings.TrimSpace(m.Spec.Metadata["destAddr"]); d != "" {
			return strings.ToLower(d)
		}
	}
	return ""
}

// --- SQL read-only classifier (MySQL) ---

// isReadOnlySQLSelect classifies a SQL statement fail-closed: strips plain SQL
// comments (rejecting MySQL /*! ... */ executable comments), collapses all
// whitespace outside quoted literals (catching \nFOR UPDATE, FOR\tUPDATE,
// FOR/**/UPDATE), requires a FROM clause (rejecting standalone function calls
// like SELECT nextval(...), SELECT setval(...), SELECT my_proc(...), and
// session-state queries like SELECT LAST_INSERT_ID()), rejects table-valued
// function calls in FROM/JOIN, multi-statement batches, locking clauses,
// DML/DDL, and side-effecting / session-state SQL functions.
func isReadOnlySQLSelect(sql string) bool {
	norm, ok := normalizeSQLForReadCheck(sql)
	if !ok || norm == "" {
		return false
	}
	norm = strings.TrimSpace(strings.TrimSuffix(norm, ";"))
	if strings.ContainsRune(norm, ';') {
		return false
	}
	upper := strings.ToUpper(norm)
	if !strings.HasPrefix(upper, "SELECT ") {
		return false
	}
	padded := " " + upper + " "
	if !strings.Contains(padded, " FROM ") && !strings.Contains(padded, " FROM(") {
		return false
	}
	for _, forbidden := range []string{
		" FOR UPDATE ", " FOR SHARE ", " FOR NO KEY UPDATE ", " FOR KEY SHARE ",
		" LOCK IN SHARE MODE ",
		" INTO OUTFILE ", " INTO DUMPFILE ", " INTO ",
		" INSERT ", " UPDATE ", " DELETE ", " MERGE ", " REPLACE ",
		" CREATE ", " ALTER ", " DROP ", " TRUNCATE ", " CALL ", " EXEC ", " DO ",
	} {
		if strings.Contains(padded, forbidden) {
			return false
		}
	}
	// Collapse spaces immediately before '(' so "nextval (" matches "NEXTVAL(".
	tightParen := strings.ReplaceAll(upper, " (", "(")
	for _, sideEffectFn := range []string{
		"NEXTVAL(", "SETVAL(", "CURRVAL(", "LASTVAL(",
		"LAST_INSERT_ID(", "FOUND_ROWS(",
		"PG_ADVISORY_LOCK", "PG_TRY_ADVISORY_LOCK", "PG_ADVISORY_UNLOCK", "PG_ADVISORY_XACT_LOCK",
		"PG_NOTIFY(", "SET_CONFIG(", "PG_TERMINATE_BACKEND(", "PG_CANCEL_BACKEND(",
		"LO_CREATE(", "LO_IMPORT(", "LO_UNLINK(",
		"GET_LOCK(", "RELEASE_LOCK(", "RELEASE_ALL_LOCKS(",
	} {
		if strings.Contains(tightParen, sideEffectFn) {
			return false
		}
	}
	return !hasTableValuedFunctionInFrom(padded)
}

// hasTableValuedFunctionInFrom rejects table-valued function calls and LATERAL
// sources anywhere in a FROM / JOIN / comma-join list (Postgres/SQL
// table-valued procedures that may mutate state).
func hasTableValuedFunctionInFrom(paddedUpper string) bool {
	if hasTableSourceCallAfter(paddedUpper, "JOIN") {
		return true
	}
	rest := paddedUpper
	for {
		after, ok := nextSQLKeywordClause(rest, "FROM")
		if !ok {
			break
		}
		if fromClauseHasTVFOrLateral(after) {
			return true
		}
		rest = after
	}
	return false
}

// nextSQLKeywordClause finds the next occurrence of " <kw> " or " <kw>(" in s
// and returns the slice starting immediately after the keyword (preserving any
// leading '(' so parenthesized subqueries/joins balance depth cleanly).
func nextSQLKeywordClause(s, kw string) (string, bool) {
	patSpace := " " + kw + " "
	patParen := " " + kw + "("
	iSpace := strings.Index(s, patSpace)
	iParen := strings.Index(s, patParen)
	switch {
	case iSpace < 0 && iParen < 0:
		return "", false
	case iSpace >= 0 && (iParen < 0 || iSpace < iParen):
		return s[iSpace+len(patSpace):], true
	default:
		// Keep the '(' in the returned slice so depth tracking sees it.
		return s[iParen+len(kw)+1:], true
	}
}

func hasTableSourceCallAfter(s, kw string) bool {
	rest := s
	for {
		after, ok := nextSQLKeywordClause(rest, kw)
		if !ok {
			return false
		}
		if startsWithTVFOrLateral(after) {
			return true
		}
		rest = after
	}
}

// fromClauseHasTVFOrLateral runs a conservative dual-pass scan (both ignoring
// and tracking backtick-quoted identifiers) so an identifier containing
// " WHERE " before a comma-joined table-valued function or a quoted identifier
// containing a comma-joined TVF cannot hide a LATERAL / TVF call. Both passes
// are fail-closed.
func fromClauseHasTVFOrLateral(clause string) bool {
	return fromClauseHasTVFOrLateralScan(clause, false) || fromClauseHasTVFOrLateralScan(clause, true)
}

func fromClauseHasTVFOrLateralScan(clause string, trackBacktick bool) bool {
	if startsWithTVFOrLateral(clause) {
		return true
	}
	depth := 0
	inSingle, inDouble, inBacktick := false, false, false
	for i := 0; i < len(clause); i++ {
		ch := clause[i]
		if inSingle {
			if ch == '\\' && i+1 < len(clause) {
				i++
				continue
			}
			if ch == '\'' {
				if i+1 < len(clause) && clause[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' && i+1 < len(clause) {
				i++
				continue
			}
			if ch == '"' {
				if i+1 < len(clause) && clause[i+1] == '"' {
					i++
					continue
				}
				inDouble = false
			}
			continue
		}
		if inBacktick {
			if ch == '`' {
				if i+1 < len(clause) && clause[i+1] == '`' {
					i++
					continue
				}
				inBacktick = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '`':
			if trackBacktick {
				inBacktick = true
			}
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return false
			}
			depth--
		case ',':
			if depth == 0 && startsWithTVFOrLateral(clause[i+1:]) {
				return true
			}
		case ' ':
			if depth == 0 && startsWithFromClauseEnd(clause[i+1:]) {
				return false
			}
		}
	}
	return false
}

func startsWithFromClauseEnd(s string) bool {
	for _, endKW := range []string{
		"WHERE ", "GROUP ", "HAVING ", "ORDER ", "LIMIT ", "OFFSET ",
		"FETCH ", "UNION ", "INTERSECT ", "EXCEPT ", "WINDOW ",
		"FOR ", "LOCK ", "INTO ", "QUALIFY ",
	} {
		if strings.HasPrefix(s, endKW) {
			return true
		}
	}
	return false
}

func startsWithTVFOrLateral(s string) bool {
	after := strings.TrimLeft(s, " ")
	if strings.HasPrefix(after, "ONLY ") {
		after = strings.TrimLeft(after[len("ONLY "):], " ")
	}
	if len(after) == 0 {
		return false
	}
	if after[0] == '(' {
		return false
	}
	i := 0
	for i < len(after) && (after[i] >= 'A' && after[i] <= 'Z' || after[i] >= '0' && after[i] <= '9' || after[i] == '_' || after[i] == '.' || after[i] == '"' || after[i] == '`') {
		i++
	}
	if i == 0 {
		return false
	}
	ident := after[:i]
	if ident == "LATERAL" {
		return true
	}
	for i < len(after) && after[i] == ' ' {
		i++
	}
	if ident == "ROWS" && (strings.HasPrefix(after[i:], "FROM ") || strings.HasPrefix(after[i:], "FROM(")) {
		return true
	}
	return i < len(after) && after[i] == '('
}

// isMySQLDashCommentDelim reports whether b satisfies MySQL's "--" comment
// rule: a "--" sequence starts a comment only when followed by whitespace or
// control characters (not e.g. "--1").
func isMySQLDashCommentDelim(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

func normalizeSQLForReadCheck(sql string) (string, bool) {
	var b strings.Builder
	b.Grow(len(sql))
	inSingle, inDouble, inBacktick := false, false, false
	lastSpace := true
	for i := 0; i < len(sql); {
		ch := sql[i]
		if inSingle {
			b.WriteByte(ch)
			if ch == '\\' && i+1 < len(sql) {
				b.WriteByte(sql[i+1])
				i += 2
				lastSpace = false
				continue
			}
			if ch == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					b.WriteByte(sql[i+1])
					i += 2
					continue
				}
				inSingle = false
			}
			lastSpace = false
			i++
			continue
		}
		if inDouble {
			b.WriteByte(ch)
			if ch == '\\' && i+1 < len(sql) {
				b.WriteByte(sql[i+1])
				i += 2
				lastSpace = false
				continue
			}
			if ch == '"' {
				if i+1 < len(sql) && sql[i+1] == '"' {
					b.WriteByte(sql[i+1])
					i += 2
					continue
				}
				inDouble = false
			}
			lastSpace = false
			i++
			continue
		}
		if inBacktick {
			b.WriteByte(ch)
			if ch == '`' {
				if i+1 < len(sql) && sql[i+1] == '`' {
					b.WriteByte(sql[i+1])
					i += 2
					lastSpace = false
					continue
				}
				inBacktick = false
			}
			lastSpace = false
			i++
			continue
		}
		if ch == '\'' {
			inSingle = true
			lastSpace = false
			b.WriteByte(ch)
			i++
			continue
		}
		if ch == '"' {
			inDouble = true
			lastSpace = false
			b.WriteByte(ch)
			i++
			continue
		}
		if ch == '`' {
			inBacktick = true
			lastSpace = false
			b.WriteByte(ch)
			i++
			continue
		}
		if ch == '#' || (ch == '-' && i+1 < len(sql) && sql[i+1] == '-' && (i+2 >= len(sql) || isMySQLDashCommentDelim(sql[i+2]))) {
			step := 1
			if ch == '-' {
				step = 2
			}
			nl := strings.IndexByte(sql[i+step:], '\n')
			if nl < 0 {
				break
			}
			i += step + nl + 1
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		if ch == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			// MySQL /*! ... */ executable comments run SQL code; fail closed.
			if i+2 < len(sql) && sql[i+2] == '!' {
				return "", false
			}
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return "", false
			}
			i += 2 + end + 2
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' || ch == '\v' {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			i++
			continue
		}
		b.WriteByte(ch)
		lastSpace = false
		i++
	}
	if inSingle || inDouble || inBacktick {
		return "", false
	}
	return strings.TrimSpace(b.String()), true
}

func readThroughDestPort(meta map[string]string) string {
	if len(meta) == 0 {
		return ""
	}
	if p := strings.TrimSpace(meta["destPort"]); p != "" {
		return p
	}
	if addr := strings.TrimSpace(meta["destAddr"]); addr != "" {
		if _, port, err := net.SplitHostPort(addr); err == nil {
			return strings.TrimSpace(port)
		}
	}
	return ""
}

func mysqlReadThroughKey(m *Mock) (string, bool) {
	if len(m.Spec.MySQLRequests) != 1 {
		return "", false
	}
	// Fail closed when the connection's active database could not be tracked
	// unambiguously (mid-stream join, COM_CHANGE_USER, multi-statement batch,
	// or unparseable USE statement) or when the read ran inside an open
	// transaction (SERVER_STATUS_IN_TRANS) where it can observe uncommitted
	// writes.
	if m.Spec.Metadata != nil {
		if strings.EqualFold(m.Spec.Metadata["databaseUnknown"], "true") {
			return "", false
		}
		if strings.EqualFold(m.Spec.Metadata["inTx"], "true") {
			return "", false
		}
	}
	var query string
	switch qp := m.Spec.MySQLRequests[0].Message.(type) {
	case *mysql.QueryPacket:
		if qp == nil {
			return "", false
		}
		query = qp.Query
	case mysql.QueryPacket:
		query = qp.Query
	default:
		return "", false
	}
	if !isReadOnlySQLSelect(query) {
		return "", false
	}
	// Scope by destination port (omitting ephemeral pod host IP such as
	// "192.0.2.10:3306" that changes across replay pods) and the connection's
	// active database (stamped from HandshakeResponse41 and COM_INIT_DB/USE on
	// both record and replay) so identical queries across tenant databases or
	// distinct MySQL server ports stay isolated.
	port := readThroughDestPort(m.Spec.Metadata)
	db := ""
	if m.Spec.Metadata != nil {
		db = strings.TrimSpace(m.Spec.Metadata["database"])
	}
	return hashReadThroughParts("MySQL", port, db, strings.TrimSpace(query)), true
}

// --- Ordered JSON AST (preserves MongoDB key order in nested objects) ---

type orderedJSONField struct {
	key string
	val any
}

type orderedJSONObj struct {
	fields []orderedJSONField
}

var errDuplicateJSONKey = errors.New("duplicate JSON object key")

func parseOrderedJSONObject(raw string) (*orderedJSONObj, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	val, err := decodeOrderedJSON(dec)
	if err != nil {
		return nil, false
	}
	obj, ok := val.(*orderedJSONObj)
	if !ok {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return obj, true
}

func decodeOrderedJSON(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		obj := &orderedJSONObj{}
		seen := make(map[string]struct{}, 8)
		for dec.More() {
			ktok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := ktok.(string)
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			if _, dup := seen[key]; dup {
				return nil, errDuplicateJSONKey
			}
			seen[key] = struct{}{}
			val, err := decodeOrderedJSON(dec)
			if err != nil {
				return nil, err
			}
			obj.fields = append(obj.fields, orderedJSONField{key: key, val: val})
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		var arr []any
		for dec.More() {
			val, err := decodeOrderedJSON(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	default:
		return nil, io.ErrUnexpectedEOF
	}
}

func encodeOrderedJSONObj(obj *orderedJSONObj, preserveKeyOrder bool) string {
	fields := obj.fields
	if !preserveKeyOrder && len(fields) > 1 {
		cp := make([]orderedJSONField, len(fields))
		copy(cp, fields)
		sort.Slice(cp, func(i, j int) bool { return cp[i].key < cp[j].key })
		fields = cp
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(f.key)
		b.Write(kb)
		b.WriteByte(':')
		b.WriteString(encodeOrderedJSONValue(f.val, true))
	}
	b.WriteByte('}')
	return b.String()
}

func encodeOrderedJSONValue(v any, preserveKeyOrder bool) string {
	switch x := v.(type) {
	case *orderedJSONObj:
		return encodeOrderedJSONObj(x, preserveKeyOrder)
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, elem := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(encodeOrderedJSONValue(elem, preserveKeyOrder))
		}
		b.WriteByte(']')
		return b.String()
	case json.Number:
		return x.String()
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// mongoVolatileRespKeys are server-generated response metadata fields that vary
// on every invocation even when the returned data is identical.
var mongoVolatileRespKeys = map[string]struct{}{
	"$clusterTime":        {},
	"operationTime":       {},
	"electionId":          {},
	"localTime":           {},
	"lastCommittedOpTime": {},
}

// ReadThroughResponseDigest returns a deterministic SHA-256 digest of mk's
// semantic recorded response payload, omitting per-invocation wire/header
// metadata (Mongo RequestID/ResponseTo/ReadDelay/$clusterTime/operationTime,
// HTTP Date/X-Request-Id, MySQL SequenceID/ReadDelay). Used by both recording
// deduplication and replay staging to distinguish identical duplicates from
// genuinely conflicting responses for the same canonical key.
func ReadThroughResponseDigest(mk *Mock) string {
	if mk == nil {
		return ""
	}
	h := sha256.New()
	switch mk.Kind {
	case Mongo:
		for _, r := range mk.Spec.MongoResponses {
			switch msg := r.Message.(type) {
			case *MongoOpMessage:
				if msg == nil {
					continue
				}
				for _, sec := range msg.Sections {
					_, _ = h.Write([]byte(canonicalMongoRespSection(sec)))
					_, _ = h.Write([]byte{'\x00'})
				}
			case MongoOpMessage:
				for _, sec := range msg.Sections {
					_, _ = h.Write([]byte(canonicalMongoRespSection(sec)))
					_, _ = h.Write([]byte{'\x00'})
				}
			case *MongoOpReply:
				if msg == nil {
					continue
				}
				for _, d := range msg.Documents {
					_, _ = h.Write([]byte(canonicalMongoRespDoc(d)))
					_, _ = h.Write([]byte{'\x00'})
				}
			case MongoOpReply:
				for _, d := range msg.Documents {
					_, _ = h.Write([]byte(canonicalMongoRespDoc(d)))
					_, _ = h.Write([]byte{'\x00'})
				}
			}
		}
	case HTTP:
		if mk.Spec.HTTPResp != nil {
			var ct, ce string
			for k, v := range mk.Spec.HTTPResp.Header {
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "content-type":
					ct = strings.TrimSpace(v)
				case "content-encoding":
					ce = strings.TrimSpace(v)
				}
			}
			_, _ = h.Write([]byte(hashReadThroughParts(
				strconv.Itoa(mk.Spec.HTTPResp.StatusCode),
				ct,
				ce,
				mk.Spec.HTTPResp.Body,
			)))
		}
	case MySQL:
		for _, r := range mk.Spec.MySQLResponses {
			b, _ := json.Marshal(r.Message)
			_, _ = h.Write(b)
			_, _ = h.Write([]byte{'\x00'})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalMongoRespSection(sec string) string {
	raw, ok := extractMongoOpMsgSingleJSON(sec)
	if !ok {
		return sec
	}
	return canonicalMongoRespDoc(raw)
}

func canonicalMongoRespDoc(raw string) string {
	doc, ok := parseOrderedJSONObject(raw)
	if !ok {
		return raw
	}
	filtered := make([]orderedJSONField, 0, len(doc.fields))
	for _, f := range doc.fields {
		if _, volatile := mongoVolatileRespKeys[f.key]; volatile {
			continue
		}
		filtered = append(filtered, f)
	}
	return encodeOrderedJSONObj(&orderedJSONObj{fields: filtered}, true)
}
