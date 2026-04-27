package models

import (
	"bytes"
	"reflect"
	"testing"

	yamlLib "gopkg.in/yaml.v3"
)

// TestMockNamePostgresV3Constants pins the exact string values of the
// PostgresV3 Mock.Name constants. Mock.Name participates in hit-count
// indexing, dedup, and by-name lookups across MockManager; drifting
// these strings silently splits the pool. The integrations-repo
// recorder is expected to migrate to these constants in a follow-up
// commit so the string literal and the constant remain identical.
func TestMockNamePostgresV3Constants(t *testing.T) {
	if MockNamePostgresV3Query != "PostgresV3Query" {
		t.Fatalf("MockNamePostgresV3Query: want %q, got %q", "PostgresV3Query", MockNamePostgresV3Query)
	}
	if MockNamePostgresV3Session != "PostgresV3Session" {
		t.Fatalf("MockNamePostgresV3Session: want %q, got %q", "PostgresV3Session", MockNamePostgresV3Session)
	}
}

// TestPostgresV3QuerySpec_BackwardCompat_ScopeSilentlyIgnored asserts
// that old YAML recordings containing the retired `scope` key still
// unmarshal cleanly into PostgresV3QuerySpec after the field was
// removed from the struct. gopkg.in/yaml.v3 is non-strict by default,
// so unknown keys are silently skipped; this test pins that behaviour
// so a future switch to Decoder.KnownFields(true) would fail loudly.
//
// Derived from a real fkppl debug-bundle fragment
// (/tmp/auto-replay-issue/...mocks.yaml) which still stamps
// `scope: session` on 72 per-test Postgres mocks.
func TestPostgresV3QuerySpec_BackwardCompat_ScopeSilentlyIgnored(t *testing.T) {
	// Minimal old-shape YAML: carries the retired `scope` key
	// alongside lifetime / sqlAstHash / invocationId. Fields we do NOT
	// care about for this assertion (response / bindValues / ...) are
	// omitted — zero-values round-trip fine.
	const oldYAML = `
class: select
lifetime: perTest
scope: session
sqlAstHash: sha256:a66bf255653587dd476dfd0524632134bc0623f293ee93d2bf2666447cd2b4d1
sqlNormalized: "select 1"
invocationId: inv-1
`

	var spec PostgresV3QuerySpec
	if err := yamlLib.Unmarshal([]byte(oldYAML), &spec); err != nil {
		t.Fatalf("yaml.Unmarshal returned error on old recording with scope: field — "+
			"backward-compat broke. err=%v", err)
	}
	if spec.Class != "select" {
		t.Fatalf("Class: want %q, got %q", "select", spec.Class)
	}
	if spec.Lifetime != "perTest" {
		t.Fatalf("Lifetime: want %q, got %q", "perTest", spec.Lifetime)
	}
	if spec.SQLAstHash == "" {
		t.Fatalf("SQLAstHash: want non-empty, got empty — known keys stopped parsing")
	}
	if spec.InvocationID != "inv-1" {
		t.Fatalf("InvocationID: want %q, got %q", "inv-1", spec.InvocationID)
	}
}

