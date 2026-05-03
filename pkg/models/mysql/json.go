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
	c.Value = v
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
		// Match yaml.v3's reflective default for !!int into
		// interface{}: it returns Go `int`, NOT int64. The
		// integrations-side wire encoder (binaryProtocolRowPacket.go
		// line ~297) does `ce.Value.(int)` for FieldTypeLongLong
		// and friends — an int64 there panics the encoder, the
		// MySQL parser recovers but the connection is dropped, and
		// the fuzzer reports `query execution failed: invalid
		// connection` on every subsequent op. Return int when the
		// integer literal fits, fall back to int64 only for values
		// outside int's range (32-bit hosts; modern keploy runs on
		// 64-bit so this branch is empty in practice).
		if i, err := x.Int64(); err == nil {
			if int64(int(i)) == i {
				return int(i), nil
			}
			return i, nil
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

