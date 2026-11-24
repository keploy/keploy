package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/matcher"
	grpcMatcher "go.keploy.io/server/v3/pkg/matcher/grpc"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/protoschema"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// THE PROTO FILES NEVER DECIDE A gRPC VERDICT.
//
// --proto-file/--proto-dir/--proto-include (test.protoFile/protoDir/
// protoInclude) used to turn both bodies into JSON and compare that. protojson
// does not print a field the descriptor does not define, and the matcher
// forgives a length difference when the decoded data is identical, so a
// change in such a field -- a stale proto, a field the branch removed, a type
// the proto no longer matches, an optional field going from unset to zero --
// passed. Without protos the same change failed: giving protos made the
// comparison weaker.
//
// Now the bodies are always compared as recorded (protoscope, by field
// number). A schema only (a) translates noise written by field name into the
// field-number paths that comparison applies, so a rule on `created_at` keeps
// working, (b) says which repeated fields are maps (compared in any order),
// (c) lets a body difference be graded as a JSON one, and (d) shows a failed
// test's bodies with field names (Result.GrpcDecode).
//
// EACH SIDE HAS ITS OWN SCHEMA FOR DISPLAY. The recorded body was produced by
// the release it was recorded on; the new one by the code under test. With a
// schema source (cloud replay sets one: GrpcSchemaSource), the recorded side
// is shown with the schema uploaded for its release, and the new side with the
// run's local protos or, without them, the schema uploaded for the release
// being replayed. A recorded release with no upload shows both sides as
// recorded and says so, never a guess from another release. Without a source
// (a plain keploy test) the run's local protos read both, as before.

// GrpcSchemaSource supplies schemas uploaded per release. Cloud replay sets
// one (SetGrpcSchemaSource); a plain keploy test has none.
type GrpcSchemaSource interface {
	// RecordedSchema is the schema uploaded for the release testSetID was
	// recorded on, with its label ("release:<image ref>"). A nil schema comes
	// with a note saying why (none uploaded for that release, the set names
	// no release), shown to the user as given.
	RecordedSchema(ctx context.Context, testSetID string) (schema *protoschema.Schema, label, note string)
	// TargetSchema is the schema uploaded for the release being replayed,
	// used for the new side when the run has no local protos. Nil when none.
	TargetSchema(ctx context.Context) (schema *protoschema.Schema, label string)
}

// SetGrpcSchemaSource gives the replay the schemas uploaded per release.
func (r *Replayer) SetGrpcSchemaSource(src GrpcSchemaSource) { r.grpcSchemas.source = src }

// grpcSchemas caches what a run resolves: the local schema per method, and
// the source's schemas per test set and for the target.
type grpcSchemas struct {
	source GrpcSchemaSource

	mu       sync.Mutex
	local    map[string]*protoschema.Schema // per method; nil value: unresolved
	recorded map[string]recordedSchema      // per test set
	target   *recordedSchema
	// warned holds the refused noise entries already said (firstRefusal).
	warned sync.Map
}

// firstRefusal reports whether these refused noise entries were not yet
// said for this method in this run.
func (g *grpcSchemas) firstRefusal(method string, keys []string) bool {
	return g.firstWarning(method + "\x00" + strings.Join(keys, "\x00"))
}

// firstWarning reports whether a warning keyed so was not yet said in this
// run (the comparison's invalid noise patterns use it: WithWarnOnce).
func (g *grpcSchemas) firstWarning(key string) bool {
	_, seen := g.warned.LoadOrStore(key, true)
	return !seen
}

type recordedSchema struct {
	schema *protoschema.Schema
	label  string
	note   string
}

// grpcProtoView is the schema each side of one test case is read with.
type grpcProtoView struct {
	method string

	expected      *protoschema.Schema
	expectedLabel string
	actual        *protoschema.Schema
	actualLabel   string
	// note says why a side has no schema, for the report.
	note string
}

func (r *Replayer) protosConfigured() bool {
	return r.config.Test.ProtoFile != "" || r.config.Test.ProtoDir != "" || len(r.config.Test.ProtoInclude) > 0
}