// TestPostgresV3Response_CopyOut_RoundTrip pins the YAML serialization
// contract for the server-side COPY ... TO STDOUT response shape added
// to support the Postgres wire-features sample (integrations PR #134).
// The replay path in keploy/integrations needs to reconstruct the exact
// CopyOutResponse header + every CopyData packet body the server
// produced, in arrival order; the test walks a recording containing a
// binary-mode, two-column COPY of three rows — including a row whose
// body contains NUL bytes and a row containing 0xFF — and asserts the
// marshal → unmarshal → re-marshal cycle preserves every byte.
//
// Why [][]byte rather than a single concatenated []byte: packet
// boundaries are observable on the wire, and drivers that stream
// CopyOut rely on the boundaries to frame the stdout stream. Storing
// per-packet slices keeps the replay emitter honest.
func TestPostgresV3Response_CopyOut_RoundTrip(t *testing.T) {
	orig := PostgresV3Response{
		CommandComplete: "COPY 3",
		CopyOut: &PostgresV3CopyOutPayload{
			OverallFormat:     1, // binary
			ColumnFormatCodes: []uint16{1, 1},
			Data: [][]byte{
				{0x00, 0x00, 0x00, 0x02, 'a', '\x00', 'b'},
				{0xFF, 0xFE, 0xFD, 0x00, 0x01, 0x02},
				{}, // empty packet — valid per protocol
			},
		},
	}

	firstPass, err := yamlLib.Marshal(&orig)
	if err != nil {
		t.Fatalf("yaml.Marshal orig: %v", err)
	}

	var decoded PostgresV3Response
	if err := yamlLib.Unmarshal(firstPass, &decoded); err != nil {
		t.Fatalf("yaml.Unmarshal first pass: %v", err)
	}

	if decoded.CopyOut == nil {
		t.Fatalf("CopyOut: want non-nil, got nil — YAML round-trip dropped the whole payload")
	}
	if decoded.CopyOut.OverallFormat != orig.CopyOut.OverallFormat {
		t.Fatalf("OverallFormat: want %d, got %d", orig.CopyOut.OverallFormat, decoded.CopyOut.OverallFormat)
	}
	if !reflect.DeepEqual(decoded.CopyOut.ColumnFormatCodes, orig.CopyOut.ColumnFormatCodes) {
		t.Fatalf("ColumnFormatCodes: want %v, got %v", orig.CopyOut.ColumnFormatCodes, decoded.CopyOut.ColumnFormatCodes)
	}
	if len(decoded.CopyOut.Data) != len(orig.CopyOut.Data) {
		t.Fatalf("Data length: want %d, got %d", len(orig.CopyOut.Data), len(decoded.CopyOut.Data))
	}
	for i, got := range decoded.CopyOut.Data {
		want := orig.CopyOut.Data[i]
		if !bytes.Equal(got, want) {
			t.Fatalf("Data[%d]: want %x, got %x — binary payload corrupted on YAML round-trip", i, want, got)
		}
	}

	// Round-trip semantic stability: marshal → unmarshal → marshal →
	// unmarshal must yield a payload identical to the first decode.
	// We avoid asserting raw-byte equality across the two marshal
	// passes because yaml.v3's formatting (style choices, line
	// folding, key ordering for non-struct maps) is not part of its
	// stability contract and can drift across patch releases without
	// changing the decoded value. Comparing the decoded structs
	// catches the things we actually care about (omitempty regressions,
	// custom MarshalYAML drift, semantic loss) without making the test
	// brittle to upstream cosmetics.
	secondPass, err := yamlLib.Marshal(&decoded)
	if err != nil {
		t.Fatalf("yaml.Marshal decoded: %v", err)
	}
	var redecoded PostgresV3Response
	if err := yamlLib.Unmarshal(secondPass, &redecoded); err != nil {
		t.Fatalf("yaml.Unmarshal second pass: %v\nbody:\n%s", err, secondPass)
	}
	if !reflect.DeepEqual(decoded, redecoded) {
		t.Fatalf("round-trip semantic drift:\n first decode:  %#v\n second decode: %#v\n--- first marshal ---\n%s\n--- second marshal ---\n%s", decoded, redecoded, firstPass, secondPass)
	}
}

