package learn

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"go.keploy.io/server/v3/pkg/models"
)

// pgMaps hands each read of a Postgres recording its own pgtype.Map: a Map
// is no safe place for two readers at once, and a new one registers every
// type it knows.
var pgMaps = sync.Pool{New: func() any { return pgtype.NewMap() }}

// pgPayload is pg, a Postgres recording, as payloadSpec reads it: what the
// application sent and was answered, each value as the value it is, without
// what says where, when and how it was recorded. pg is not changed: the copy
// is. unread says why a value of it cannot be read as the value it is ("":
// every one can); a drift that may have come from it is refused
// (Raw.Unread).
//
// Of a session (the connection's start-up), the protocol version, the
// server's answer to the SSL request and the authentication mode keploy
// observed are what the driver and keploy said to each other, and no word
// the application was given; the server's version, its parameters and the
// backend's process id and key are what the server told the client, which
// the application can read, and stay.
//
// Of a query, keploy's own words for it, its invocation id, the wire's
// typing of what was sent and answered and its SQL text's placeholder
// numbers go (query). Its bound values, constants and answer cells are read
// as values (value): a value the client sent as text as that text, one it
// sent in binary as its type's text (int4 00 00 00 21 is 33), never as the
// base64 of its bytes, which is no word the application had; a cell the
// recorder kept as bytes (models.PostgresV3CellRaw, whose format code is the
// wire's typing) likewise; a numeric, an interval or another value keploy
// keeps in a form of its own as its type's text (a numeric 12345e-2 is
// 123.45); an array, a multirange or a range as each of its elements, by its
// element type (a range's are its bounds); and a bytea value as the forms an
// application has bytes in (bytesRead). A recording that does not say it
// kept its bound values as the bytes sent (wire, the bindEncoding the
// recorder writes since integrations#229) kept a bytea, json, jsonb or xml
// one decoded, and that is read as the value it is (bind). So are the rows of a
// table's data and of its migration history, a fastpath function call's
// arguments and result, and a text COPY's data; the COPY's formats go. A
// table's data and migration rows state no column types, so an array's or a
// multirange's element and a point, lseg, box or polygon in them, loaded from
// mocks.yaml, are read as keploy's words for them (models reads a cell back by
// its column's type); no recorder writes such rows yet. A field of an
// anonymous record (a record or record[] column), whose field types the answer
// does not state, is read so too. A value keploy cannot read (a binary one of
// a type it does not know, a binary COPY's data, one the recorder kept only
// the start of) leaves the recording unread.
//
// The JSON keys of what stays (query, rows, name, ...) are still read as
// words: they are keploy's, but a document's keys are the application's,
// and a word the application never had found there is checked at replay,
// never left unchecked.
func pgPayload(pg *models.PostgresV3Spec, wire bool) (*models.PostgresV3Spec, string) {
	p := *pg
	r := pgReader{m: pgMaps.Get().(*pgtype.Map), wireBinds: wire}
	defer pgMaps.Put(r.m)
	if s := pg.Session; s != nil {
		c := *s
		c.ProtocolVersion, c.SSLResponse, c.ObservedAuthMode = "", "", ""
		p.Session = &c
	}
	if q := pg.Query; q != nil {
		p.Query = r.query(q)
	}
	if d := pg.Data; d != nil {
		c := *d
		c.Rows = r.rows(d.Rows, nil, d.Columns, "of its data of table "+d.Table)
		p.Data = &c
	}
	if cat := pg.Catalog; cat != nil && len(cat.MigrationState) > 0 {
		c := *cat
		c.MigrationState = make([]models.PostgresV3MigrationTable, len(cat.MigrationState))
		for i, t := range cat.MigrationState {
			t.Rows = r.rows(t.Rows, nil, t.Columns, "of its migration table "+t.Name)
			c.MigrationState[i] = t
		}
		p.Catalog = &c
	}
	return &p, r.unread
}

