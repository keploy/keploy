package mysql

// JSON marshalling for ColumnEntry and Parameter. Both carry an
// `interface{}` Value field whose concrete Go type (int64 vs float64,
// []byte vs string, time.Time vs RFC3339-string) the keploy mock
// matcher and the integrations-side codec dispatch on. With the
// json-storage-format work merged on top of this branch, every MySQL
// mock began going through encoding/json on the JSON record path —
// which has no equivalent of yaml.v3's resolver tags. encoding/json's
// reflective decoder maps every JSON number to float64, every JSON
// string to string, and has no way to tell `[]byte` from a regular
// string (Go's default for `[]byte` is a base64-encoded JSON string,
// which round-trips into the wrong Go type).
//
// Symptom in the wild: the MySQL fuzzer sample on
// `record_build_replay_build` returned `mismatches=3939` with diffs
// like `query execution failed: invalid connection, op: select,
// step: 8` — once a column's value type drifted from int64 to
// float64, the integrations-side binary protocol encoder produced
// malformed wire bytes, the driver rejected the response, and every
// subsequent op on the connection failed with `invalid connection`.
//
// Fix: a custom MarshalJSON envelopes []byte / time.Time values with
// `$bin` / `$ts` discriminator wrappers; UnmarshalJSON parses with
// json.Decoder.UseNumber so integer values keep int64 width on the
// way back. The resulting Value is byte-identical to what the YAML
// path produced from the same recording.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// jsonValueDiscriminator is the marker key that distinguishes a
// keploy-internal envelope ($bin / $ts) from a regular JSON object
// that happens to live in a column value (e.g. a JSON-typed column).
// The leading `$` keeps it lexically distinct from any user-supplied
// column name; MySQL identifiers can't legally start with `$` after
// quoting either, so a real column called "$bin" is impossible.
const jsonValueDiscriminator = "$type"

const (
	jsonValueTypeBinary    = "bin"
	jsonValueTypeTimestamp = "ts"
)

// MarshalJSON for ColumnEntry. Wraps `[]byte` and `time.Time` Value
// instances in a discriminator envelope so the round trip preserves
// the Go type. Other Value shapes (numbers, strings, bools, nil,
// nested maps/slices for JSON-column content) flow through
// encoding/json's reflective default — which is type-correct for
// everything except the two cases the envelope handles.
func (c ColumnEntry) MarshalJSON() ([]byte, error) {
	wire := struct {
		Type     FieldType `json:"type"`
		Name     string    `json:"name"`
		Value    any       `json:"value"`
		Unsigned bool      `json:"unsigned"`
	}{
		Type:     c.Type,
		Name:     c.Name,
		Value:    valueToJSONFriendly(c.Value),
		Unsigned: c.Unsigned,
	}
	return json.Marshal(wire)
}

// UnmarshalJSON for ColumnEntry. Splits the wire form into typed
// fields and the raw `value` payload, then decodes the payload
// separately with json.Decoder.UseNumber so integer values come back
// as int64 (matching what yaml.v3's resolver hands the YAML path).
// The recovered any-typed value is what the mock matcher compares
// against and what the integrations codec encodes onto the wire, so
// preserving Go-type fidelity here is a hard requirement.
func (c *ColumnEntry) UnmarshalJSON(data []byte) error {
	type wire struct {
		Type     FieldType       `json:"type"`
		Name     string          `json:"name"`
		Value    json.RawMessage `json:"value"`
		Unsigned bool            `json:"unsigned"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("ColumnEntry: %w", err)
	}
	c.Type = w.Type
	c.Name = w.Name
	c.Unsigned = w.Unsigned
	v, err := decodeJSONValue(w.Value)
	if err != nil {
		return fmt.Errorf("ColumnEntry value: %w", err)
	}
	// Restore the concrete Go type the column's FieldType implies.
	// json.Marshal strips trailing zeros, so a recorded float64(42.0)
	// is written as `42` and recovered as int(42) — a DOUBLE column
	// whose value is integral changes Go type across the round trip.
	// That drift ends up back on disk, because UpdateMocks re-serializes
	// what it decodes; see coerceValueForFieldType.
	c.Value = coerceValueForFieldType(v, c.Type)
	return nil
}

// MarshalJSON for Parameter. Same envelope pattern as ColumnEntry —
// the StmtExecutePacket parameter list shares the same wire-bytes
// fidelity requirement.
func (p Parameter) MarshalJSON() ([]byte, error) {
	wire := struct {
		Type     uint16 `json:"type"`
		Unsigned bool   `json:"unsigned"`
		Name     string `json:"name,omitempty"`
		Value    any    `json:"value"`
	}{
		Type:     p.Type,
		Unsigned: p.Unsigned,
		Name:     p.Name,
		Value:    valueToJSONFriendly(p.Value),
	}
	return json.Marshal(wire)
}

func (p *Parameter) UnmarshalJSON(data []byte) error {
	type wire struct {
		Type     uint16          `json:"type"`
		Unsigned bool            `json:"unsigned"`
		Name     string          `json:"name,omitempty"`
		Value    json.RawMessage `json:"value"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("Parameter: %w", err)
	}
	p.Type = w.Type
	p.Unsigned = w.Unsigned
	p.Name = w.Name
	v, err := decodeJSONValue(w.Value)
	if err != nil {
		return fmt.Errorf("Parameter value: %w", err)
	}
	p.Value = v
	return nil
}

