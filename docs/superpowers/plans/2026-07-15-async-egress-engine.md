# Async Egress Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a transport-agnostic async-egress engine to keploy so that user-declared "lanes" of background/long-poll traffic are marked inline at record, anchored to testcase position, and served in order at replay (with a request-shape verdict), starting with HTTP and pluggable to Mongo/Kafka later.

**Architecture:** Async is metadata (`async`/`lane`/`anchorAfter`/`anchorPos`/`asyncSeq`) stamped onto ordinary egress mocks by a record hook, on the sole authority of config lanes. A transport-agnostic `Engine` (hung on `*proxy.Proxy`) serves those mocks at replay: gated by a monotonic `completed` counter advanced per testcase, ordered by `asyncSeq`, verified via a parser-supplied `MatchRequestShape`, with a parser-supplied keep-alive when nothing is armed. Parsers opt in by implementing the `AsyncParser` capability interface (the same opt-in idiom as `IntegrationsV2`).

**Tech Stack:** Go, keploy proxy integrations, `zap` logging, `path.Match` globbing, existing HTTP matcher (`SchemaMatch`), viper + yaml.v3 config, testify-style table tests.

## Global Constraints

- Language: Go (module `go.keploy.io/server/v3`); local checkout root `/home/aditya/flipkart/asyncHttpApproach/keploy`.
- Shared config sub-types live in `pkg/models` (precedent: `models.BypassRule`, `models.Filter`) — never make `config` import a proxy subpackage, and never make a parser import `config`.
- `integrations.Integrations` interface signature is FIXED — do not add parameters to `MatchType`/`RecordOutgoing`/`MockOutgoing`. New capabilities are separate opt-in interfaces resolved by type assertion.
- Zero-impact guarantee: when `cfg.Async.Lanes` is empty, record and replay MUST be byte-identical to today (the record hook must not be installed, and the engine must be nil/no-op).
- HTTP mock `Kind` constant is `models.HTTP == "Http"`. Metadata is the flat map `mock.Spec.Metadata` (`map[string]string`); nil-check/init before writing.
- HTTP request/response on a mock are POINTERS: `mock.Spec.HTTPReq *models.HTTPReq`, `mock.Spec.HTTPResp *models.HTTPResp` — nil-check before deref.
- Commit after every task with a `feat:`/`test:` message; run `gofmt`/`go vet` before each commit.

---

## File Structure

**Create:**
- `pkg/models/async.go` — `AsyncLane` struct + async metadata key constants (shared, importable everywhere).
- `pkg/agent/proxy/integrations/async/async.go` — `AsyncParser` + `AsyncAware` interfaces.
- `pkg/agent/proxy/integrations/async/engine.go` — `Engine`, `laneStream`, `Report`.
- `pkg/agent/proxy/integrations/async/engine_test.go` — transport-agnostic engine tests + pluggability proof (fake parser).
- `pkg/agent/proxy/integrations/http/async.go` — HTTP's `AsyncParser` implementation.
- `pkg/agent/proxy/integrations/http/async_test.go` — HTTP async unit tests.
- `pkg/service/record/asynchook.go` — `AsyncRecorder` (a `RecordHooks` impl).
- `pkg/service/record/asynchook_test.go` — record-side marking tests.

**Modify:**
- `config/config.go` — add `Async` field + `Async` section struct (near line 47).
- `config/default.go` — add `async:` block to the `defaultConfig` template.
- `pkg/agent/proxy/proxy.go` — `Proxy.asyncEngine` field (near line 158); build in `New` (line 650); inject in `InitIntegrations` (line 992); advance in `SetMocksWithWindow` (line 2796).
- `pkg/agent/proxy/integrations/http/decode.go` — lane-routing branch before `h.match(...)` (line 247).
- `cli/provider/core_service.go` — install `AsyncRecorder` when lanes configured (near line 44).
- `pkg/service/replay/replay.go` — final position advance at test-set teardown (in `RunTestSet`, after the loop).

---

## Task 1: Shared types + config wiring

**Files:**
- Create: `pkg/models/async.go`
- Modify: `config/config.go` (add field near line 47; add `Async` struct beside `Agent`)
- Modify: `config/default.go` (add `async:` sub-tree to `defaultConfig`, ~line 33+)
- Test: `config/async_config_test.go` (create)

**Interfaces:**
- Produces: `models.AsyncLane{Name string; Type string; Match map[string]string; VolatileParams []string; NotExercised string}`; metadata key consts `models.MetaAsync`, `models.MetaAsyncLane`, `models.MetaAnchorAfter`, `models.MetaAnchorPos`, `models.MetaAsyncSeq`; `config.Async{Lanes []models.AsyncLane}` reachable at `cfg.Async.Lanes`.

- [ ] **Step 1: Write the failing test**

Create `config/async_config_test.go`:

```go
package config

import (
	"testing"

	yaml3 "gopkg.in/yaml.v3"
)

func TestAsyncLanesUnmarshalFromYAML(t *testing.T) {
	src := `
async:
  lanes:
    - name: notifications
      type: http
      match:
        host: "notify.internal.svc"
        path: "/v1/poll*"
      volatileParams: ["cursor"]
      notExercised: skip
`
	var c Config
	if err := yaml3.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(c.Async.Lanes) != 1 {
		t.Fatalf("want 1 lane, got %d", len(c.Async.Lanes))
	}
	l := c.Async.Lanes[0]
	if l.Name != "notifications" || l.Type != "http" {
		t.Fatalf("bad lane header: %+v", l)
	}
	if l.Match["host"] != "notify.internal.svc" || l.Match["path"] != "/v1/poll*" {
		t.Fatalf("bad match block: %+v", l.Match)
	}
	if len(l.VolatileParams) != 1 || l.VolatileParams[0] != "cursor" {
		t.Fatalf("bad volatileParams: %+v", l.VolatileParams)
	}
	if l.NotExercised != "skip" {
		t.Fatalf("bad notExercised: %q", l.NotExercised)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./config/ -run TestAsyncLanesUnmarshalFromYAML -v`
Expected: FAIL — `c.Async undefined (type Config has no field or method Async)`.

- [ ] **Step 3: Create the shared model types**

Create `pkg/models/async.go`:

```go
package models

// Async metadata keys stamped on an ordinary egress mock's Spec.Metadata
// when it matches a declared async lane at record time.
const (
	MetaAsync       = "async"       // "true"
	MetaAsyncLane   = "lane"        // lane name
	MetaAnchorAfter = "anchorAfter" // last completed testcase Name, or "startup" (readability)
	MetaAnchorPos   = "anchorPos"   // number of testcases completed before this egress fired (decimal)
	MetaAsyncSeq    = "asyncSeq"    // per-lane order counter (decimal)
)

// AnchorStartup is the MetaAnchorAfter value for async mocks that fired
// before the first testcase completed.
const AnchorStartup = "startup"

// AsyncLane is one declared async lane. Match is opaque to the engine and
// interpreted by the owning parser's MatchesLane. Lives in models so both
// config (yaml) and the proxy async package can reference it without an
// import cycle (mirrors BypassRule / Filter).
type AsyncLane struct {
	Name           string            `json:"name" yaml:"name" mapstructure:"name"`
	Type           string            `json:"type" yaml:"type" mapstructure:"type"` // owning parser, e.g. "http"
	Match          map[string]string `json:"match" yaml:"match" mapstructure:"match"`
	VolatileParams []string          `json:"volatileParams,omitempty" yaml:"volatileParams,omitempty" mapstructure:"volatileParams"`
	NotExercised   string            `json:"notExercised,omitempty" yaml:"notExercised,omitempty" mapstructure:"notExercised"` // skip|fail (default skip)
}
```