// pgColumnsByName is v, a Postgres recording's payload as RawOf decoded its
// JSON, with each answer column only by its name, and a text COPY's data
// (copyOut, of pg, the recording) as its text: the copy pgPayload makes keeps
// a column's descriptor with its name alone, whose JSON still writes its
// type as 0, a number the server never sent, and a COPY's format as 0, and
// would write the COPY's rows as base64. A binary COPY's data, whose types
// no message states, is not written: pgPayload notes the recording unread.
// v is changed.
func pgColumnsByName(v any, pg *models.PostgresV3Spec) {
	at := func(v any, key string) any {
		if m, ok := v.(map[string]any); ok {
			return m[key]
		}
		return nil
	}
	resp, _ := at(at(at(v, "postgresV3"), "query"), "response").(map[string]any)
	if resp == nil {
		return
	}
	cols, _ := resp["rowDescription"].([]any)
	for _, c := range cols {
		if m, ok := c.(map[string]any); ok {
			for k := range m {
				if k != "name" {
					delete(m, k)
				}
			}
		}
	}
	if _, ok := resp["copyOut"]; ok && pg.Query != nil && pg.Query.Response != nil && pg.Query.Response.CopyOut != nil {
		co := pg.Query.Response.CopyOut
		if co.OverallFormat != 0 {
			resp["copyOut"] = map[string]any{}
			return
		}
		rows := make([]any, len(co.Data))
		for i, row := range co.Data {
			rows[i] = string(row)
		}
		resp["copyOut"] = map[string]any{"data": rows}
	}
}

// pgReader reads a Postgres recording's values with its own pgtype.Map, and
// keeps the first reason one cannot be read. wireBinds says the recording
// states it kept its bound values as the bytes the client sent (bind).
type pgReader struct {
	m         *pgtype.Map
	wireBinds bool
	unread    string
}

func (r *pgReader) note(why string) {
	if r.unread == "" {
		r.unread = why
	}
}

// query is q without keploy's words for it, its invocation id, the wire's
// typing and its placeholders' numbers (see payloadSpec), with its values
// read as values.
func (r *pgReader) query(q *models.PostgresV3QuerySpec) *models.PostgresV3QuerySpec {
	c := *q
	c.InvocationID, c.SQLAstHash, c.Class, c.Lifetime, c.PrecedingTxState = "", "", "", "", ""
	c.VolatilePositions, c.ParamOIDs, c.BindFormats, c.ResultFormats = nil, nil, nil, nil
	c.SQLNormalized = withoutPlaceholders(q.SQLNormalized)
	c.BindValues = r.binds(q)
	if len(q.Literals) > 0 {
		c.Literals = make(models.PostgresV3Cells, len(q.Literals))
		for i, v := range q.Literals {
			c.Literals[i] = models.PostgresV3Cell{Value: r.value(v.Value, 0, "its constant "+strconv.Itoa(i+1))}
		}
	}
	if q.Response != nil {
		resp := *q.Response
		var oids []uint32
		var names []string
		if len(resp.RowDescription) > 0 {
			resp.RowDescription = make([]models.PostgresV3ColumnDescriptor, len(q.Response.RowDescription))
			oids = make([]uint32, len(q.Response.RowDescription))
			names = make([]string, len(q.Response.RowDescription))
			for i, d := range q.Response.RowDescription {
				resp.RowDescription[i] = models.PostgresV3ColumnDescriptor{Name: d.Name}
				oids[i], names[i] = d.TypeOID, d.Name
			}
		}
		resp.Rows = r.rows(q.Response.Rows, oids, names, "of its answer")
		// A COPY's formats are the wire's typing; its data, sent as text, is
		// read as that text (pgColumnsByName), and binary data, whose
		// columns' types no message states, cannot be read.
		resp.CopyIn = nil
		if co := q.Response.CopyOut; co != nil {
			if co.OverallFormat != 0 && len(co.Data) > 0 {
				r.note("its answer is a binary COPY, whose columns' types keploy is not told")
			}
			resp.CopyOut = &models.PostgresV3CopyOutPayload{}
		}
		if fc := q.Response.FunctionCall; fc != nil {
			resp.FunctionCall = r.functionCall(fc)
		}
		c.Response = &resp
	}
	if q.SideEffects != nil {
		se := *q.SideEffects
		se.TxTransition = ""
		c.SideEffects = &se
	}
	return &c
}

// binds are q's bound values, each as the value the client sent (bind), as
// its type, the parameter type the statement states for it (ParamOIDs).
func (r *pgReader) binds(q *models.PostgresV3QuerySpec) models.PostgresV3Cells {
	if len(q.BindValues) == 0 {
		return q.BindValues
	}
	n := len(q.BindValues)
	out := make(models.PostgresV3Cells, n)
	for i, v := range q.BindValues {
		at := "its bound value $" + strconv.Itoa(i+1)
		var oid uint32
		if i < len(q.ParamOIDs) {
			oid = q.ParamOIDs[i]
		}
		out[i] = models.PostgresV3Cell{Value: r.bind(v.Value, oid, q.BindFormats, i, n, at)}
	}
	return out
}