// valueToJSONFriendly converts an any-typed Value into a Go value
// whose json.Marshal output preserves the Go type identity round-
// trip (via decodeJSONValue / recoverJSONValue). Only the two
// JSON-lossy cases need explicit envelopes; numbers, strings, bools,
// and structured map/slice values flow through encoding/json's
// reflective path correctly.
func valueToJSONFriendly(v any) any {
	switch x := v.(type) {
	case []byte:
		return map[string]any{
			jsonValueDiscriminator: jsonValueTypeBinary,
			"data":                 base64.StdEncoding.EncodeToString(x),
		}
	case time.Time:
		return map[string]any{
			jsonValueDiscriminator: jsonValueTypeTimestamp,
			"value":                x.Format(time.RFC3339Nano),
		}
	}
	return v
}

// decodeJSONValue parses a raw JSON payload (the `value` sub-field of
// a ColumnEntry or Parameter) into an any-typed Go value with int64
// preserved for integer literals and []byte / time.Time recovered
// from their `$type` envelopes.
//
// An empty / null payload returns nil so callers can write
//
//	c.Value = nil
//
// without separate sentinel handling.
func decodeJSONValue(payload json.RawMessage) (any, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return recoverJSONValue(raw)
}

// recoverJSONValue walks a json-parsed value (with json.Number nodes
// from UseNumber) and produces the equivalent Go-typed value the
// recorder originally wrote. Mirrors the per-shape behavior of
// yaml.v3's resolver:
//
//	JSON null   → nil
//	JSON bool   → bool
//	json.Number → int64 (preferred) / float64 (fall-through)
//	JSON string → string  (legacy null sentinel handled at caller)
//	JSON array  → []any with each element recovered recursively
//	JSON object → map[string]any (with $type-envelope fast-path
//	              before the recursive walk so binary / timestamp
//	              cells short-circuit cleanly)
func recoverJSONValue(raw any) (any, error) {
	switch x := raw.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case json.Number:
		// Mirror yaml.v3's reflective default for !!int into
		// interface{}: Go `int` when the literal fits, int64 above
		// that (32-bit hosts; empty in practice on 64-bit), and
		// uint64 for an integer literal that overflows int64.
		//
		// The uint64 rung matters: a BIGINT UNSIGNED above MaxInt64
		// fails Int64(), and falling straight through to Float64()
		// loses every bit below 2^11 — 18446744073709551614 and
		// 9223372036854775808 both landed on the same float64 and
		// then on the same wrong integer. yaml.v3 already yields
		// uint64 here, so this is also what keeps the two mock
		// formats agreeing on the same Go type.
		if i, err := x.Int64(); err == nil {
			if int64(int(i)) == i {
				return int(i), nil
			}
			return i, nil
		}
		// Decoded as a JSON number rather than with strconv, to stay
		// symmetric with the rest of this file: x is a JSON token, and
		// encoding/json is what defines whether it names an integer.
		// Behaviour matches strconv.ParseUint(s, 10, 64) exactly for
		// every form reachable here — it accepts a plain integer
		// literal up to MaxUint64 and rejects signed, fractional,
		// exponent and overflowing forms, all of which fall through to
		// the float64 rung below.
		var u uint64
		if err := json.Unmarshal([]byte(x), &u); err == nil {
			return u, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid number %q: %w", string(x), err)
		}
		return f, nil
	case string:
		return x, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := recoverJSONValue(e)
			if err != nil {
				return nil, fmt.Errorf("array[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		// $type-discriminator fast path. The envelope's payload
		// key is fixed by the wrapper (`data` for binary, `value`
		// for timestamp) so the pair can't be confused with a
		// user-supplied JSON-column object.
		if disc, ok := x[jsonValueDiscriminator].(string); ok {
			switch disc {
			case jsonValueTypeBinary:
				return decodeBinaryValue(x["data"])
			case jsonValueTypeTimestamp:
				return decodeTimestampValue(x["value"])
			}
		}
		out := make(map[string]any, len(x))
		for k, v := range x {
			r, err := recoverJSONValue(v)
			if err != nil {
				return nil, fmt.Errorf("object[%q]: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	}
	// json.Decoder with UseNumber should never produce other types
	// at the untyped level, but surface a useful error if it ever
	// does (e.g. someone hand-writes a wrapper that bypasses the
	// decoder).
	return nil, fmt.Errorf("unexpected JSON type %T", raw)
}

func decodeBinaryValue(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("$type=bin: expected string data, got %T", v)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("$type=bin: base64: %w", err)
	}
	return b, nil
}

// coerceValueForFieldType restores the concrete Go type that the
// column's FieldType implies, so that a value recovered from JSON has
// the same Go type it would have had coming off the wire or out of
// YAML:
//
//	FieldTypeTiny / Short / Year / Long / LongLong / Int24 → int
//	FieldTypeFloat                                         → float32
//	FieldTypeDouble                                        → float64
//
// No wire encoder depends on this any more: the binary-row encoder
// coerces whatever it is handed, and text rows are length-encoded
// strings on the wire, so DecodeTextRow stores every value as a string
// and this function passes strings through untouched.
//
// What still depends on it is the mock file itself. MockYaml.UpdateMocks
// is a read-modify-write — it decodes every mock, drops the unused ones,
// and writes the survivors back over the user's file (see
// pkg/platform/yaml/mockdb/db.go). So whatever Go type comes out of here
// is the type that gets re-serialized onto disk. Letting a DOUBLE column
// come back as an int would rewrite `9.0` as `9` in a file users keep in
// git, and would keep drifting on every subsequent run.
//
// Unsigned columns share the same Go type; the unsigned bit selects
// the wire width at encode time, not the Go type here. An integer
// literal above MaxInt64 arrives as uint64 (see recoverJSONValue) and
// is left alone — narrowing it to int is exactly the corruption that
// rung exists to prevent.
//
// String / BLOB / Date-Time columns are left as-is: MarshalJSON
// already preserves their Go type through the round trip ([]byte and
// time.Time go through the $type envelope).
func coerceValueForFieldType(v any, ftype FieldType) any {
	if v == nil {
		return nil
	}
	switch ftype {
	case FieldTypeTiny, FieldTypeShort, FieldTypeYear,
		FieldTypeLong, FieldTypeLongLong, FieldTypeInt24:
		switch x := v.(type) {
		case int:
			return x
		case int64:
			// recoverJSONValue only yields int64 when the literal did
			// not fit int, which is a 32-bit host. Narrowing anyway
			// would truncate exactly the value that rung exists to
			// preserve, so leave it wide when it does not fit.
			//
			// Bounds first, conversion second. `int64(int(x)) == x`
			// would round-trip through the very conversion being
			// tested, which is implementation-defined when out of
			// range — the same mistake exactInt exists to avoid.
			if x >= math.MinInt && x <= math.MaxInt {
				return int(x)
			}
			return x
		case uint64:
			// Above MaxInt64. Leave the width alone; the encoder
			// narrows to the column's wire width and keeps the bits.
			return x
		case float64:
			if n, ok := exactInt(x); ok {
				return n
			}
			// Not integral, or outside int's range. Converting anyway is
			// implementation-defined in Go and saturates to MinInt on
			// amd64, which turns a corrupt mock into a plausible-looking
			// wrong number. Hand it on untouched and let the encoder's
			// coercion reject it by name.
			return x
		case float32:
			if n, ok := exactInt(float64(x)); ok {
				return n
			}
			return x
		}
	case FieldTypeFloat:
		switch x := v.(type) {
		case float32:
			return x
		case float64:
			return float32(x)
		case int:
			return float32(x)
		case int64:
			return float32(x)
		case uint64:
			// Reachable, not defensive: encoding/json writes a float64
			// in 'f' format for any magnitude below 1e21, so a DOUBLE
			// holding 1e19 is marshalled as the integer literal
			// 10000000000000000000 and recovered as uint64.
			return float32(x)
		}
	case FieldTypeDouble:
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		case int:
			return float64(x)
		case int64:
			return float64(x)
		case uint64:
			// See the FieldTypeFloat arm above.
			return float64(x)
		}
	}
	return v
}

// exactInt reports whether f names an integer that survives the trip
// through int without loss. Go leaves an out-of-range float→int
// conversion implementation-defined, so the range has to be checked
// before the conversion, not after.
//
// There are two conversions here and each needs its own bound.
//
// float64 -> int64 is bounded in float space, because 2^63 is the first
// float64 at or above MaxInt64 (float64(MaxInt64) itself rounds *up* to
// it), so MaxInt64 is unreachable from a float64 and the top bound has
// to be exclusive against 2^63 rather than inclusive against MaxInt64.
//
// int64 -> int is then bounded in integer space. On a 64-bit host that
// second bound cannot fire; on a 32-bit one it is the only thing
// standing between a perfectly valid float64 and a truncated int.
func exactInt(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	const twoPow63 = 9223372036854775808.0
	if f < -twoPow63 || f >= twoPow63 {
		return 0, false
	}
	n := int64(f)
	if n < math.MinInt || n > math.MaxInt {
		return 0, false
	}
	return int(n), true
}

func decodeTimestampValue(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("$type=ts: expected string value, got %T", v)
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