// grpcProtoViewFor resolves the schemas tc's bodies are read with, or nil
// when the run has neither local protos nor a schema source.
func (r *Replayer) grpcProtoViewFor(tc *models.TestCase, testSetID string) *grpcProtoView {
	if tc == nil {
		return nil
	}
	src := r.grpcSchemas.source
	if src == nil && !r.protosConfigured() {
		return nil
	}
	method := tc.GrpcReq.Headers.PseudoHeaders[":path"]
	if method == "" {
		return nil
	}
	v := &grpcProtoView{method: method}
	local := r.localSchema(method)
	ctx := context.Background()
	if src != nil {
		rec := r.recordedSchemaFor(ctx, testSetID)
		v.expected, v.expectedLabel, v.note = rec.schema, rec.label, rec.note
		if local != nil {
			v.actual, v.actualLabel = local, "local"
		} else if t := r.targetSchema(ctx); t.schema != nil {
			v.actual, v.actualLabel = t.schema, t.label
		}
	} else if local != nil {
		v.expected, v.expectedLabel = local, "local"
		v.actual, v.actualLabel = local, "local"
	}
	if v.expected == nil && v.actual == nil && v.note == "" {
		return nil
	}
	return v
}

func (r *Replayer) localSchema(method string) *protoschema.Schema {
	if !r.protosConfigured() {
		return nil
	}
	r.grpcSchemas.mu.Lock()
	defer r.grpcSchemas.mu.Unlock()
	if r.grpcSchemas.local == nil {
		r.grpcSchemas.local = map[string]*protoschema.Schema{}
	}
	if s, seen := r.grpcSchemas.local[method]; seen {
		return s
	}
	pc := models.ProtoConfig{
		ProtoFile:    r.config.Test.ProtoFile,
		ProtoDir:     r.config.Test.ProtoDir,
		ProtoInclude: r.config.Test.ProtoInclude,
		RequestURI:   method,
	}
	var s *protoschema.Schema
	if _, files, err := utils.GetProtoMessageDescriptor(context.Background(), r.logger, pc); err == nil {
		s, err = protoschema.FromFileDescriptors(files)
		if err != nil {
			r.logger.Warn("the run's proto files could not be loaded; gRPC bodies of this method are shown and noised by field number only",
				zap.String("method", method), zap.Error(err))
			s = nil
		}
	} else {
		r.logger.Warn("the proto files given for this run do not describe this gRPC method, so its bodies are shown and noised by field number only",
			zap.String("method", method), zap.Error(err))
	}
	r.grpcSchemas.local[method] = s
	return s
}

func (r *Replayer) recordedSchemaFor(ctx context.Context, testSetID string) recordedSchema {
	r.grpcSchemas.mu.Lock()
	if rs, ok := r.grpcSchemas.recorded[testSetID]; ok {
		r.grpcSchemas.mu.Unlock()
		return rs
	}
	r.grpcSchemas.mu.Unlock()
	s, label, note := r.grpcSchemas.source.RecordedSchema(ctx, testSetID)
	rs := recordedSchema{schema: s, label: label, note: note}
	r.grpcSchemas.mu.Lock()
	if r.grpcSchemas.recorded == nil {
		r.grpcSchemas.recorded = map[string]recordedSchema{}
	}
	r.grpcSchemas.recorded[testSetID] = rs
	r.grpcSchemas.mu.Unlock()
	return rs
}

func (r *Replayer) targetSchema(ctx context.Context) recordedSchema {
	r.grpcSchemas.mu.Lock()
	if t := r.grpcSchemas.target; t != nil {
		r.grpcSchemas.mu.Unlock()
		return *t
	}
	r.grpcSchemas.mu.Unlock()
	s, label := r.grpcSchemas.source.TargetSchema(ctx)
	t := &recordedSchema{schema: s, label: label}
	r.grpcSchemas.mu.Lock()
	r.grpcSchemas.target = t
	r.grpcSchemas.mu.Unlock()
	return *t
}