// bind is v, bound value i of n a Bind with formats sent as type oid, as the
// value it is (sent). A recording that does not state it kept the bytes sent
// (r.wireBinds) is one made before the recorder did (integrations#229),
// which kept a bytea, json, jsonb or xml value decoded, as bytes: such a
// value is read as that value, told from the bytes sent the way the Postgres
// replay tells it (integrations' wireBindBytes, pgSentBytes).
func (r *pgReader) bind(v any, oid uint32, formats []int, i, n int, at string) any {
	b, ok := v.([]byte)
	if !ok || r.wireBinds {
		return r.sent(v, oid, formats, i, n, "the Bind", at)
	}
	format, why := formatAt(formats, i, n, "the Bind")
	if why != "" {
		r.note(at + " was sent in a format keploy cannot tell: " + why)
		return nil
	}
	if pgSentBytes(b, oid, format) {
		return r.wire(oid, format, b, at)
	}
	if r.truncated(b, at) {
		return nil
	}
	if oid == pgtype.ByteaOID {
		return bytesRead(b)
	}
	return string(b)
}

// pgSentBytes reports whether b, a bound value of type oid sent in format
// and kept as bytes by a recorder that does not say how it kept them, is the
// bytes sent rather than a decoded value: a decoded value is bytes only of a
// bytea, a json, a jsonb or an xml, and of those a text bytea in hex form
// (its `\x`, then an even number of hex digits) and a binary jsonb (its
// version byte 1 first, which no JSON text begins with) are the bytes sent.
// The rest of those four (a json, an xml, a binary bytea, a text jsonb)
// read the same either way, and are read as sent. As in integrations'
// wireBindBytes, a decoded bytea whose own bytes spell `\x` and hex is
// taken for the bytes sent; unlike it, so is a binary jsonb that is no JSON
// text either, which wire then notes as unread rather than read as text.
func pgSentBytes(b []byte, oid uint32, format int16) bool {
	switch oid {
	case pgtype.ByteaOID:
		return format == pgtype.TextFormatCode && isHexBytea(b)
	case pgtype.JSONBOID:
		return format == pgtype.BinaryFormatCode && len(b) > 0 && (b[0] == 1 || !json.Valid(b))
	}
	return true
}

// pgBindEncodingKey is the mock metadata in which the Postgres recorder
// states how it kept the bound values; pgBindEncodingWire, as the bytes the
// client sent (integrations' types.BindEncodingKey and BindEncodingWire).
const (
	pgBindEncodingKey  = "bindEncoding"
	pgBindEncodingWire = "wire"
)

// isHexBytea reports whether b is a bytea's text in hex form: `\x`, then an
// even number of hex digits.
func isHexBytea(b []byte) bool {
	if len(b) < 2 || b[0] != '\\' || b[1] != 'x' || len(b)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(string(b[2:]))
	return err == nil
}

// sent is v, value i of n a client sent in msg (the Bind, the call) with
// formats (its format codes, as Postgres reads them: formatAt) as type oid
// (0: not stated), as the value it is: one kept as the bytes sent by its
// format and type (wire), one kept decoded as that value (value).
func (r *pgReader) sent(v any, oid uint32, formats []int, i, n int, msg, at string) any {
	b, ok := v.([]byte)
	if !ok {
		return r.value(v, oid, at)
	}
	format, why := formatAt(formats, i, n, msg)
	if why != "" {
		r.note(at + " was sent in a format keploy cannot tell: " + why)
		return nil
	}
	return r.wire(oid, format, b, at)
}

// functionCall is fc, a fastpath function call, with its arguments and
// result read as values by their formats, and without the formats. A
// fastpath call states no argument or result types, so a binary one cannot
// be read.
func (r *pgReader) functionCall(fc *models.PostgresV3FunctionCall) *models.PostgresV3FunctionCall {
	c := *fc
	formats := make([]int, len(fc.ArgFormats))
	for i, f := range fc.ArgFormats {
		formats[i] = int(f)
	}
	c.ArgFormats, c.ResultFormat = nil, 0
	if len(fc.Args) > 0 {
		c.Args = make([]models.PostgresV3Cell, len(fc.Args))
		for i, a := range fc.Args {
			c.Args[i] = models.PostgresV3Cell{Value: r.sent(a.Value, 0, formats, i, len(fc.Args), "the call", "its function call's argument "+strconv.Itoa(i+1))}
		}
	}
	c.Result = models.PostgresV3Cell{Value: r.sent(fc.Result.Value, 0, []int{int(fc.ResultFormat)}, 0, 1, "the call", "its function call's result")}
	return &c
}