- [ ] **Step 4: Add the config section**

In `config/config.go`, add the field to `type Config struct` right after the `Agent` field (line 47):

```go
	Async     Async     `json:"async" yaml:"async" mapstructure:"async"`
```

Add the section struct beside `type Agent struct` (after line ~66):

```go
// Async configures the async-egress engine. Empty Lanes => feature off,
// record & replay byte-identical to today.
type Async struct {
	Lanes []models.AsyncLane `json:"lanes" yaml:"lanes" mapstructure:"lanes"`
}
```

(`models` is already imported in config.go — confirm; it is used for `BypassRule`/`Filter`.)

- [ ] **Step 5: Add the default-config template block**

In `config/default.go`, inside the `defaultConfig` raw string (the `test:`/`record:` sub-trees near line 33+), add a sibling block:

```yaml
async:
    lanes: []
```

- [ ] **Step 6: Run test to verify it passes**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./config/ -run TestAsyncLanesUnmarshalFromYAML -v && go build ./...`
Expected: PASS; build succeeds.

- [ ] **Step 7: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/models/async.go config/config.go config/async_config_test.go
git add pkg/models/async.go config/config.go config/default.go config/async_config_test.go
git commit -m "feat(async): add AsyncLane model + config.Async section"
```

---

## Task 2: async package — capability interfaces

**Files:**
- Create: `pkg/agent/proxy/integrations/async/async.go`
- Test: `pkg/agent/proxy/integrations/async/async_test.go`

**Interfaces:**
- Consumes: `models.AsyncLane`, `models.Mock`.
- Produces:
  - `async.AsyncParser` interface:
    - `MatchesLane(m *models.Mock, lane models.AsyncLane) bool`
    - `MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (ok bool, detail string)`
    - `EmptyResponse(lane models.AsyncLane) ([]byte, error)`
  - `async.AsyncAware` interface: `SetAsyncEngine(e *async.Engine)` (defined here as a forward decl via `*Engine` from engine.go, same package).

- [ ] **Step 1: Write the failing test**

Create `pkg/agent/proxy/integrations/async/async_test.go`:

```go
package async

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeParser is a transport-agnostic stand-in used across async tests.
type fakeParser struct {
	matches   bool
	shapeOK   bool
	empty     []byte
}

func (f *fakeParser) MatchesLane(_ *models.Mock, _ models.AsyncLane) bool { return f.matches }
func (f *fakeParser) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	if f.shapeOK {
		return true, ""
	}
	return false, "shape drift"
}
func (f *fakeParser) EmptyResponse(_ models.AsyncLane) ([]byte, error) { return f.empty, nil }

// compile-time assertion the fake satisfies the interface.
var _ AsyncParser = (*fakeParser)(nil)

func TestFakeParserSatisfiesInterface(t *testing.T) {
	var p AsyncParser = &fakeParser{matches: true, shapeOK: true, empty: []byte("empty")}
	if !p.MatchesLane(nil, models.AsyncLane{}) {
		t.Fatal("MatchesLane should be true")
	}
	if ok, _ := p.MatchRequestShape(nil, nil, models.AsyncLane{}); !ok {
		t.Fatal("MatchRequestShape should be ok")
	}
	b, _ := p.EmptyResponse(models.AsyncLane{})
	if string(b) != "empty" {
		t.Fatalf("EmptyResponse = %q", b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/integrations/async/ -run TestFakeParserSatisfiesInterface -v`
Expected: FAIL — `undefined: AsyncParser`.

- [ ] **Step 3: Write the interfaces**

Create `pkg/agent/proxy/integrations/async/async.go`:

```go
// Package async implements keploy's transport-agnostic async-egress engine.
// Parsers opt in to async handling by implementing AsyncParser; the proxy
// injects the shared Engine into opted-in parsers via AsyncAware.
package async

import "go.keploy.io/server/v3/pkg/models"

// AsyncParser is the capability interface a protocol parser implements to
// participate in async egress. The engine holds only *models.Mock and
// models.AsyncLane, delegating every protocol decision here — this is what
// keeps the engine transport-agnostic.
type AsyncParser interface {
	// MatchesLane reports whether the egress (recorded mock at record time,
	// or a live request wrapped as a mock at replay) belongs to the lane.
	MatchesLane(m *models.Mock, lane models.AsyncLane) bool

	// MatchRequestShape compares a live request against a recorded async
	// mock's request, treating lane.VolatileParams as noise. ok=false with a
	// human-readable detail on drift.
	MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (ok bool, detail string)

	// EmptyResponse returns the parser's "no data yet" keep-alive payload for
	// the lane, served when no async mock is armed. Always synthesizable.
	EmptyResponse(lane models.AsyncLane) ([]byte, error)
}

// AsyncAware is an optional interface a parser implements so the proxy can
// hand it the shared Engine at InitIntegrations time (setter injection,
// mirroring the IntegrationsV2 capability idiom).
type AsyncAware interface {
	SetAsyncEngine(e *Engine)
}
```

NOTE: this file references `*Engine` (defined in Task 3, same package). Task 2 and Task 3 compile together; if implementing strictly in order, add a temporary `type Engine struct{}` at the bottom of `async.go` and delete it in Task 3 Step 3. (The provided engine_test in Task 3 will fail until Task 3 is done.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/integrations/async/ -run TestFakeParserSatisfiesInterface -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/agent/proxy/integrations/async/
git add pkg/agent/proxy/integrations/async/
git commit -m "feat(async): add AsyncParser + AsyncAware capability interfaces"
```

---

## Task 3: Engine core (transport-agnostic)

**Files:**
- Create: `pkg/agent/proxy/integrations/async/engine.go`
- Test: `pkg/agent/proxy/integrations/async/engine_test.go`

**Interfaces:**
- Consumes: `async.AsyncParser`, `models.AsyncLane`, `models.Mock`, metadata consts from `models`.
- Produces:
  - `func NewEngine(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]AsyncParser) *Engine`
  - `func (e *Engine) Load(mocks []*models.Mock)` — build per-lane streams from async-tagged mocks (sorted by `asyncSeq`).
  - `func (e *Engine) OnTestComplete()` — increment the completed counter.
  - `func (e *Engine) LaneFor(m *models.Mock) (models.AsyncLane, bool)` — which lane a live request routes to (first parser whose MatchesLane is true).
  - `func (e *Engine) Decide(lane models.AsyncLane, live *models.Mock) (recorded *models.Mock, keepAlive []byte, err error)` — returns exactly one of `recorded` (serve it) or `keepAlive` (serve keep-alive).
  - `func (e *Engine) Report() ReportSnapshot` — `{Pass, Flag, NotExercised int; Flags []string}`.

- [ ] **Step 1: Write the failing tests**

Create `pkg/agent/proxy/integrations/async/engine_test.go`:

```go
package async

import (
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func asyncMock(lane string, seq, anchorPos int, respBody string) *models.Mock {
	return &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				models.MetaAsync:     "true",
				models.MetaAsyncLane: lane,
				models.MetaAnchorPos: strconv.Itoa(anchorPos),
				models.MetaAsyncSeq:  strconv.Itoa(seq),
			},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: respBody},
		},
	}
}