// noiseMessage is the response descriptor noise names are read against: the
// schema the test case's fields were written in. With a schema source that is
// the recorded release's, and nothing else -- never a guess from another
// release; without one, the run's local protos.
func (v *grpcProtoView) noiseMessage() protoreflect.MessageDescriptor {
	s := v.expected
	if s == nil {
		return nil
	}
	if _, resp, err := s.MessageTypes(v.method); err == nil {
		return resp
	}
	return nil
}

// withNameNoiseAsFieldNumbers returns a copy of tc whose noise also names, by
// field-number path, every body field the configured noise (the run's global
// and test-set noise, then the test case's own, which wins) names by field
// name. tc itself is not changed.
//
// A key with value patterns is translated only for a field whose recorded
// text is the value a pattern was written against (patternReadable): an enum
// is recorded as its number and a message as its fields, so a pattern written
// for "SHIPPED" or an RFC3339 timestamp would read something else entirely.
// Such keys are left untranslated, and said once per method.
func (r *Replayer) withNameNoiseAsFieldNumbers(tc *models.TestCase, view *grpcProtoView, testSetID string) *models.TestCase {
	if tc == nil || view == nil {
		return tc
	}
	md := view.noiseMessage()
	if md == nil {
		return tc
	}
	global := r.config.Test.GlobalNoise.Global
	if ts, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		global = LeftJoinNoise(global, ts)
	}
	tcBody, _, _ := matcher.SplitNoise(tc.Noise, r.logger)
	added := map[string][]string{}
	var refused []string
	translate := func(entries map[string][]string) {
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			patterns := entries[key]
			num, fd, ok := fieldNumberPathOf(md, key)
			if !ok {
				continue
			}
			if hasPatterns(patterns) && !patternReadable(fd) {
				refused = append(refused, key)
				continue
			}
			added["body."+num] = patterns
		}
	}
	translate(global["body"]) // first: the test case's own entries win below
	translate(tcBody)
	if len(refused) > 0 && r.grpcSchemas.firstRefusal(view.method, refused) {
		r.logger.Warn("these gRPC noise entries name a field whose recorded value a pattern cannot be matched against (an enum, message, bytes, bool, sint, float or fixed-width field) and carry value patterns; list the field without a pattern, or by its field-number path",
			zap.String("method", view.method), zap.Strings("noise", refused))
	}
	if len(added) == 0 {
		return tc
	}
	out := *tc
	out.Noise = make(map[string][]string, len(tc.Noise)+len(added))
	for k, v := range tc.Noise {
		out.Noise[k] = v
	}
	for k, v := range added {
		if _, exists := out.Noise[k]; !exists {
			out.Noise[k] = v
		}
	}
	return &out
}

func hasPatterns(patterns []string) bool {
	for _, p := range patterns {
		if p != "" {
			return true
		}
	}
	return false
}

// patternReadable says a field's recorded protoscope value is the value a
// pattern written for its JSON form reads: a string, or a plainly encoded
// integer. Not a bool (recorded as 0/1), a sint (zigzag-encoded, which the
// recorded text does not undo), a float or fixed-width value (whose text
// depends on protoscope guessing the type), an enum (its number), a message
// or group (its fields) or bytes.
func patternReadable(fd protoreflect.FieldDescriptor) bool {
	if fd == nil {
		return false
	}
	switch fd.Kind() {
	case protoreflect.StringKind, protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return true
	}
	return false
}

// fieldNumberPath translates a body noise key written by field name
// ("order.created_at", "items.0.price", "body.order.createdAt") into its
// field-number path ("3.7", "4.2"). Array indices name every element, as the
// field-number path does. A key that is already numeric, names a field the
// descriptor does not have, or reaches into a map by key is not translated.
func fieldNumberPath(md protoreflect.MessageDescriptor, key string) (string, bool) {
	p, _, ok := fieldNumberPathOf(md, key)
	return p, ok
}