// rows are rows, each cell as the value it is; oids are the columns' types
// (nil: none stated) and names their names, of (what holds the rows) says
// where they are, for a reason one cannot be read.
func (r *pgReader) rows(rows []models.PostgresV3Cells, oids []uint32, names []string, of string) []models.PostgresV3Cells {
	if len(rows) == 0 {
		return rows
	}
	out := make([]models.PostgresV3Cells, len(rows))
	for i, row := range rows {
		out[i] = make(models.PostgresV3Cells, len(row))
		for j, v := range row {
			var oid uint32
			if j < len(oids) {
				oid = oids[j]
			}
			col := strconv.Itoa(j + 1)
			if j < len(names) && names[j] != "" {
				col = strconv.Quote(names[j])
			}
			out[i][j] = models.PostgresV3Cell{Value: r.value(v.Value, oid, "row "+strconv.Itoa(i+1)+", column "+col+" "+of)}
		}
	}
	return out
}

// value is v, a value of type oid (0: not stated) as the recording keeps it,
// as the value it is: the bytes the recorder kept for a type it does not
// decode (models.PostgresV3CellRaw) in their format, as that type (wire); a
// bytea value in the forms an application has bytes in (bytesRead); a value
// keploy keeps in a form of its own (a numeric, an interval, a time, a uuid,
// a geometric value, ...) as its type's text, the type stated or else the
// one its form is of; each element of an array by the array's element type,
// each range of a multirange as a range of the multirange's range type, and
// each value of a document, as the value it is; a float4 as Postgres writes
// it, a text, number or truth value as it stands, as anything keploy cannot
// write as text.
func (r *pgReader) value(v any, oid uint32, at string) any {
	switch x := v.(type) {
	case string:
		if r.truncated([]byte(x), at) {
			return nil
		}
		return v
	case float64:
		return floatText(x)
	case float32:
		// As Postgres writes a float4: 0.1, not the 0.10000000149011612 of
		// its widening to a float8.
		if f := float64(x); math.IsNaN(f) || math.IsInf(f, 0) {
			return floatText(f)
		}
		// Postgres writes 1234567 as 1.234567e+06, a driver hands the
		// application 1234567: both.
		g, f := strconv.FormatFloat(float64(x), 'g', -1, 32), strconv.FormatFloat(float64(x), 'f', -1, 32)
		if g == f {
			return json.Number(g)
		}
		return []any{json.Number(g), json.Number(f)}
	case nil, bool, json.Number, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v
	case models.PostgresV3CellRaw:
		return r.wire(oid, x.Format, x.Bytes, at)
	case []byte:
		if r.truncated(x, at) {
			return nil
		}
		return bytesRead(x)
	case [16]uint8:
		oid = pgtype.UUIDOID
	case net.HardwareAddr:
		oid = pgtype.MacaddrOID
	case time.Time:
		// As its type writes it, and as Go's JSON did.
		if txt, ok := r.typeText(oid, v); ok {
			return []any{txt, x.Format(time.RFC3339Nano)}
		}
		return x.Format(time.RFC3339Nano)
	case pgtype.Range[any]:
		// Its bounds, each by the range's subtype (a tsrange's as
		// timestamps), not keploy's words for their kinds; pgtype writes
		// no Range[any] in text.
		sub, _ := r.elementOID(oid)
		return []any{r.value(x.Lower, sub, at), r.value(x.Upper, sub, at)}
	case pgtype.Multirange[pgtype.Range[any]]:
		// Each range as a range is read, by the multirange's range type.
		et, _ := r.elementOID(oid)
		out := make([]any, len(x))
		for i, rg := range x {
			out[i] = r.value(rg, et, at)
		}
		return out
	case []any:
		// An array's elements, each by its element type (a uuid[]'s as
		// uuids, a bytea[]'s as bytes), never the array's Postgres text,
		// which quotes and escapes them; the elements of a document (a json
		// array) or of no stated type as the values they are.
		et, _ := r.elementOID(oid)
		return r.elements(x, et, at)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = r.value(e, 0, at)
		}
		return out
	}
	if txt, ok := r.typeText(oid, v); ok {
		return txt
	}
	return v
}

