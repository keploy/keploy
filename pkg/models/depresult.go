package models

import (
	"fmt"
	"strconv"
	"strings"
)

// Dependency-assertion vocabulary for models.Result.DepResult.
//
// DepResult was declared long before anything wrote it: across keploy OSS,
// enterprise and k8s-proxy it had ONE declaration, ONE reader and ZERO
// writers. Slice 4 of keploy-consumer-design-v2.md ("DepResult on the sync
// path") makes the replayer the first writer: for every HTTP/gRPC test, one
// row per per-test dependency the recording says the test exercised that was
// NOT observed during the test's window. The ones that WERE observed are not
// rows at all — they are a count on the result (Result.DepsConsumed), so a
// test that loses nothing still serializes `dep_result: []`.
//
// The one existing reader is k8s-proxy's report-download handler, which turns
// each Normal==false row into a "dependency" diff — but only for tests whose
// status is FAILED or OBSOLETE (it calls compactFailureDiffs on those alone).
// A PASSED test carrying a missing dependency — the silent-green case this
// slice exists to expose — therefore still shows as clean in the fleet report
// until a companion k8s-proxy change surfaces it. OSS's text, JUnit and NDJSON
// surfaces all show it today.
//
// The row NAME is a stable, human-first identifier that agents also parse, so
// its shape is a one-way door:
//
//	deps[<i>] <type> <target> (presence)
//
// <i> indexes the test's RECORDED dependency list (the filtered mapping), not
// the emitted rows, so a dependency keeps the same name from run to run
// whatever else went missing in any particular run.
//
// SETTLED CONVENTION: the sync path uses `deps[i]`; `effects[i]` / `writes[i]`
// are reserved for the consumer projector (design §2, slice 5), whose rows
// carry field-level diffs decoded from a protocol payload. A sync-path row is
// presence-only and covers outgoing reads as well as writes, so calling an
// outgoing HTTP GET a "write" would be wrong, and keeping the prefixes
// disjoint lets a consumer tell the two producers apart without a
// schema-version bump. keploy-consumer-design-v2.md §2 and §7 slice 4 carry
// the same convention.
const (
	// DepKeyPresence is the DepMetaResult.Key used by presence-only
	// dependency assertions — the only per-dependency kind the sync path
	// emits.
	DepKeyPresence = "presence"

	// DepKeyMissingCount is the DepMetaResult.Key of the OVERFLOW row that
	// stands in for the missing dependencies past the per-test cap. See
	// DepMissingOverflowRow.
	DepKeyMissingCount = "missing_count"

	// DepPresenceConsumed / DepPresenceMissing are the Expected/Actual values
	// of a presence assertion. They are literal strings, not booleans, because
	// DepMetaResult.Expected/Actual are strings in the persisted schema and
	// must stay that way for on-disk compatibility.
	DepPresenceConsumed = "consumed"
	DepPresenceMissing  = "not consumed"

	// DepNameSuffixPresence marks a row as PRODUCED BY THE SYNC-PATH PRESENCE
	// WRITER. Every row this file builds carries it — the per-dependency rows
	// and the missing-count overflow row — so one regex
	// (`^deps\[[0-9*]+\] .* \(presence\)$`) matches the whole family and a
	// consumer can tell a slice-4 sync row from a slice-5 `effects[i]` row,
	// which carries field-level diffs decoded from a protocol payload, without
	// a schema-version bump.
	//
	// It is deliberately NOT read as "this row asserts presence and nothing
	// else": the overflow row asserts a COUNT (DepKeyMissingCount), and
	// FormatDepResults special-cases it for exactly that reason. The suffix is
	// a FAMILY MARKER, not an assertion kind.
	DepNameSuffixPresence = "(presence)"

	// DepSummaryIndex is the index token used by the missing-count overflow
	// row, chosen so it cannot collide with a real `deps[<int>]` row.
	DepSummaryIndex = "*"
)

// Normalised dependency protocol families used by DepResult.Type and by the
// `effects[].type` field of the NDJSON contract.
//
// These are FAMILIES, not parser versions. models.Kind carries the parser
// version (PostgresV2 / PostgresV3 / Http2), which is an implementation detail
// of whichever recorder produced the mock: the same logical Postgres
// dependency would otherwise render as "postgres", "postgresv2" or
// "postgresv3" purely by recording vintage, and an agent keying on
// `type == "postgres"` would silently miss most real recordings.
const (
	DepTypeHTTP     = "http"
	DepTypePostgres = "postgres"
	DepTypeGRPC     = "grpc"
)