// fieldNumberPathOf is fieldNumberPath that also returns the field the path
// ends at.
func fieldNumberPathOf(md protoreflect.MessageDescriptor, key string) (string, protoreflect.FieldDescriptor, bool) {
	key = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(key), "body."), "decoded_data.")
	if key == "" {
		return "", nil, false
	}
	var nums []string
	var last protoreflect.FieldDescriptor
	cur := md
	for _, seg := range strings.Split(key, ".") {
		if _, err := strconv.Atoi(seg); err == nil {
			continue // an index (or an already-numeric key): every element
		}
		if cur == nil {
			return "", nil, false
		}
		fd := fieldByLowerName(cur, seg)
		if fd == nil {
			return "", nil, false
		}
		last = fd
		nums = append(nums, strconv.Itoa(int(fd.Number())))
		switch {
		case fd.IsMap():
			cur = nil // a map's entries are not named by key in a field-number path
		case fd.Message() != nil:
			cur = fd.Message()
		default:
			cur = nil
		}
	}
	if last == nil || len(nums) == 0 {
		return "", nil, false
	}
	return strings.Join(nums, "."), last, true
}

func fieldByLowerName(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if strings.ToLower(string(fd.Name())) == name || strings.ToLower(fd.JSONName()) == name {
			return fd
		}
	}
	return nil
}

// encodingOnlyNote is said when both sides decode to the same fields and
// values but the field-number comparison still failed.
const encodingOnlyNote = "the decoded fields are equal: the bodies differ only in how they are encoded (field order inside a list, packed vs unpacked repeated fields, a default value written explicitly), which the comparison of the recorded bytes sees"

// compareGRPCWithSchemas is the gRPC comparison both replay sites run. The
// verdict is the field-number comparison of the recorded bodies; the schemas
// only (a) translate noise written by field name into field numbers, (b) say
// which repeated fields are maps (compared in any order), (c) let a body
// difference be graded as a JSON one, and (d) show a failure's bodies with
// field names in Result.GrpcDecode.
func (r *Replayer) compareGRPCWithSchemas(tc *models.TestCase, actual *models.GrpcResp, testSetID string, emitFailureLogs, failedElsewhere bool) (bool, *models.Result) {
	view := r.grpcProtoViewFor(tc, testSetID)
	compared := r.withNameNoiseAsFieldNumbers(tc, view, testSetID)
	extra := []grpcMatcher.MatchOption{grpcMatcher.WithWarnOnce(r.grpcSchemas.firstWarning)}
	if view != nil && view.expected != nil {
		if md := view.noiseMessage(); md != nil {
			extra = append(extra, grpcMatcher.WithUnorderedFields(mapFieldPaths(md)))
		}
		// Grading reads both sides with ONE schema, the recorded one, as
		// the verdict compares them by field number: read with two, a
		// renamed field would look removed on one side and added on the
		// other. The per-side schemas are for the display only.
		grade := view.expected
		extra = append(extra, grpcMatcher.WithAssessmentDecoder(func(e, a string) (string, string, bool) {
			exp, err1 := decodeResponse(grade, view.method, e)
			act, err2 := decodeResponse(grade, view.method, a)
			if err1 != nil || err2 != nil {
				return "", "", false
			}
			return string(exp.JSON), string(act.JSON), true
		}))
	}
	pass, res := r.compareGRPCResp(compared, actual, testSetID, emitFailureLogs, failedElsewhere, extra...)
	r.showGrpcBodiesDecoded(res, view, tc.GrpcResp.AllMessages(), actual.AllMessages())
	return pass, res
}