func newTestEngine(p AsyncParser) *Engine {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	return NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"fake": p})
}

func TestServesInSeqOrderWhenArmed(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{
		asyncMock("L", 2, 0, "b"),
		asyncMock("L", 1, 0, "a"), // out of order on purpose
	})
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	got1, _, _ := e.Decide(lane, &models.Mock{})
	got2, _, _ := e.Decide(lane, &models.Mock{})
	if got1.Spec.HTTPResp.Body != "a" || got2.Spec.HTTPResp.Body != "b" {
		t.Fatalf("want a then b, got %q then %q", got1.Spec.HTTPResp.Body, got2.Spec.HTTPResp.Body)
	}
}

func TestGatedByAnchorPosition(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 1, "after-T1")}) // anchorPos=1
	lane := models.AsyncLane{Name: "L", Type: "fake"}

	rec, ka, _ := e.Decide(lane, &models.Mock{}) // completed=0 -> not armed
	if rec != nil || string(ka) != "KA" {
		t.Fatalf("before anchor: want keep-alive, got rec=%v ka=%q", rec, ka)
	}
	e.OnTestComplete() // completed=1
	rec, ka, _ = e.Decide(lane, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "after-T1" {
		t.Fatalf("after anchor: want the mock, got rec=%v ka=%q", rec, ka)
	}
}

func TestStartupArmedImmediately(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "boot")}) // anchorPos=0 (startup)
	rec, _, _ := e.Decide(models.AsyncLane{Name: "L", Type: "fake"}, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "boot" {
		t.Fatalf("startup mock should be armed immediately, got %v", rec)
	}
}

func TestKeepAliveWhenDrained(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "a")})
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	_, _, _ = e.Decide(lane, &models.Mock{}) // consume the only mock
	rec, ka, _ := e.Decide(lane, &models.Mock{})
	if rec != nil || string(ka) != "KA" {
		t.Fatalf("drained lane should keep-alive, got rec=%v ka=%q", rec, ka)
	}
}

func TestShapeMismatchServesAndFlags(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: false, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "a")})
	rec, _, _ := e.Decide(models.AsyncLane{Name: "L", Type: "fake"}, &models.Mock{})
	if rec == nil {
		t.Fatal("mismatch must still serve the recorded mock")
	}
	if snap := e.Report(); snap.Flag != 1 || snap.Pass != 0 {
		t.Fatalf("want 1 flag 0 pass, got %+v", snap)
	}
}

// Pluggability proof: a SECOND, different fake parser drives the same engine
// with zero engine changes.
type otherFake struct{ fakeParser }

func TestPluggableSecondTransport(t *testing.T) {
	lane := models.AsyncLane{Name: "K", Type: "other"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane},
		map[string]AsyncParser{"other": &otherFake{fakeParser{matches: true, shapeOK: true, empty: []byte("EK")}}})
	e.Load([]*models.Mock{asyncMock("K", 1, 0, "kafka-ish")})
	rec, _, _ := e.Decide(lane, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "kafka-ish" {
		t.Fatalf("engine must serve any transport unchanged, got %v", rec)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/integrations/async/ -run 'TestServes|TestGated|TestStartup|TestKeepAlive|TestShape|TestPluggable' -v`
Expected: FAIL — `undefined: NewEngine`.

- [ ] **Step 3: Implement the engine**

Delete any temporary `type Engine struct{}` placeholder from `async.go` (Task 2). Create `pkg/agent/proxy/integrations/async/engine.go`:

```go
package async

import (
	"sort"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type laneStream struct {
	lane   models.AsyncLane
	mocks  []*models.Mock // sorted by asyncSeq
	cursor int            // next unconsumed index
}

// ReportSnapshot is a point-in-time copy of the engine's verdict tallies.
type ReportSnapshot struct {
	Pass         int
	Flag         int
	NotExercised int
	Flags        []string
}

// Engine is transport-agnostic: it holds only *models.Mock, models.AsyncLane,
// and AsyncParser delegates. Safe for concurrent use.
type Engine struct {
	logger  *zap.Logger
	lanes   map[string]models.AsyncLane // by name
	parsers map[string]AsyncParser      // by lane.Type

	mu        sync.Mutex
	streams   map[string]*laneStream // by lane name
	completed int                    // number of testcases completed

	pass, flag int
	flags      []string
}

func NewEngine(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]AsyncParser) *Engine {
	lm := make(map[string]models.AsyncLane, len(lanes))
	for _, l := range lanes {
		lm[l.Name] = l
	}
	return &Engine{
		logger:  logger,
		lanes:   lm,
		parsers: parsers,
		streams: make(map[string]*laneStream),
	}
}

// Load partitions async-tagged mocks into per-lane, seq-ordered streams.
func (e *Engine) Load(mocks []*models.Mock) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range mocks {
		if m == nil || m.Spec.Metadata[models.MetaAsync] != "true" {
			continue
		}
		name := m.Spec.Metadata[models.MetaAsyncLane]
		lane, ok := e.lanes[name]
		if !ok {
			continue
		}
		s := e.streams[name]
		if s == nil {
			s = &laneStream{lane: lane}
			e.streams[name] = s
		}
		s.mocks = append(s.mocks, m)
	}
	for _, s := range e.streams {
		sort.SliceStable(s.mocks, func(i, j int) bool {
			return seqOf(s.mocks[i]) < seqOf(s.mocks[j])
		})
	}
}

func (e *Engine) OnTestComplete() {
	e.mu.Lock()
	e.completed++
	e.mu.Unlock()
}

// LaneFor returns the lane a live request routes to, by asking each lane's
// parser. First match wins.
func (e *Engine) LaneFor(m *models.Mock) (models.AsyncLane, bool) {
	for _, lane := range e.lanes {
		p := e.parsers[lane.Type]
		if p != nil && p.MatchesLane(m, lane) {
			return lane, true
		}
	}
	return models.AsyncLane{}, false
}

// Decide returns the recorded mock to serve, or a keep-alive payload.
func (e *Engine) Decide(lane models.AsyncLane, live *models.Mock) (*models.Mock, []byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	p := e.parsers[lane.Type]
	s := e.streams[lane.Name]
	if s != nil && s.cursor < len(s.mocks) && e.isArmedLocked(s.mocks[s.cursor]) {
		recorded := s.mocks[s.cursor]
		s.cursor++
		if p != nil {
			if ok, detail := p.MatchRequestShape(live, recorded, lane); ok {
				e.pass++
			} else {
				e.flag++
				e.flags = append(e.flags, lane.Name+": "+detail)
			}
		}
		return recorded, nil, nil // serve recorded either way
	}
	if p == nil {
		return nil, nil, nil
	}
	ka, err := p.EmptyResponse(lane)
	return nil, ka, err
}

// isArmedLocked reports whether a mock's anchor has been reached. Caller holds mu.
func (e *Engine) isArmedLocked(m *models.Mock) bool {
	return e.completed >= anchorPosOf(m)
}

// Report snapshots verdict tallies, counting undrained armed mocks as not-exercised.
func (e *Engine) Report() ReportSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	ne := 0
	for _, s := range e.streams {
		for i := s.cursor; i < len(s.mocks); i++ {
			if e.isArmedLocked(s.mocks[i]) {
				ne++
			}
		}
	}
	out := ReportSnapshot{Pass: e.pass, Flag: e.flag, NotExercised: ne}
	out.Flags = append(out.Flags, e.flags...)
	return out
}

