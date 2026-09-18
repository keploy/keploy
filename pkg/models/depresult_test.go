package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDepRowName(t *testing.T) {
	tests := []struct {
		name    string
		index   int
		depType string
		target  string
		want    string
	}{
		{name: "full row", index: 0, depType: "postgres", target: "orders", want: "deps[0] postgres orders (presence)"},
		{name: "no target", index: 3, depType: "redis", target: "", want: "deps[3] redis (presence)"},
		{name: "no type", index: 1, depType: "", target: "db:5432", want: "deps[1] db:5432 (presence)"},
		{name: "neither", index: 2, depType: "", target: "", want: "deps[2] (presence)"},
		{
			name: "two calls to the same host stay distinguishable",
			// The regression this guards: an HTTP target that collapses to the
			// bare host makes every call to one service the same row name.
			index: 4, depType: "http", target: "GET api.internal:8080/orders",
			want: "deps[4] http GET api.internal:8080/orders (presence)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DepRowName(tt.index, tt.depType, tt.target); got != tt.want {
				t.Errorf("DepRowName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDepTypeForKind(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		// The whole point: parser versions collapse onto one family, so an
		// agent keying on "postgres" cannot miss a v2/v3 recording.
		{kind: Postgres, want: "postgres"},
		{kind: PostgresV2, want: "postgres"},
		{kind: PostgresV3, want: "postgres"},
		{kind: HTTP, want: "http"},
		{kind: HTTP2, want: "http"},
		{kind: GRPC_EXPORT, want: "grpc"},
		// Kinds whose lowercase form already IS the family.
		{kind: MySQL, want: "mysql"},
		{kind: Mongo, want: "mongo"},
		{kind: REDIS, want: "redis"},
		{kind: KAFKA, want: "kafka"},
		{kind: DNS, want: "dns"},
		{kind: GENERIC, want: "generic"},
		{kind: Aerospike, want: "aerospike"},
		{kind: RevokedTests, want: "keploy-revoked-tests"},
		// Degenerate input must not produce a half-formed row name.
		{kind: Kind(""), want: ""},
		{kind: Kind("  Http  "), want: "http"},
		{kind: Kind("SomeFutureProtocol"), want: "somefutureprotocol"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := DepTypeForKind(tt.kind); got != tt.want {
				t.Errorf("DepTypeForKind(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

// kindConstantsInMockGo parses mock.go and returns every top-level constant
// declared with the explicit type Kind, so the table above cannot silently
// fall behind a newly added protocol.
func kindConstantsInMockGo(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mock.go", nil, 0)
	if err != nil {
		t.Fatalf("parse mock.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Kind" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				out[name.Name] = strings.Trim(lit.Value, `"`)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Kind constants in mock.go — the parser above is broken, not the code")
	}
	return out
}

// Every Kind constant declared in mock.go must have an explicit expectation in
// TestDepTypeForKind. A new version-suffixed Kind (the PostgresV4 case) would
// otherwise leak its parser version into the DepResult `type` contract and
// into the row name, both of which are one-way doors.
func TestDepTypeForKind_CoversEveryKind(t *testing.T) {
	// Mirror of the table in TestDepTypeForKind, keyed by constant NAME so
	// adding a constant here is a deliberate act.
	covered := map[string]string{
		"HTTP":         "http",
		"HTTP2":        "http",
		"GENERIC":      "generic",
		"REDIS":        "redis",
		"KAFKA":        "kafka",
		"MySQL":        "mysql",
		"Postgres":     "postgres",
		"PostgresV2":   "postgres",
		"PostgresV3":   "postgres",
		"GRPC_EXPORT":  "grpc",
		"Mongo":        "mongo",
		"DNS":          "dns",
		"Aerospike":    "aerospike",
		"RevokedTests": "keploy-revoked-tests",
	}

	declared := kindConstantsInMockGo(t)
	for name, value := range declared {
		want, ok := covered[name]
		if !ok {
			t.Errorf("models.Kind constant %s (%q) is not covered by TestDepTypeForKind. "+
				"Add it to both tables and check DepTypeForKind maps it to a protocol FAMILY, "+
				"not a parser version — `type` is part of the NDJSON contract and of the row name.",
				name, value)
			continue
		}
		if got := DepTypeForKind(Kind(value)); got != want {
			t.Errorf("DepTypeForKind(%s = %q) = %q, want %q", name, value, got, want)
		}
	}
	for name := range covered {
		if _, ok := declared[name]; !ok {
			t.Errorf("the coverage table names %s, which no longer exists in mock.go", name)
		}
	}
}

// The consumed side is TWO SCALARS on the result, not rows. An earlier
// revision of this slice persisted an aggregate `deps[*] N consumed` DepResult
// row unconditionally, which cost +224 bytes of nested YAML per test on EVERY
// report — including the ones where nothing is missing and the verdict knob is
// off — to encode one bit and one small int. This pins the cheap encoding, and
// pins that DependenciesChecked reads the BIT rather than inferring it from
// len(DepResult): inferring is what forced the aggregate row to exist.
func TestDependenciesCheckedReadsTheBitNotTheRows(t *testing.T) {
	missing := DepResult{Name: "deps[0] postgres orders (presence)", Meta: []DepMetaResult{
		{Normal: false, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceMissing},
	}}

	tests := []struct {
		name        string
		result      Result
		wantChecked bool
		wantMissing bool
		why         string
	}{
		{
			name:   "never checked: the assertion did not run",
			result: Result{},
			why: "--base-path / remote-agent, --disable-mapping, no mappings.yaml, a failed " +
				"consumed-mock fetch or a pre-slice-4 report. `dep_result: []` on its own " +
				"cannot say this.",
		},
		{
			name:        "checked and clean: nothing missing, nothing consumed",
			result:      Result{DepsChecked: true},
			wantChecked: true,
			why: "The whole reason DepsChecked exists. Byte-identical to the row above on " +
				"dep_result alone, and a consumer applying `any(matched == false)` would call " +
				"both of them green.",
		},
		{
			name:        "checked and clean with a consumed count",
			result:      Result{DepsChecked: true, DepsConsumed: 20},
			wantChecked: true,
		},
		{
			name:        "checked and lossy",
			result:      Result{DepsChecked: true, DepsConsumed: 3, DepResult: []DepResult{missing}},
			wantChecked: true,
			wantMissing: true,
		},
		{
			name:        "rows without the bit are still not proof the assertion ran",
			result:      Result{DepResult: []DepResult{missing}},
			wantMissing: true,
			why: "len(DepResult) > 0 must never be read as 'checked' — that inference is what " +
				"the deleted aggregate row existed to prop up.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.DependenciesChecked(); got != tt.wantChecked {
				t.Errorf("DependenciesChecked() = %v, want %v. %s", got, tt.wantChecked, tt.why)
			}
			if got := tt.result.HasMissingDeps(); got != tt.wantMissing {
				t.Errorf("HasMissingDeps() = %v, want %v", got, tt.wantMissing)
			}
		})
	}
}

// A clean CHECKED result must serialize with no dependency rows at all, so a
// report whose tests lose nothing stays byte-compatible with a pre-slice-4
// report apart from one short key. This is the hard constraint the aggregate
// row violated; it is pinned on the YAML bytes, not on the struct.
func TestCleanCheckedResultSerializesWithoutRows(t *testing.T) {
	clean, err := yaml.Marshal(Result{DepsChecked: true, DepsConsumed: 20})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	unchecked, err := yaml.Marshal(Result{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !strings.Contains(string(clean), "dep_result: []") {
		t.Errorf("a checked-and-clean result must still serialize `dep_result: []`:\n%s", clean)
	}
	if strings.Contains(string(clean), "deps[") {
		t.Errorf("a checked-and-clean result must persist NO dependency rows:\n%s", clean)
	}
	for _, want := range []string{"deps_checked: true", "deps_consumed: 20"} {
		if !strings.Contains(string(clean), want) {
			t.Errorf("missing %q:\n%s", want, clean)
		}
	}

	// omitempty: an UNCHECKED result — every pre-slice-4 report and every mode
	// that cannot arm the mapping — must not grow either key.
	for _, bad := range []string{"deps_checked", "deps_consumed"} {
		if strings.Contains(string(unchecked), bad) {
			t.Errorf("an unchecked result must not serialize %q; existing reports would change:\n%s",
				bad, unchecked)
		}
	}

	// The measured cost of the bit, pinned so a future revision cannot quietly
	// re-inflate it. The deleted aggregate row cost +224 B per test.
	if grew := len(clean) - len(unchecked); grew > 64 {
		t.Errorf("a checked-and-clean result costs %d bytes over an unchecked one; "+
			"the budget for encoding 'checked, N consumed' is one bit and one small int, "+
			"not a nested row:\n%s", grew, clean)
	}
}

// The overflow row is what keeps the flagship "every dependency vanished"
// shape from writing a 5.4 MB test-set report. It must still read as a
// FAILURE, or capping the missing side would silently un-report the thing the
// slice exists to report.
// The wire vocabulary, pinned as LITERALS. Everything else in this package
// asserts these constants against themselves — renaming DepKeyMissingCount to
// "presence" left the whole suite green — while `key` is a field of the NDJSON
// contract (`effects[].key`) and of the persisted report YAML, which agents
// and the k8s-proxy report handler both read. Changing one of these strings is
// a schema change; it has to be a deliberate fixture update.
func TestDepVocabularyLiterals(t *testing.T) {
	for _, tt := range []struct{ got, want, what string }{
		{DepKeyPresence, "presence", "DepKeyPresence"},
		{DepKeyMissingCount, "missing_count", "DepKeyMissingCount"},
		{DepPresenceConsumed, "consumed", "DepPresenceConsumed"},
		{DepPresenceMissing, "not consumed", "DepPresenceMissing"},
		{DepNameSuffixPresence, "(presence)", "DepNameSuffixPresence"},
		{DepSummaryIndex, "*", "DepSummaryIndex"},
		{DepNoticePrefix, "DEPENDENCY NOT EXERCISED:", "DepNoticePrefix"},
		{DepNoticeHint, "reported only — run with --assert-dependencies to fail on this", "DepNoticeHint"},
		{DepTypeHTTP, "http", "DepTypeHTTP"},
		{DepTypePostgres, "postgres", "DepTypePostgres"},
		{DepTypeGRPC, "grpc", "DepTypeGRPC"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q. These strings are on the wire (NDJSON effects[].key / "+
				"effects[].type and the persisted dep_result YAML) and in the CI-scraped notice "+
				"line; changing one is a contract change, not a rename.", tt.what, tt.got, tt.want)
		}
	}
	// The overflow key must stay distinct from the per-dependency key, or
	// IsDepMissingOverflow stops telling "this named dependency went missing"
	// apart from "3 more did".
	if DepKeyMissingCount == DepKeyPresence {
		t.Fatal("the overflow and presence keys are the same string")
	}
}

func TestDepMissingOverflowRow(t *testing.T) {
	row := DepMissingOverflowRow(150)

	// The literal, not the builders: this name is on the wire (NDJSON
	// `effects[].name`), in the persisted YAML and in the rendered block.
	if row.Name != "deps[*] 150 more not consumed (presence)" {
		t.Errorf("Name = %q", row.Name)
	}
	if row.Type != "" {
		t.Errorf("the overflow spans protocols so Type must stay empty, got %q", row.Type)
	}
	if !IsDepMissingOverflow(row) {
		t.Error("IsDepMissingOverflow must recognise its own row")
	}
	if DepRowMatched(row) {
		t.Fatal("the overflow row must count as a FAILED assertion, otherwise capping the " +
			"missing side hides the missing dependencies from HasMissingDeps, MissingDepNames, " +
			"the k8s-proxy report handler and the NDJSON matched==false rule")
	}
	if len(row.Meta) != 1 || row.Meta[0].Key != DepKeyMissingCount ||
		row.Meta[0].Expected != "0" || row.Meta[0].Actual != "150" {
		t.Errorf("meta = %+v", row.Meta)
	}

	res := Result{DepsChecked: true, DepResult: []DepResult{row}}
	if !res.HasMissingDeps() {
		t.Error("a result carrying only the overflow row must report missing dependencies")
	}
	if names := MissingDepNames(res.DepResult); len(names) != 1 || names[0] != row.Name {
		t.Errorf("MissingDepNames = %v", names)
	}
}

// The overflow row shares the `deps[*] ... (presence)` grammar, which is the
// family marker every sync-path row carries (see DepNameSuffixPresence). It is
// told apart from a per-dependency row by its Meta key, never by the name.
func TestAggregateRowsShareTheSyncPathNameGrammar(t *testing.T) {
	for _, row := range []DepResult{DepMissingOverflowRow(3)} {
		if !strings.HasPrefix(row.Name, "deps["+DepSummaryIndex+"] ") {
			t.Errorf("%q does not use the aggregate index token", row.Name)
		}
		if !strings.HasSuffix(row.Name, " "+DepNameSuffixPresence) {
			t.Errorf("%q does not carry the sync-path family marker", row.Name)
		}
	}
}

func TestDepRowMatchedAndMissingDepNames(t *testing.T) {
	missing := DepResult{Name: "deps[0] postgres orders (presence)", Type: "postgres", Meta: []DepMetaResult{
		{Normal: false, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceMissing},
	}}
	ok := DepResult{Name: "deps[1] http api (presence)", Type: "http", Meta: []DepMetaResult{
		{Normal: true, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceConsumed},
	}}
	noMeta := DepResult{Name: "deps[2] redis (presence)", Type: "redis"}

	tests := []struct {
		name      string
		result    Result
		wantHas   bool
		wantNames []string
	}{
		{name: "no rows", result: Result{}, wantHas: false},
		{name: "all matched", result: Result{DepResult: []DepResult{ok, noMeta}}, wantHas: false},
		{
			name: "one missing", result: Result{DepResult: []DepResult{ok, missing}},
			wantHas: true, wantNames: []string{"deps[0] postgres orders (presence)"},
		},
		{
			name:   "a checked result with no rows has no missing dep",
			result: Result{DepsChecked: true, DepsConsumed: 12},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.HasMissingDeps(); got != tt.wantHas {
				t.Errorf("HasMissingDeps() = %v, want %v", got, tt.wantHas)
			}
			names := MissingDepNames(tt.result.DepResult)
			if len(names) != len(tt.wantNames) {
				t.Fatalf("MissingDepNames() = %v, want %v", names, tt.wantNames)
			}
			for i := range names {
				if names[i] != tt.wantNames[i] {
					t.Errorf("MissingDepNames()[%d] = %q, want %q", i, names[i], tt.wantNames[i])
				}
			}
		})
	}

	if !DepRowMatched(noMeta) {
		t.Error("a row with no assertions must count as matched")
	}
	if DepRowMatched(missing) {
		t.Error("a row with a failed assertion must not count as matched")
	}
}

func TestFormatDepResults(t *testing.T) {
	tests := []struct {
		name      string
		deps      []DepResult
		wantEmpty bool
		contains  []string
		absent    []string
	}{
		{
			name:      "no rows renders nothing at all",
			deps:      nil,
			wantEmpty: true,
		},
		{
			name: "missing row renders expected vs actual",
			deps: []DepResult{{
				Name: "deps[0] postgres orders (presence)",
				Type: "postgres",
				Meta: []DepMetaResult{{Normal: false, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceMissing}},
			}},
			contains: []string{
				"=== DEPENDENCY ASSERTIONS ===",
				"MISSING deps[0] postgres orders (presence)",
				`presence: expected "consumed", actual "not consumed"`,
			},
			absent: []string{"consumed (presence only)"},
		},
		{
			name: "matched row collapses to one compact line",
			deps: []DepResult{{
				Name: "deps[0] http GET api.internal:80/orders (presence)",
				Type: "http",
				Meta: []DepMetaResult{{Normal: true, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceConsumed}},
			}},
			contains: []string{"OK      deps[0] http GET api.internal:80/orders (presence) - consumed (presence only)"},
			absent:   []string{"MISSING"},
		},
		{
			// The overflow row already reads "<n> more not consumed";
			// restating it as `missing_count: expected "0", actual "150"`
			// would stutter.
			name:     "the missing overflow does not stutter",
			deps:     []DepResult{DepMissingOverflowRow(150)},
			contains: []string{"MISSING deps[*] 150 more not consumed (presence)"},
			absent:   []string{"missing_count:"},
		},
		{
			name: "a meta with no key falls back to the presence key",
			deps: []DepResult{{
				Name: "deps[0] kafka orders (presence)",
				Type: "kafka",
				Meta: []DepMetaResult{{Normal: false, Expected: "1", Actual: "0"}},
			}},
			contains: []string{`presence: expected "1", actual "0"`},
		},
		{
			name: "matched metas inside a failed row are not printed twice",
			deps: []DepResult{{
				Name: "deps[0] kafka orders (presence)",
				Type: "kafka",
				Meta: []DepMetaResult{
					{Normal: true, Key: "protocol", Expected: "kafka", Actual: "kafka"},
					{Normal: false, Key: "presence", Expected: "consumed", Actual: "not consumed"},
				},
			}},
			contains: []string{`presence: expected "consumed", actual "not consumed"`},
			absent:   []string{"protocol:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDepResults(tt.deps)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("expected empty output, got %q", got)
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(got, bad) {
					t.Errorf("output unexpectedly contains %q:\n%s", bad, got)
				}
			}
		})
	}
}

func TestFormatDepNotice(t *testing.T) {
	missing := func(name string) DepResult {
		return DepResult{Name: name, Meta: []DepMetaResult{
			{Normal: false, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceMissing},
		}}
	}
	consumed := DepResult{Name: "deps[0] http x (presence)", Meta: []DepMetaResult{
		{Normal: true, Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceConsumed},
	}}

	tests := []struct {
		name string
		deps []DepResult
		want string
	}{
		{name: "nothing missing renders nothing", deps: nil, want: ""},
		{name: "everything consumed renders nothing", deps: []DepResult{consumed}, want: ""},
		{
			name: "one missing dependency is one line",
			deps: []DepResult{missing("deps[0] postgres INSERT (presence)")},
			want: DepNoticePrefix + " deps[0] postgres INSERT (presence) (" + DepNoticeHint + ")\n",
		},
		{
			name: "several missing dependencies stay on one line",
			deps: []DepResult{missing("deps[0] a (presence)"), missing("deps[1] b (presence)")},
			want: DepNoticePrefix + " deps[0] a (presence); deps[1] b (presence) (" + DepNoticeHint + ")\n",
		},
		{
			// THE LITERAL, not the constants. Every other assertion on this
			// line writes `DepNoticePrefix + ...`, i.e. compares the constant
			// to itself: changing DepNoticePrefix to "DEPS:" left the whole
			// suite green even though its own doc calls it a stable grep
			// contract for CI log scrapers and the agent loop. Pinning the
			// bytes once makes a change to either constant a deliberate
			// fixture update. Same treatment goldenNDJSONLine gives
			// schema_version.
			name: "the rendered line is byte-stable (grep contract)",
			deps: []DepResult{missing("deps[0] postgres INSERT (presence)")},
			want: "DEPENDENCY NOT EXERCISED: deps[0] postgres INSERT (presence) " +
				"(reported only — run with --assert-dependencies to fail on this)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatDepNotice(tt.deps); got != tt.want {
				t.Errorf("FormatDepNotice() = %q, want %q", got, tt.want)
			}
		})
	}
}