// mapFieldPaths lists, by field-number path, every map field reachable from
// md (a map's entries have no order in protobuf), maps inside a map's value
// message included. A recursive message (a tree: Node{map attrs; repeated
// Node children}) is followed maxTypeRepeats levels deep, so its nested
// maps are listed too; the walk stops at maxMapFieldDepth and after
// maxMapFieldPaths paths, so a schema with several recursive fields cannot
// make it explode.
func mapFieldPaths(md protoreflect.MessageDescriptor) []string {
	var out []string
	var walk func(m protoreflect.MessageDescriptor, prefix string, seen map[protoreflect.FullName]int, depth int)
	walk = func(m protoreflect.MessageDescriptor, prefix string, seen map[protoreflect.FullName]int, depth int) {
		if m == nil || seen[m.FullName()] >= maxTypeRepeats || depth > maxMapFieldDepth || len(out) >= maxMapFieldPaths {
			return
		}
		seen[m.FullName()]++
		defer func() { seen[m.FullName()]-- }()
		fields := m.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			p := strconv.Itoa(int(fd.Number()))
			if prefix != "" {
				p = prefix + "." + p
			}
			if fd.IsMap() {
				out = append(out, p)
				// A map's value may be a message holding maps of its own:
				// entries are field 1 (key) and 2 (value).
				if v := fd.MapValue(); v.Message() != nil {
					walk(v.Message(), p+".2", seen, depth+1)
				}
				continue
			}
			if fd.Message() != nil {
				walk(fd.Message(), p, seen, depth+1)
			}
		}
	}
	walk(md, "", map[protoreflect.FullName]int{}, 0)
	sort.Strings(out)
	return out
}

const (
	maxMapFieldDepth = 32
	// maxTypeRepeats is how many times one message type may recur on a
	// path: a tree's maps a few levels down are listed; deeper ones keep
	// their order, as without a schema.
	maxTypeRepeats = 8
	// maxMapFieldPaths bounds the list a schema with several recursive
	// fields could otherwise grow without end.
	maxMapFieldPaths = 4096
)

// showGrpcBodiesDecoded says, in Result.GrpcDecode, which schema reads each
// side of a failed gRPC test, and shows the first differing message of each
// side with field names (MessageIndex: which message of a stream). The
// verdict and BodyResult -- the recorded protoscope the comparison read, which
// reports and noise learners consume -- are not touched. Both sides are
// decoded or neither is: a recorded side with no schema leaves both as
// recorded, with the reason. The console's diff, printed by the comparison,
// shows the recorded form.
func (r *Replayer) showGrpcBodiesDecoded(res *models.Result, view *grpcProtoView, expMsgs, actMsgs []models.GrpcLengthPrefixedMessage) {
	if res == nil || view == nil {
		return
	}
	row := -1
	idx := 0
	for i := range res.BodyResult {
		if res.BodyResult[i].Type != models.GrpcData {
			continue
		}
		if !res.BodyResult[i].Normal {
			row = idx
			break
		}
		idx++
	}
	if row < 0 {
		return
	}
	info := &models.GrpcDecodeInfo{Note: view.note, MessageIndex: row}
	res.GrpcDecode = info
	if view.expected == nil || view.actual == nil {
		if info.Note == "" {
			info.Note = "no schema for the new response: give the run its protos (--proto-dir) or upload them for the release being replayed"
		}
		return
	}
	if row >= len(expMsgs) || row >= len(actMsgs) {
		info.Note = "the streams carry a different number of messages, so the differing message is shown as recorded"
		return
	}
	exp, expErr := decodeResponse(view.expected, view.method, expMsgs[row].DecodedData)
	act, actErr := decodeResponse(view.actual, view.method, actMsgs[row].DecodedData)
	if expErr != nil || actErr != nil {
		info.Note = "the bodies could not be decoded with their schemas, so they are shown as recorded: " + firstErr(expErr, actErr).Error()
		return
	}
	info.ExpectedJSON, info.ActualJSON = string(exp.JSON), string(act.JSON)
	info.Expected, info.Actual = view.expectedLabel, view.actualLabel
	info.ExpectedUnknownFields, info.ActualUnknownFields = exp.UnknownFields, act.UnknownFields
	if sameJSON(exp.JSON, act.JSON) {
		info.Note = encodingOnlyNote
	}
}

func decodeResponse(s *protoschema.Schema, method, body string) (*protoschema.Decoded, error) {
	_, resp, err := s.MessageTypes(method)
	if err != nil {
		return nil, err
	}
	return s.DecodeProtoscope(resp, body)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