func seqOf(m *models.Mock) int       { return atoiOr(m.Spec.Metadata[models.MetaAsyncSeq], 0) }
func anchorPosOf(m *models.Mock) int  { return atoiOr(m.Spec.Metadata[models.MetaAnchorPos], 0) }

func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/integrations/async/ -v`
Expected: PASS (all 6 engine tests + Task 2's interface test).

- [ ] **Step 5: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/agent/proxy/integrations/async/
go vet ./pkg/agent/proxy/integrations/async/
git add pkg/agent/proxy/integrations/async/
git commit -m "feat(async): transport-agnostic engine (gated ordered serving + shape verdict)"
```

---

## Task 4: HTTP AsyncParser implementation

**Files:**
- Create: `pkg/agent/proxy/integrations/http/async.go`
- Test: `pkg/agent/proxy/integrations/http/async_test.go`

**Interfaces:**
- Consumes: `models.AsyncLane`, `models.Mock`, `*HTTP`, `h.SchemaMatch`, `req` (http package), `MatchURLPath`.
- Produces (methods on `*HTTP`, satisfying `async.AsyncParser`):
  - `MatchesLane(m *models.Mock, lane models.AsyncLane) bool`
  - `MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (bool, string)`
  - `EmptyResponse(lane models.AsyncLane) ([]byte, error)`
  - helper `func liveReqToMock(input *req) *models.Mock`
  - `SetAsyncEngine(e *async.Engine)` + field `asyncEngine *async.Engine` on `HTTP` (satisfies `async.AsyncAware`).

- [ ] **Step 1: Write the failing tests**

Create `pkg/agent/proxy/integrations/http/async_test.go`:

```go
package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func laneNotify() models.AsyncLane {
	return models.AsyncLane{
		Name: "notifications", Type: "http",
		Match:          map[string]string{"host": "notify.internal.svc", "path": "/v1/poll*"},
		VolatileParams: []string{"cursor"},
	}
}

func TestMatchesLaneHostPathGlob(t *testing.T) {
	h := newHTTP()
	m := httpMock("m1", "GET", "http://notify.internal.svc/v1/poll?cursor=5")
	if !h.MatchesLane(m, laneNotify()) {
		t.Fatal("expected lane match on host+path glob")
	}
	other := httpMock("m2", "GET", "http://api.other.svc/v2/users")
	if h.MatchesLane(other, laneNotify()) {
		t.Fatal("non-lane host must not match")
	}
}

func TestEmptyResponseIs204(t *testing.T) {
	h := newHTTP()
	b, err := h.EmptyResponse(laneNotify())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got[:12] != "HTTP/1.1 204" {
		t.Fatalf("keep-alive should be 204, got %q", got[:20])
	}
}

func TestMatchRequestShapeVolatileParamIgnored(t *testing.T) {
	h := newHTTP()
	recorded := httpMock("rec", "GET", "http://notify.internal.svc/v1/poll?cursor=1")
	live := httpMock("live", "GET", "http://notify.internal.svc/v1/poll?cursor=999")
	ok, detail := h.MatchRequestShape(live, recorded, laneNotify())
	if !ok {
		t.Fatalf("volatile cursor difference must not fail shape: %s", detail)
	}
}

func TestMatchRequestShapePathDriftFlags(t *testing.T) {
	h := newHTTP()
	recorded := httpMock("rec", "GET", "http://notify.internal.svc/v1/poll?cursor=1")
	live := httpMock("live", "GET", "http://notify.internal.svc/v1/DIFFERENT?cursor=1")
	ok, _ := h.MatchRequestShape(live, recorded, laneNotify())
	if ok {
		t.Fatal("path drift must report shape mismatch")
	}
}
```