// elements are x, an array's elements (or an array's of arrays), each as a
// value of type et (0: not stated).
func (r *pgReader) elements(x []any, et uint32, at string) []any {
	out := make([]any, len(x))
	for i, e := range x {
		if inner, ok := e.([]any); ok {
			out[i] = r.elements(inner, et, at)
			continue
		}
		out[i] = r.value(e, et, at)
	}
	return out
}

// elementOID is the type of each element of a value of type oid: an array's
// element type, a multirange's range type, a range's subtype (its bounds'
// type). ok is false for any other type.
func (r *pgReader) elementOID(oid uint32) (uint32, bool) {
	if oid == 0 {
		return 0, false
	}
	t, ok := r.m.TypeForOID(oid)
	if !ok {
		return 0, false
	}
	switch c := t.Codec.(type) {
	case *pgtype.ArrayCodec:
		if c.ElementType != nil {
			return c.ElementType.OID, true
		}
	case *pgtype.MultirangeCodec:
		if c.ElementType != nil {
			return c.ElementType.OID, true
		}
	case *pgtype.RangeCodec:
		if c.ElementType != nil {
			return c.ElementType.OID, true
		}
	}
	return 0, false
}

// typeText is v as type oid writes it in text, or, where oid is 0 or does
// not write v, as the type pgtype gives v's form; ok is false where neither
// does.
func (r *pgReader) typeText(oid uint32, v any) (string, bool) {
	if oid != 0 {
		if txt, err := encodeText(r.m, oid, v); err == nil {
			return txt, true
		}
	}
	if t, ok := r.m.TypeForValue(v); ok {
		if txt, err := encodeText(r.m, t.OID, v); err == nil {
			return txt, true
		}
	}
	return "", false
}

// floatText is f as Postgres writes a float: NaN and the infinities by name
// (keploy's JSON wraps them in words of its own), any other as it is.
func floatText(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return f
}

// truncatedMark starts what the recorder keeps of a value too long to keep
// whole: its size and the hex of its first bytes.
const truncatedMark = "keploy-truncated:"

// truncated notes b, a value at at, as unread where the recorder kept only
// its start: what the rest held cannot be told.
func (r *pgReader) truncated(b []byte, at string) bool {
	if !bytes.HasPrefix(b, []byte(truncatedMark)) {
		return false
	}
	size, _, _ := strings.Cut(string(b[len(truncatedMark):]), ":")
	r.note(at + " was " + size + " bytes long, and keploy recorded only its start. To record it whole, raise KEPLOY_PG_V3_MAX_CAPTURED_VALUE_BYTES above that and record again")
	return true
}

// bytesRead is b, a bytea value, in each form an application has bytes in:
// as their text, where they are UTF-8 (written as JSON, other bytes are no
// text the application had); the hex text Postgres writes them in, and that
// hex as a word of its own (hex.EncodeToString, Python's bytes.hex(); the
// `\x` would glue an x onto the word); and their standard base64, the form
// Go's encoding/json and Jackson write a []byte or byte[] in.
func bytesRead(b []byte) any {
	if len(b) == 0 {
		return ""
	}
	h := hex.EncodeToString(b)
	out := []any{`\x` + h, h, base64.StdEncoding.EncodeToString(b)}
	if utf8.Valid(b) {
		out = append(out, string(b))
	}
	return out
}