// TestPostgresV3Response_BackwardCompat_NoCopyFields asserts every
// existing recording — captured before CopyOut/CopyIn existed — still
// unmarshals into the extended PostgresV3Response with both Copy
// fields left nil. This is the gate that stops the upstream schema
// bump from silently invalidating every mock file on disk.
func TestPostgresV3Response_BackwardCompat_NoCopyFields(t *testing.T) {
	// Shape of a legacy recording: RowDescription + Rows +
	// CommandComplete only. Pre-PostgresV3Cell, Rows lived on disk
	// as a [][]string slice — each inner element a YAML scalar
	// containing the recorded text-format wire bytes verbatim
	// (binary-format cells used the printable
	// "~~KEPLOY_PG_NULL~~" sentinel for SQL NULL and the raw text
	// for everything else; yaml.v3 rejected base64 for these short
	// numeric cells because the recorder picked plain style by
	// default). PostgresV3Cells.UnmarshalYAML therefore needs to
	// keep accepting bare `"1"` / `"2"` scalars in the row position.
	// This fixture also includes the retired `scope:` tombstone
	// key — real fkppl debug-bundle fragments still carry it — so
	// the test asserts both (a) the new optional Copy fields default
	// to nil on old mocks and (b) yaml.v3's lenient unknown-field
	// handling still holds on PostgresV3Response: a switch to
	// Decoder.KnownFields(true) downstream would fail this test
	// loudly.
	const legacyYAML = `
rowDescription:
  - name: id
    typeOid: 23
rows:
  - - "1"
  - - "2"
commandComplete: "SELECT 2"
scope: session
`

	var resp PostgresV3Response
	if err := yamlLib.Unmarshal([]byte(legacyYAML), &resp); err != nil {
		t.Fatalf("yaml.Unmarshal legacy recording returned error: %v", err)
	}
	if resp.CopyOut != nil {
		t.Fatalf("CopyOut: want nil on legacy recording, got %+v", resp.CopyOut)
	}
	if resp.CopyIn != nil {
		t.Fatalf("CopyIn: want nil on legacy recording, got %+v", resp.CopyIn)
	}
	if resp.CommandComplete != "SELECT 2" {
		t.Fatalf("CommandComplete: want %q, got %q — unknown-field handling broke existing parsing", "SELECT 2", resp.CommandComplete)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("Rows length: want 2, got %d — legacy shape dropped", len(resp.Rows))
	}
}

// TestPostgresV3Response_CopyIn_RoundTrip covers the COPY ... FROM
// STDIN direction: only the server's CopyInResponse header is
// persisted; the client-produced CopyData is intentionally not stored
// (see PostgresV3CopyInPayload doc). Pinning the tiny shape keeps
// replay emitters honest — a drift from `uint16` to `int16` on the
// format codes would be wire-incompatible and this test flags it.
func TestPostgresV3Response_CopyIn_RoundTrip(t *testing.T) {
	orig := PostgresV3Response{
		CommandComplete: "COPY 10",
		CopyIn: &PostgresV3CopyInPayload{
			OverallFormat:     0, // text
			ColumnFormatCodes: []uint16{0},
		},
	}

	buf, err := yamlLib.Marshal(&orig)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var decoded PostgresV3Response
	if err := yamlLib.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if decoded.CopyIn == nil {
		t.Fatalf("CopyIn: want non-nil, got nil")
	}
	if decoded.CopyIn.OverallFormat != 0 {
		t.Fatalf("OverallFormat: want 0, got %d", decoded.CopyIn.OverallFormat)
	}
	if !reflect.DeepEqual(decoded.CopyIn.ColumnFormatCodes, []uint16{0}) {
		t.Fatalf("ColumnFormatCodes: want [0], got %v", decoded.CopyIn.ColumnFormatCodes)
	}
	// Serialization-stability check. The fixture set only CopyIn, so
	// omitempty on Rows and CopyOut should keep both out of the
	// emitted YAML and the re-decoded struct zero on those paths.
	// This is NOT a schema-level mutual-exclusion guard — nothing in
	// the struct today stops a caller from populating Rows + CopyIn
	// simultaneously and marshaling that malformed shape. Enforcing
	// the wire-level invariant (CopyIn and row-producing traffic
	// never coexist on the same Query response) is the recorder's
	// job in integrations; a misbehaving recorder would emit an
	// invalid mock and this test would still pass. See the
	// TODO(validation) below if we ever add struct-level validation.
	if decoded.Rows != nil {
		t.Fatalf("Rows: omitempty should have kept this nil on the CopyIn-only fixture, got %v", decoded.Rows)
	}
	if decoded.CopyOut != nil {
		t.Fatalf("CopyOut: omitempty should have kept this nil on the CopyIn-only fixture, got %+v", decoded.CopyOut)
	}
	// TODO(validation): once a Validate() method or a custom
	// UnmarshalYAML lands on PostgresV3Response, add a separate test
	// that feeds a malformed fixture carrying BOTH Rows and CopyIn
	// and asserts the validator rejects it.
}