(`newHTTP()` and `httpMock(name, method, rawURL)` already exist in `match_test.go`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/integrations/http/ -run 'TestMatchesLane|TestEmptyResponse|TestMatchRequestShape' -v`
Expected: FAIL — `h.MatchesLane undefined`.

- [ ] **Step 3: Implement HTTP AsyncParser**

Create `pkg/agent/proxy/integrations/http/async.go`:

```go
package http

import (
	"fmt"
	"net/http"
	"net/url"
	"path"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
)

// compile-time assertions.
var _ async.AsyncParser = (*HTTP)(nil)
var _ async.AsyncAware = (*HTTP)(nil)

// SetAsyncEngine stores the shared engine (setter injection). Requires adding
// `asyncEngine *async.Engine` to the HTTP struct in http.go.
func (h *HTTP) SetAsyncEngine(e *async.Engine) { h.asyncEngine = e }

// MatchesLane matches host + path globs (path.Match) from lane.Match against
// the mock's recorded request URL.
func (h *HTTP) MatchesLane(m *models.Mock, lane models.AsyncLane) bool {
	if m == nil || m.Spec.HTTPReq == nil {
		return false
	}
	host, p := hostAndPath(m.Spec.HTTPReq)
	if hg := lane.Match["host"]; hg != "" {
		if ok, _ := path.Match(hg, host); !ok {
			return false
		}
	}
	if pg := lane.Match["path"]; pg != "" {
		if ok, _ := path.Match(pg, p); !ok {
			return false
		}
	}
	return lane.Match["host"] != "" || lane.Match["path"] != ""
}

// MatchRequestShape reuses SchemaMatch against the single recorded mock,
// after stripping volatile query params from both sides.
func (h *HTTP) MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (bool, string) {
	if live == nil || live.Spec.HTTPReq == nil || recorded == nil || recorded.Spec.HTTPReq == nil {
		return false, "missing request payload"
	}
	liveReq, err := mockToReq(live)
	if err != nil {
		return false, "unparseable live request: " + err.Error()
	}
	rec := stripVolatile(recorded, lane.VolatileParams)
	// SchemaMatch does field-by-field request-shape comparison; a non-empty
	// result means the single candidate matched.
	matched, err := h.SchemaMatch(nil, liveReq, []*models.Mock{rec}, flakyHeaderNoise(), nil, true)
	if err != nil {
		return false, "schema match error: " + err.Error()
	}
	if len(matched) == 0 {
		return false, fmt.Sprintf("request shape drift: %s %s vs %s",
			liveReq.method, liveReq.url.Path, recorded.Spec.HTTPReq.URL)
	}
	return true, ""
}

// EmptyResponse is a minimal 204 keep-alive.
func (h *HTTP) EmptyResponse(_ models.AsyncLane) ([]byte, error) {
	return []byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"), nil
}

func hostAndPath(r *models.HTTPReq) (host, p string) {
	if r.Header != nil && r.Header["Host"] != "" {
		host = r.Header["Host"]
	}
	if u, err := url.Parse(r.URL); err == nil {
		if host == "" {
			host = u.Host
		}
		p = u.Path
	}
	return host, p
}

// stripVolatile returns a shallow copy of the mock with volatile query params
// removed from URL + URLParams, so key-set comparison ignores them.
func stripVolatile(m *models.Mock, volatile []string) *models.Mock {
	if len(volatile) == 0 {
		return m
	}
	vol := make(map[string]bool, len(volatile))
	for _, v := range volatile {
		vol[v] = true
	}
	req := *m.Spec.HTTPReq
	if u, err := url.Parse(req.URL); err == nil {
		q := u.Query()
		for k := range vol {
			q.Del(k)
		}
		u.RawQuery = q.Encode()
		req.URL = u.String()
	}
	if req.URLParams != nil {
		np := make(map[string]string, len(req.URLParams))
		for k, v := range req.URLParams {
			if !vol[k] {
				np[k] = v
			}
		}
		req.URLParams = np
	}
	cp := *m
	sp := m.Spec
	sp.HTTPReq = &req
	cp.Spec = sp
	return &cp
}

// mockToReq builds the matcher's `req` value from a recorded/live mock.
func mockToReq(m *models.Mock) (*req, error) {
	u, err := url.Parse(m.Spec.HTTPReq.URL)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	for k, v := range m.Spec.HTTPReq.Header {
		hdr.Set(k, v)
	}
	return &req{
		method: string(m.Spec.HTTPReq.Method),
		url:    u,
		header: hdr,
		body:   []byte(m.Spec.HTTPReq.Body),
	}, nil
}

// flakyHeaderNoise returns the package flaky-header list as a header-noise map.
func flakyHeaderNoise() map[string][]string {
	nm := make(map[string][]string, len(flakyHeaders))
	for _, h := range flakyHeaders {
		nm[h] = []string{}
	}
	return nm
}
```

VERIFY DURING IMPLEMENTATION: confirm `flakyHeaders` is the exact package-level identifier (http.go lines 42–110) and that `SchemaMatch`'s first param tolerates a `nil` context in this call path; if it dereferences ctx, pass `context.Background()`. Confirm `req` field names (`method`,`url`,`header`,`body`) against match.go lines 112–118.

- [ ] **Step 4: Add the struct field**

In `pkg/agent/proxy/integrations/http/http.go`, add to `type HTTP struct` (line 46):

```go
type HTTP struct {
	Logger      *zap.Logger
	asyncEngine *async.Engine
}
```

Add the import `"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"` to http.go.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/integrations/http/ -run 'TestMatchesLane|TestEmptyResponse|TestMatchRequestShape' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/agent/proxy/integrations/http/async.go pkg/agent/proxy/integrations/http/http.go pkg/agent/proxy/integrations/http/async_test.go
go vet ./pkg/agent/proxy/integrations/http/
git add pkg/agent/proxy/integrations/http/async.go pkg/agent/proxy/integrations/http/async_test.go pkg/agent/proxy/integrations/http/http.go
git commit -m "feat(async): HTTP AsyncParser (lane glob, 204 keep-alive, shape via SchemaMatch)"
```

---

## Task 5: Record-side marking hook

**Files:**
- Create: `pkg/service/record/asynchook.go`
- Test: `pkg/service/record/asynchook_test.go`

**Interfaces:**
- Consumes: `record.BaseRecordHooks`, `record.MockContext`, `record.TestCaseContext`, `models.AsyncLane`, `async.AsyncParser`, `integrations.Registered`.
- Produces:
  - `func NewAsyncRecorder(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]async.AsyncParser) *AsyncRecorder`
  - `AsyncRecorder` embeds `BaseRecordHooks`; overrides `AfterTestCaseInsert` and `AfterMockInsert`.
  - `func ResolveAsyncParsers(logger *zap.Logger, lanes []models.AsyncLane) map[string]async.AsyncParser` — builds stateless parser instances from `integrations.Registered` keyed by lane.Type.

- [ ] **Step 1: Write the failing tests**

Create `pkg/service/record/asynchook_test.go`:

```go
package record

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// laneStub matches any mock whose metadata has type == "lane".
type laneStub struct{}

func (laneStub) MatchesLane(m *models.Mock, _ models.AsyncLane) bool {
	return m != nil && m.Spec.Metadata["kind"] == "lane"
}
func (laneStub) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) { return true, "" }
func (laneStub) EmptyResponse(_ models.AsyncLane) ([]byte, error)                       { return nil, nil }

func egress(kind string, completedAt time.Time) *models.Mock {
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"kind": kind},
		ResTimestampMock: completedAt, // async COMPLETION time drives the anchor
	}}
}

func newAsyncRec() *AsyncRecorder {
	lane := models.AsyncLane{Name: "L", Type: "http"}
	return NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]interface {
			MatchesLane(*models.Mock, models.AsyncLane) bool
			MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string)
			EmptyResponse(models.AsyncLane) ([]byte, error)
		}{"http": laneStub{}})
}

func TestAnchorIsEffectiveTestcaseDuringWindow(t *testing.T) {
	r := newAsyncRec()
	base := time.Unix(1000, 0)
	// T1 starts at base, T2 starts at base+5s (window START = HTTPReq.Timestamp)
	_ = r.AfterTestCaseInsert(context.Background(), &TestCaseContext{
		TestCase: &models.TestCase{Name: "T1", HTTPReq: models.HTTPReq{Timestamp: base}}})
	_ = r.AfterTestCaseInsert(context.Background(), &TestCaseContext{
		TestCase: &models.TestCase{Name: "T2", HTTPReq: models.HTTPReq{Timestamp: base.Add(5 * time.Second)}}})

	// delivery COMPLETES mid-T2 (base+6s): effective testcase = T2, effect from T3.
	m := egress("lane", base.Add(6*time.Second))
	_ = r.AfterMockInsert(context.Background(), &MockContext{Mock: m})

	md := m.Spec.Metadata
	if md[models.MetaAsync] != "true" || md[models.MetaAsyncLane] != "L" {
		t.Fatalf("not stamped async: %+v", md)
	}
	if md[models.MetaAnchorAfter] != "T2" || md[models.MetaAnchorPos] != "2" || md[models.MetaAsyncSeq] != "1" {
		t.Fatalf("delivery mid-T2 must anchor to effective testcase T2/pos2: %+v", md)
	}
}

func TestAnchorInGapUsesLastStartedTest(t *testing.T) {
	r := newAsyncRec()
	base := time.Unix(1000, 0)
	_ = r.AfterTestCaseInsert(context.Background(), &TestCaseContext{
		TestCase: &models.TestCase{Name: "T1", HTTPReq: models.HTTPReq{Timestamp: base}}})
	// delivery completes after T1 started, before any later test -> anchor T1/pos1
	m := egress("lane", base.Add(2*time.Second))
	_ = r.AfterMockInsert(context.Background(), &MockContext{Mock: m})
	if m.Spec.Metadata[models.MetaAnchorAfter] != "T1" || m.Spec.Metadata[models.MetaAnchorPos] != "1" {
		t.Fatalf("gap delivery must anchor to last started test T1/pos1: %+v", m.Spec.Metadata)
	}
}

func TestStartupAnchorBeforeFirstTest(t *testing.T) {
	r := newAsyncRec()
	m := egress("lane", time.Unix(500, 0))
	_ = r.AfterMockInsert(context.Background(), &MockContext{Mock: m})
	if m.Spec.Metadata[models.MetaAnchorAfter] != models.AnchorStartup ||
		m.Spec.Metadata[models.MetaAnchorPos] != "0" {
		t.Fatalf("pre-first-test egress must anchor to startup/0: %+v", m.Spec.Metadata)
	}
}

func TestNonLaneEgressUntouched(t *testing.T) {
	r := newAsyncRec()
	m := egress("normal", time.Unix(2000, 0))
	_ = r.AfterMockInsert(context.Background(), &MockContext{Mock: m})
	if _, ok := m.Spec.Metadata[models.MetaAsync]; ok {
		t.Fatalf("non-lane egress must not be stamped: %+v", m.Spec.Metadata)
	}
}
```

NOTE: the inline interface literal in `newAsyncRec()` is awkward; in Step 3 export the `async.AsyncParser` type and replace the map value type with `map[string]async.AsyncParser`. Adjust the test's `newAsyncRec` to `map[string]async.AsyncParser{"http": laneStub{}}` and add the `async` import once the hook compiles. (Keep `laneStub` — it satisfies `async.AsyncParser`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/service/record/ -run 'TestLaneEgress|TestStartupAnchor|TestNonLane' -v`
Expected: FAIL — `undefined: NewAsyncRecorder`.

- [ ] **Step 3: Implement the hook**

Create `pkg/service/record/asynchook.go`:

```go
package record

import (
	"context"
	"sort"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type testWindow struct {
	id        string
	startedAt int64 // unix nanos (ingress request timestamp = window START)
}

// AsyncRecorder stamps async metadata on egress mocks that match a declared
// lane. Lane match is the SOLE discriminator; position only sets the anchor.
type AsyncRecorder struct {
	BaseRecordHooks
	logger  *zap.Logger
	lanes   []models.AsyncLane
	parsers map[string]async.AsyncParser

	mu    sync.Mutex
	tests []testWindow   // ingress windows, by start timestamp
	seq   map[string]int // per-lane counter
}

func NewAsyncRecorder(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]async.AsyncParser) *AsyncRecorder {
	return &AsyncRecorder{logger: logger, lanes: lanes, parsers: parsers, seq: map[string]int{}}
}

func (r *AsyncRecorder) AfterTestCaseInsert(_ context.Context, info *TestCaseContext) error {
	if info == nil || info.TestCase == nil {
		return nil
	}
	r.mu.Lock()
	r.tests = append(r.tests, testWindow{
		id:        info.TestCase.Name,
		startedAt: info.TestCase.HTTPReq.Timestamp.UnixNano(), // window START
	})
	r.mu.Unlock()
	return nil
}

func (r *AsyncRecorder) AfterMockInsert(_ context.Context, info *MockContext) error {
	if info == nil || info.Mock == nil || len(r.lanes) == 0 {
		return nil
	}
	m := info.Mock
	for _, lane := range r.lanes {
		p := r.parsers[lane.Type]
		if p == nil || !p.MatchesLane(m, lane) {
			continue
		}
		r.mu.Lock()
		r.seq[lane.Name]++
		seq := r.seq[lane.Name]
		// Anchor by the async COMPLETION time (response timestamp).
		anchorID, anchorPos := r.anchorLocked(m.Spec.ResTimestampMock.UnixNano())
		r.mu.Unlock()

		if m.Spec.Metadata == nil {
			m.Spec.Metadata = map[string]string{}
		}
		m.Spec.Metadata[models.MetaAsync] = "true"
		m.Spec.Metadata[models.MetaAsyncLane] = lane.Name
		m.Spec.Metadata[models.MetaAnchorAfter] = anchorID
		m.Spec.Metadata[models.MetaAnchorPos] = strconv.Itoa(anchorPos)
		m.Spec.Metadata[models.MetaAsyncSeq] = strconv.Itoa(seq)
		return nil
	}
	return nil
}

// anchorLocked returns the "effective testcase" for an async completion at ts:
// the last testcase STARTED at or before ts (its effect arms only from the
// NEXT test, never retroactively altering that test). Returns
// (id-or-startup, 1-based index / count started). Order-independent. Caller holds mu.
func (r *AsyncRecorder) anchorLocked(ts int64) (string, int) {
	id, pos := models.AnchorStartup, 0
	var best int64
	for _, w := range r.tests {
		if w.startedAt <= ts {
			pos++
			if id == models.AnchorStartup || w.startedAt >= best {
				best = w.startedAt
				id = w.id
			}
		}
	}
	return id, pos
}

// ResolveAsyncParsers builds stateless parser instances from the global
// registry, keyed by lane.Type. Parsers must implement async.AsyncParser.
func ResolveAsyncParsers(logger *zap.Logger, lanes []models.AsyncLane) map[string]async.AsyncParser {
	out := map[string]async.AsyncParser{}
	for _, lane := range lanes {
		if _, done := out[lane.Type]; done {
			continue
		}
		reg := integrations.Registered[integrations.IntegrationType(lane.Type)]
		if reg == nil {
			logger.Warn("async lane type not registered", zap.String("type", lane.Type))
			continue
		}
		if ap, ok := reg.Initializer(logger).(async.AsyncParser); ok {
			out[lane.Type] = ap
		} else {
			logger.Warn("async lane parser does not implement AsyncParser", zap.String("type", lane.Type))
		}
	}
	return out
}

var _ = sort.SliceStable // retained if needed for deterministic lane order
```

Then update `newAsyncRec()` in the test to `map[string]async.AsyncParser{"http": laneStub{}}` and import `async`. Remove the placeholder `sort` usage line if unused (delete the `var _ = sort.SliceStable` and the `sort` import if not needed).

VERIFY: `record` importing `pkg/agent/proxy/integrations` and `.../async` must not create an import cycle. `integrations` imports `models` only; `async` imports `models` + `integrations`. `record` already sits above the proxy layer. If a cycle appears, move `ResolveAsyncParsers` to the CLI provider package (Task 7) where both are already imported, and keep the hook depending only on `async.AsyncParser`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/service/record/ -run 'TestLaneEgress|TestStartupAnchor|TestNonLane' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/service/record/asynchook.go pkg/service/record/asynchook_test.go
go vet ./pkg/service/record/
git add pkg/service/record/asynchook.go pkg/service/record/asynchook_test.go
git commit -m "feat(async): record hook stamps async metadata on lane egress"
```

---

## Task 6: Replay wiring (engine on Proxy + decode branch)

**Files:**
- Modify: `pkg/agent/proxy/proxy.go` (struct field 158; `New` 650; `InitIntegrations` 992; `SetMocksWithWindow` 2796)
- Modify: `pkg/agent/proxy/integrations/http/decode.go` (before `h.match(...)` at line 247)
- Test: `pkg/agent/proxy/proxy_async_test.go` (create) — SetMocksWithWindow position advance

**Interfaces:**
- Consumes: `async.NewEngine`, `async.AsyncAware`, `engine.Load`, `engine.OnTestComplete`, `engine.LaneFor`, `engine.Decide`, `cfg.Async.Lanes`.
- Produces: `Proxy.asyncEngine *async.Engine`; per-test advance behavior.

- [ ] **Step 1: Write the failing test**

Create `pkg/agent/proxy/proxy_async_test.go`:

```go
package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestSetMocksWithWindowAdvancesEngineAfterFirst(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, nil)
	p := &Proxy{logger: zap.NewNop(), asyncEngine: eng}

	// first window => no completion advance
	_ = p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, models.BaseTime)
	if got := eng.CompletedForTest(); got != 0 {
		t.Fatalf("after first window completed=%d want 0", got)
	}
	// second window => one test completed
	_ = p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, models.BaseTime)
	if got := eng.CompletedForTest(); got != 1 {
		t.Fatalf("after second window completed=%d want 1", got)
	}
}
```

Add a tiny test accessor to `engine.go` (Task 3 file): `func (e *Engine) CompletedForTest() int { e.mu.Lock(); defer e.mu.Unlock(); return e.completed }`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/ -run TestSetMocksWithWindowAdvancesEngine -v`
Expected: FAIL — `Proxy has no field asyncEngine` / `CompletedForTest undefined`.