// DepTypeForKind maps a mock Kind onto the stable protocol family used by
// DepResult.Type. Kinds that already lowercase to their family (MySQL, Mongo,
// Redis, Kafka, DNS, Generic, Aerospike, …) fall through to the default, so a
// newly added Kind is handled sensibly without a code change — but
// TestDepTypeForKind_CoversEveryKind fails until the new constant is
// explicitly accounted for in the table, so a version-suffixed Kind cannot
// slip through unnoticed.
func DepTypeForKind(kind Kind) string {
	switch kind {
	case HTTP, HTTP2:
		return DepTypeHTTP
	case Postgres, PostgresV2, PostgresV3:
		return DepTypePostgres
	case GRPC_EXPORT:
		return DepTypeGRPC
	}
	return strings.ToLower(strings.TrimSpace(string(kind)))
}

// DepRowName builds the stable DepResult.Name for a presence-checked
// dependency. index is the dependency's position in the test's RECORDED
// (filtered) dependency list — never its position among the emitted rows,
// which would make the name depend on what else went missing in that run —
// depType the normalised protocol family (see DepTypeForKind), and target a
// human-meaningful destination (method + host/path, host:port, request
// summary, …). depType and target are both optional: the mock pool does not
// always carry them and a row must stay readable without them rather than
// degrade to "deps[0]  (presence)".
func DepRowName(index int, depType, target string) string {
	return depRowName(strconv.Itoa(index), depType, target)
}

func depRowName(index, depType, target string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "deps[%s]", index)
	if depType != "" {
		sb.WriteString(" ")
		sb.WriteString(depType)
	}
	if target != "" {
		sb.WriteString(" ")
		sb.WriteString(target)
	}
	sb.WriteString(" ")
	sb.WriteString(DepNameSuffixPresence)
	return sb.String()
}

// DepMissingOverflowRow is the single row that stands in for the missing
// dependencies beyond the per-test cap.
//
// WHY THERE IS A CAP AT ALL. The flagship case this slice exists to expose —
// a downstream service removed, a worker that stopped producing, a mock pool
// whose names drifted wholesale — makes EVERY expected per-test mock
// unconsumed for EVERY test. Measured through reportdb.InsertReport over 100
// tests x 200 mapped dependencies, an uncapped missing side wrote a 5.4 MB
// test-set report where the pre-slice-4 build wrote 115 KB (47x), for a run
// that today produces OBSOLETE tests and zero extra bytes. Reports are written
// per test-set, re-read by `keploy report` and uploaded to the fleet report
// store, so that growth lands in exactly the environment that can least afford
// it — and it would land with the verdict knob OFF and no opt-in. The same
// sizing argument is why the CONSUMED side is a plain count on the result
// (Result.DepsConsumed) rather than one row per dependency.
//
// WHY THE ROW EXISTS RATHER THAN THE ROWS SIMPLY BEING TRUNCATED. It carries
// Normal=false, so HasMissingDeps, MissingDepNames, the k8s-proxy report
// handler and the NDJSON `matched: false` rule all still see the failure. A
// silent truncation would under-report the very thing the slice makes visible;
// this reports "and N more", which is a weaker claim but a true one.
//
// Expected "0" / Actual "<n>" reads as "expected nothing left unconsumed,
// found n", matching the expected/actual vocabulary of every other row.
func DepMissingOverflowRow(missing int) DepResult {
	return DepResult{
		// No Type: the overflow spans every protocol the test used, so any
		// single family string here would be a lie.
		Name: depRowName(DepSummaryIndex, "", fmt.Sprintf("%d more not consumed", missing)),
		Meta: []DepMetaResult{{
			Normal:   false,
			Key:      DepKeyMissingCount,
			Expected: "0",
			Actual:   strconv.Itoa(missing),
		}},
	}
}

// IsDepMissingOverflow reports whether a row is the missing-count overflow row
// rather than a per-dependency assertion.
func IsDepMissingOverflow(d DepResult) bool {
	return hasDepKey(d, DepKeyMissingCount)
}

func hasDepKey(d DepResult, key string) bool {
	for _, m := range d.Meta {
		if m.Key == key {
			return true
		}
	}
	return false
}

// DepRowMatched reports whether every assertion carried by a dependency row
// held. A row with no meta entries counts as matched: absence of a failed
// assertion is not a failure.
func DepRowMatched(d DepResult) bool {
	for _, m := range d.Meta {
		if !m.Normal {
			return false
		}
	}
	return true
}