// wire is b, a value sent in format (0 text, 1 binary) as type oid, as the
// value it is: text as it was sent, binary as its type writes it in text, a
// bytea as its bytes (bytesRead), a json as its text, an array, a
// multirange or a range in either format also as its elements (value),
// since pgtype writes no range it decodes in text. One keploy
// cannot read is noted, and read as nothing.
func (r *pgReader) wire(oid uint32, format int16, b []byte, at string) any {
	switch {
	case r.truncated(b, at):
		return nil
	case format != pgtype.TextFormatCode && format != pgtype.BinaryFormatCode:
		r.note(at + " was sent in format " + strconv.Itoa(int(format)) + ", which is neither text nor binary")
		return nil
	case oid == pgtype.ByteaOID && format == pgtype.TextFormatCode:
		// As sent, and as the bytes its hex text names.
		s := string(b)
		if isHexBytea(b) {
			if raw, err := hex.DecodeString(s[2:]); err == nil {
				return []any{s, bytesRead(raw)}
			}
		}
		return s
	case format == pgtype.TextFormatCode:
		// An array's, a multirange's or a range's elements as the values
		// they are, beside its text.
		if _, ok := r.elementOID(oid); ok {
			if t, ok := r.m.TypeForOID(oid); ok {
				if v, err := decodeValue(r.m, t, oid, format, b); err == nil {
					return []any{string(b), r.value(v, oid, at)}
				}
			}
		}
		return string(b)
	case oid == pgtype.ByteaOID:
		return bytesRead(b)
	case oid == 0:
		r.note(at + " was sent in binary with no type stated")
		return nil
	case oid == pgtype.JSONOID:
		return string(b)
	case oid == pgtype.JSONBOID:
		// jsonb's binary form is its version (1) and its text.
		if len(b) > 0 && b[0] == 1 {
			return string(b[1:])
		}
		r.note(at + " was sent in binary as jsonb, but not in jsonb's version 1")
		return nil
	}
	t, ok := r.m.TypeForOID(oid)
	if !ok {
		r.note(at + " was sent in binary as Postgres type " + strconv.FormatUint(uint64(oid), 10) + ", which keploy does not know how to read yet")
		return nil
	}
	if _, ok := r.elementOID(oid); ok {
		v, err := decodeValue(r.m, t, oid, format, b)
		if err != nil {
			r.note(at + " was sent in binary as Postgres type " + t.Name + " (" + strconv.FormatUint(uint64(oid), 10) + "), and keploy cannot read it as one: " + err.Error())
			return nil
		}
		return r.value(v, oid, at)
	}
	txt, err := binaryText(r.m, t, oid, b)
	if err != nil {
		r.note(at + " was sent in binary as Postgres type " + t.Name + " (" + strconv.FormatUint(uint64(oid), 10) + "), and keploy cannot read it as one: " + err.Error())
		return nil
	}
	return txt
}

// binaryText is b, a binary value of type t (oid), as t writes it in text.
// A codec that panics on bytes it cannot read (pgx's array codecs on an
// array cut short) is an error here.
func binaryText(m *pgtype.Map, t *pgtype.Type, oid uint32, b []byte) (string, error) {
	v, err := decodeValue(m, t, oid, pgtype.BinaryFormatCode, b)
	if err != nil {
		return "", err
	}
	return encodeText(m, oid, v)
}

// decodeValue is b, a value of type t (oid) in format, as t's codec decodes
// it. A codec that panics on bytes it cannot read is an error here.
func decodeValue(m *pgtype.Map, t *pgtype.Type, oid uint32, format int16, b []byte) (v any, err error) {
	defer func() {
		if p := recover(); p != nil {
			v, err = nil, fmt.Errorf("its codec failed on its bytes: %v", p)
		}
	}()
	return t.Codec.DecodeValue(m, oid, format, b)
}

// encodeText is v as type oid writes it in text ("" for an empty or null
// value). A codec that panics on v is an error here.
func encodeText(m *pgtype.Map, oid uint32, v any) (txt string, err error) {
	defer func() {
		if p := recover(); p != nil {
			txt, err = "", fmt.Errorf("its codec failed on it: %v", p)
		}
	}()
	buf, err := m.Encode(oid, pgtype.TextFormatCode, v, nil)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// formatAt is the format of value i of n that msg (the Bind, a fastpath
// call) with formats sent it in, as Postgres reads its format codes: none,
// all text; one, all of it; n, one each. why says why it cannot be told:
// another count of codes, or a code no message can carry; either is no
// message a client sends, so the recording may be damaged.
func formatAt(formats []int, i, n int, msg string) (int16, string) {
	var f int
	switch len(formats) {
	case 0:
		return pgtype.TextFormatCode, ""
	case 1:
		f = formats[0]
	case n:
		f = formats[i]
	default:
		return 0, msg + " gives " + strconv.Itoa(len(formats)) + " format codes for its " + strconv.Itoa(n) + " values. If the recording was edited, record it again"
	}
	if f < math.MinInt16 || f > math.MaxInt16 {
		return 0, "its format code " + strconv.Itoa(f) + " is no code " + msg + " can carry. If the recording was edited, record it again"
	}
	return int16(f), ""
}