- [ ] **Step 3: Add the engine accessor**

In `pkg/agent/proxy/integrations/async/engine.go` add:

```go
// CompletedForTest exposes the completed counter for wiring tests.
func (e *Engine) CompletedForTest() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completed
}
```

- [ ] **Step 4: Add the Proxy field + construction + injection + advance**

In `pkg/agent/proxy/proxy.go`:

(a) Add to `type Proxy struct` after `mockManager` (line 158):
```go
	asyncEngine       *async.Engine
	asyncWindowSeen   bool
```
Add import `"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"`.

(b) In `func New(...)` (line 650), after the `Integrations` map is allocated (line 663), build the engine when lanes exist:
```go
	if len(opts.Async.Lanes) > 0 {
		parsers := map[string]async.AsyncParser{}
		for _, lane := range opts.Async.Lanes {
			// parser instances are attached in InitIntegrations; the engine
			// only needs the lane->parser map for LaneFor/Decide, filled there.
			_ = lane
		}
		p.asyncEngine = async.NewEngine(logger, opts.Async.Lanes, parsers)
	}
```
(The parser map is populated in InitIntegrations step (c) via the AsyncAware setter, which also lets the engine resolve `parsers[lane.Type]`. To keep the engine's `parsers` populated, add `func (e *Engine) RegisterParser(t string, p AsyncParser)` to engine.go and call it from (c).)

Add to engine.go:
```go
func (e *Engine) RegisterParser(t string, p AsyncParser) {
	e.mu.Lock()
	if e.parsers == nil {
		e.parsers = map[string]AsyncParser{}
	}
	e.parsers[t] = p
	e.mu.Unlock()
}
```

(c) In `func (p *Proxy) InitIntegrations(...)` (line 987), after `p.Integrations[parserType] = prs` (line 992):
```go
		if p.asyncEngine != nil {
			if aw, ok := prs.(async.AsyncAware); ok {
				aw.SetAsyncEngine(p.asyncEngine)
				p.asyncEngine.RegisterParser(string(parserType), prs.(async.AsyncParser))
			}
		}
```

(d) In `func (p *Proxy) SetMocksWithWindow(...)` (line 2796), after the existing `m.SetMocksWithWindow(...)` call (line 2798), advance/load the engine:
```go
	if p.asyncEngine != nil {
		// First window = app reaching its first test; no test completed yet.
		// Every subsequent window = one more completed test.
		if p.asyncWindowSeen {
			p.asyncEngine.OnTestComplete()
		}
		p.asyncWindowSeen = true
	}
```
**CORRECTED (do NOT Load from `unFiltered` here).** `SetMocksWithWindow` only ever receives *windowed subsets* per test — never the complete async corpus. So this seam is used for **position advance ONLY**. The complete-corpus `engine.Load` happens once at the **agent's `StoreMocks`/`StoreMocksStream` seam** (see sub-task 6b), where `ClientMockStorage` holds the full set. `engine.Load` is already run-once/idempotent (Task 3), so 6b calls it exactly once with the complete async subset.

> **Task 6 is split into 6a/6b/6c** — 6a: Proxy field + construct + inject + position-advance + test (this section's Steps 1–4, minus the Load line). 6b: `Proxy.LoadAsyncMocks` + call from the agent `StoreMocks` seam with the complete corpus. 6c: `decode.go` serving branch (this section's Step 5). Briefs authored directly by the controller.

- [ ] **Step 5: Add the decode.go lane-routing branch**

In `pkg/agent/proxy/integrations/http/decode.go`, immediately before the `h.match(...)` call (line 247), mirroring the telemetry short-circuit at 224–245:

```go
		if h.asyncEngine != nil {
			live := liveReqToMock(input)
			if lane, ok := h.asyncEngine.LaneFor(live); ok {
				recorded, keepAlive, derr := h.asyncEngine.Decide(lane, live)
				if derr != nil {
					return derr
				}
				if recorded != nil {
					if werr := h.writeRecordedResponse(clientConn, recorded); werr != nil {
						return werr
					}
				} else {
					if _, werr := clientConn.Write(keepAlive); werr != nil {
						return werr
					}
				}
				continue // read the next request on this keep-alive connection
			}
		}
```

Add `liveReqToMock` and `writeRecordedResponse` to `pkg/agent/proxy/integrations/http/async.go`:

```go
// liveReqToMock wraps the matcher's parsed request as a *models.Mock so the
// engine (which only speaks *models.Mock) can route/verify it.
func liveReqToMock(input *req) *models.Mock {
	hdr := map[string]string{}
	for k := range input.header {
		hdr[k] = input.header.Get(k)
	}
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{},
		HTTPReq: &models.HTTPReq{
			Method: models.Method(input.method),
			URL:    input.url.String(),
			Header: hdr,
			Body:   string(input.body),
		},
	}}
}

// writeRecordedResponse serializes a recorded mock's HTTP response to the
// client conn (mirrors decode.go lines 294–346).
func (h *HTTP) writeRecordedResponse(clientConn net.Conn, stub *models.Mock) error {
	stub.HydrateResponse()
	resp := stub.Spec.HTTPResp
	if resp == nil {
		return fmt.Errorf("async: recorded mock %s has no response", stub.Name)
	}
	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n",
		stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor,
		resp.StatusCode, resp.StatusMessage)
	var sb strings.Builder
	sb.WriteString(statusLine)
	for k, v := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		sb.WriteString(k + ": " + v + "\r\n")
	}
	body := []byte(resp.Body)
	sb.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
	sb.Write(body)
	_, err := clientConn.Write([]byte(sb.String()))
	return err
}
```

Add imports to async.go: `"net"`, `"strings"`.

VERIFY DURING IMPLEMENTATION: confirm the exact identifier for the client connection variable in `decode.go` at line 247 (`clientConn` vs `src`) and that a `continue`-able request loop encloses line 247 (as the telemetry short-circuit at 224–245 implies). Reuse `pkg.ToHTTPHeader`/`pkg.Compress` if the recorded response relies on content-encoding, matching lines 294–346 exactly. Prefer factoring the existing 294–346 block into a shared `writeRecordedResponse` and calling it from both places rather than duplicating, if the diff stays small.

- [ ] **Step 6: Run tests + build**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./pkg/agent/proxy/ -run TestSetMocksWithWindowAdvancesEngine -v && go build ./...`
Expected: PASS; whole build succeeds.

- [ ] **Step 7: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/agent/proxy/proxy.go pkg/agent/proxy/integrations/http/decode.go pkg/agent/proxy/integrations/http/async.go pkg/agent/proxy/integrations/async/engine.go pkg/agent/proxy/proxy_async_test.go
go vet ./pkg/agent/proxy/...
git add pkg/agent/proxy/
git commit -m "feat(async): wire engine into proxy dispatch + per-test advance"
```

---

## Task 7: Record wiring (install the hook)

**Files:**
- Modify: `cli/provider/core_service.go` (the `record.New(..., nil, cfg)` site near line 44)
- Test: manual/e2e (Task 8) — this is thin glue; no isolated unit test.

**Interfaces:**
- Consumes: `record.New`, `record.Recorder.SetRecordHooks`, `record.NewAsyncRecorder`, `record.ResolveAsyncParsers`, `cfg.Async.Lanes`.

- [ ] **Step 1: Install the hook conditionally**

In `cli/provider/core_service.go`, where the record service is constructed (currently `record.New(..., nil, cfg)` at ~line 44), install the async hook when lanes are configured:

```go
	recordSvc := record.New(logger, testDB, mockDB, mappingDB, telemetry, instrumentation, testSetConf, nil, cfg)
	if len(cfg.Async.Lanes) > 0 {
		if rec, ok := recordSvc.(*record.Recorder); ok {
			parsers := record.ResolveAsyncParsers(logger, cfg.Async.Lanes)
			rec.SetRecordHooks(record.NewAsyncRecorder(logger, cfg.Async.Lanes, parsers))
		}
	}
```

VERIFY: match the exact existing variable names / constructor arg order at the call site (arg order confirmed: `logger, testDB, mockDB, mappingDB, telemetry, instrumentation, testSetConf, hooks, config`). If enterprise already sets a hook here via the same slot, wrap both in a small fan-out `RecordHooks` (a slice that calls each) instead of overwriting — OSS passes `nil` today so overwrite is safe for OSS.

- [ ] **Step 2: Build**

Run: `cd /home/aditya/flipkart/asyncHttpApproach/keploy && go build ./...`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w cli/provider/core_service.go
git add cli/provider/core_service.go
git commit -m "feat(async): install AsyncRecorder record hook when lanes configured"
```

---

## Task 8: Final-gap advance, E2E, and regression

**Files:**
- Modify: `pkg/service/replay/replay.go` (`RunTestSet`, after the testcase loop ~line 1721+, before returning status)
- Create: e2e sample per the `keploy-e2e-test` skill (sample app with ingress endpoints + a background HTTP long-poll consumer).

**Interfaces:**
- Consumes: the full stack from Tasks 1–7.

- [ ] **Step 1: Add the final-gap advance**

Mocks anchored after the LAST testcase arm only when that test completes, but no window event follows it. In `pkg/service/replay/replay.go`, at the end of `RunTestSet` (after the loop, on the success path), signal completion of the final test through the same window RPC path by calling `SendMockFilterParamsToAgent` once more with the last window (or add a dedicated `r.instrumentation.AsyncRunComplete(ctx)` that the agent forwards to `Proxy` → `engine.OnTestComplete()`).

Minimal approach (no new RPC): after the loop, if any tests ran, call:
```go
	// Arm the final gap: the last testcase has completed.
	_ = r.SendMockFilterParamsToAgent(runTestSetCtx, nil, lastReqTime, lastRespTime, totalConsumedMocks, useMappingBased)
```
where `lastReqTime`/`lastRespTime` are retained from the final loop iteration. This triggers one more `SetMocksWithWindow` on the proxy, advancing the engine by one (arming final-gap mocks) with an empty filter (no new sync mocks).

VERIFY: confirm an extra `SetMocksWithWindow` with an empty mapping does not disturb sync replay state (it should only reset the window; filtered set empty is acceptable at teardown). If it does, add the dedicated `AsyncRunComplete` path instead.

- [ ] **Step 2: Write the E2E (follow the keploy-e2e-test skill)**

Invoke the `keploy-e2e-test` skill and build a sample matching its harness: an app with 2 ingress endpoints and a background thread that long-polls `notify.internal.svc/v1/poll` and, per message, writes to a DB and calls an outbound HTTP endpoint. Configure `keploy.yml`:
```yaml
async:
    lanes:
      - name: notifications
        type: http
        match: { host: "notify.internal.svc", path: "/v1/poll*" }
        volatileParams: ["cursor"]
```
E2E assertions:
1. Record → the test-set contains mocks tagged `async: "true"` with `lane: notifications` and monotonic `asyncSeq`, and `anchorAfter`/`anchorPos` consistent with when each poll fired.
2. Replay → passes; polls before a delivery's anchor receive `204` keep-alive; deliveries are served in `asyncSeq` order.
3. Mutate a recorded poll request shape (change the recorded path) → replay reports a shape FLAG (Report().Flag > 0), and still serves the response.

- [ ] **Step 3: Regression — zero-impact**

Run the existing e2e/unit suite with NO `async.lanes` configured and confirm record + replay are byte-identical (no async metadata stamped, engine nil). Command:
```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy && go test ./... 2>&1 | tail -40
```
Expected: all existing tests pass; no async metadata appears in recordings made without lanes.

- [ ] **Step 4: Commit**

```bash
cd /home/aditya/flipkart/asyncHttpApproach/keploy
gofmt -w pkg/service/replay/replay.go
go vet ./...
git add pkg/service/replay/replay.go tests/ config/
git commit -m "feat(async): final-gap advance + e2e long-poll coverage"
```

---

## Self-Review

**Spec coverage:**
- §3.1 transport-agnostic engine → Task 3 (engine holds only mock/lane; parser delegates) + pluggability test.
- §3.2 async-as-metadata → Task 1 (`models` consts) + Task 5 (stamping).
- §4 lane config → Task 1.
- §5 `AsyncParser` → Task 2; HTTP impl → Task 4.
- §6 metadata keys → Task 1 (note: added `anchorPos` beyond the spec's list — the integer ordinal the arming math needs; `anchorAfter` name is kept for readability).
- §7 record-side inline marking → Task 5; lane-is-sole-discriminator + startup + during-window anchor → tests in Task 5.
- §8 replay engine (gated/ordered/keep-alive/verdict/armed-stays-armed) → Task 3 tests; wiring → Task 6; final-gap → Task 8.
- §9 edge cases: keep-alive (#1) Task 4; drained (#2/#5) Task 3; shape mismatch (#3) Tasks 3/4; concurrency (#6) engine per-lane streams; startup (#8) Task 3; zero-lanes (#10) Tasks 1/6/7. Multiple-connections-per-lane (#7) is handled by keying streams on lane name (not connID) — documented, no extra task.
- §11 testing: engine unit + pluggability (Task 3), HTTP unit (Task 4), record marking (Task 5), e2e + regression (Task 8).

**Placeholder scan:** No "TBD"/"handle edge cases". Each `VERIFY DURING IMPLEMENTATION` note points at a specific line to confirm against real source (unavoidable for a plan written against a moving tree) — not a content placeholder; the code to write is fully given.

**Type consistency:** `models.MetaAnchorPos`/`MetaAnchorAfter`/`MetaAsyncSeq`/`MetaAsyncLane`/`MetaAsync` used identically across Tasks 1/3/5. `Engine.OnTestComplete`, `Load`, `LaneFor`, `Decide`, `RegisterParser`, `CompletedForTest` names consistent Tasks 3/6. `AsyncParser` method set identical Tasks 2/3/4/5. `SetAsyncEngine` (AsyncAware) consistent Tasks 2/4/6.

**Known risk (surfaced, not hidden):** the position-advance wiring (Task 6 step (d) + Task 8 step 1) is the fragile seam — it depends on `SetMocksWithWindow` firing exactly once per test and one extra teardown call. The engine's own logic is fully unit-tested in isolation (Task 3) with explicit `OnTestComplete()` calls, so a wiring miscount is caught by e2e (Task 8), not silently wrong.