// MissingDepNames returns the names of the dependency rows carrying at least
// one failed assertion, in row order.
func MissingDepNames(deps []DepResult) []string {
	var out []string
	for _, d := range deps {
		if !DepRowMatched(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

// DependenciesChecked reports whether the per-test dependency assertion
// actually RAN for this test.
//
// It reads the persisted DepsChecked bit, NEVER len(DepResult): rows are
// written only for dependencies that went MISSING, so a test that lost nothing
// and a test whose assertion never executed (a --base-path / remote-agent run,
// --disable-mapping, a test set with no usable mappings.yaml, a failed
// per-test consumed-mock fetch, the deferred streaming path, a test the mapping
// records no dependency for, a test whose every mapped dependency was
// ineligible — session/connection tier or DNS, which is what an untagged
// HTTP/Postgres/MySQL mock is — or any report written before this field had a
// writer) all carry `dep_result: []` and are told apart only by this bit.
//
// A consumer MUST check this before reading "no failed rows" as "no dependency
// regressions"; the two are not the same statement.
func (r Result) DependenciesChecked() bool {
	return r.DepsChecked
}

// HasMissingDeps reports whether any dependency assertion on this result
// failed.
func (r Result) HasMissingDeps() bool {
	for _, d := range r.DepResult {
		if !DepRowMatched(d) {
			return true
		}
	}
	return false
}

// FormatDepResults renders the full dependency-assertion block. Its only
// callers are the two `keploy report` renderers (the compact one and --full),
// which share it so they cannot drift from each other.
//
// It is NOT used by the live `keploy test` output: that path prints through
// pkg/matcher's DiffsPrinter, which knows nothing about DepResult. During a
// replay run a vanished dependency surfaces as the replayer's own log line
// (per-test Debug with the knob off, Error with it on, plus one Warn per test
// set) and in the persisted report; the expected-vs-actual block below appears
// when the user runs `keploy report`.
//
// It returns the empty string for an empty row set, which is what keeps output
// byte-identical for every test that has no per-test dependency bookkeeping
// (i.e. everything recorded before this slice).
//
// Failed rows get one line per failed DepMetaResult in the same expected/actual
// language the body-diff block uses. The writer emits no matched rows at all
// (the consumed side is a count, Result.DepsConsumed), but the matched arm is
// kept so a row from any other producer still renders as a compact
// "consumed (presence only)" line, visually distinct from a real assertion
// exactly as design §2 requires, rather than being silently dropped.
func FormatDepResults(deps []DepResult) string {
	if len(deps) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("=== DEPENDENCY ASSERTIONS ===\n")
	for _, d := range deps {
		if DepRowMatched(d) {
			fmt.Fprintf(&sb, "  OK      %s - consumed (presence only)\n", d.Name)
			continue
		}
		fmt.Fprintf(&sb, "  MISSING %s\n", d.Name)
		if IsDepMissingOverflow(d) {
			// The overflow row already reads "<n> more not consumed";
			// appending `missing_count: expected "0", actual "<n>"` would
			// restate it.
			continue
		}
		for _, m := range d.Meta {
			if m.Normal {
				continue
			}
			key := m.Key
			if key == "" {
				key = DepKeyPresence
			}
			fmt.Fprintf(&sb, "      %s: expected %q, actual %q\n", key, m.Expected, m.Actual)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// DepNoticePrefix leads the compact one-line notice. Stable: CI log scrapers
// and the agent loop grep for it.
const DepNoticePrefix = "DEPENDENCY NOT EXERCISED:"

// DepNoticeHint names the knob that turns the notice into a verdict.
const DepNoticeHint = "reported only — run with --assert-dependencies to fail on this"

// FormatDepNotice renders the COMPACT one-line notice for a test that did not
// fail but lost a recorded outgoing call.
//
// This is the flagship silent-green case the slice exists to expose: the
// response still matches (a deterministic dependency, a cached value, a
// swallowed error) while the call the recording says the test makes was not
// observed during the test's window. The replayer leaves such a test PASSED
// by design — with --assert-dependencies off, which is the default, the
// verdict does not change. Visibility must not depend on the knob, so the
// notice is emitted whatever the status; only the FAILED/OBSOLETE path gets
// the full block.
//
// Returns "" when nothing is missing, which keeps every pre-slice-4 report
// byte-identical.
func FormatDepNotice(deps []DepResult) string {
	missing := MissingDepNames(deps)
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("%s %s (%s)\n", DepNoticePrefix, strings.Join(missing, "; "), DepNoticeHint)
}
