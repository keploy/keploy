package http

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// missWith matches the live POST /x (liveHdr, liveBody) against rec, with
// keploy.yml's noise (cfg, as keploy.yml writes it), keploy's own header
// noise unless noAuto, and the switch on or off.
// It returns whether the call was served and, when it was not, its miss
// report, as the agent builds them.
func missWith(t *testing.T, on, noAuto bool, cfg map[string]map[string][]string, rec *models.Mock, liveHdr map[string]string, liveBody string) (bool, *models.MockMismatchReport) {
	t.Helper()
	if on {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
	noise := NewRequestNoise(zap.NewNop(), models.OutgoingOptions{NoiseConfig: cfg, DisableAutoHeaderNoise: noAuto})
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", liveHdr, liveBody), db, noise.Header, noise.Body, noise.URL, true, false, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		return true, nil
	}
	request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(liveBody))
	for k, v := range liveHdr {
		request.Header.Set(k, v)
	}
	request.Header.Set("Host", request.Host) // as decode sets it
	r := newHTTP().buildHTTPMismatchReport(request, []byte(liveBody), &mockMemDb{mocks: []*models.Mock{rec}}, nil, noise.Header, noise.Body, diag)
	if r == nil {
		t.Fatal("no miss report")
	}
	return false, r
}

// The hint offers keploy.yml noise for a miss only where declaring it makes
// the call match, with the switch on and off, and names it as keploy.yml
// writes it:
//   - Without the switch the matcher lets a call carry headers its recording
//     does not, so such a header is no part of the remedy; under the switch
//     every header is compared, so it is.
//   - A recorded AWS SigV4 Authorization is never offered: under the switch
//     its parts are compared so that only its signature may differ, and
//     declaring the header would stop that. Another scheme is offered.
//     (Without the switch Authorization is among the headers keploy itself
//     treats as noise, so it causes a miss only with that noise disabled.)
//   - Without the switch the schema phase refuses a recording on its
//     Content-Type's media type and its body's type whatever the noise says:
//     the miss names them, and the hint offers no header noise for a call
//     that would still miss.
//   - A field keploy.yml already guards with patterns is to have its entry
//     changed, not added.
//
// Each offered remedy, declared, serves the call; a call offered none still
// misses with the headers the hint would otherwise have named declared.
// Before, a call that carried a new header was offered only recording again
// without the switch, a SigV4 Authorization was offered as header noise, the
// two schema refusals were not named (a missing header beside them was
// offered noise that left the call a miss), and a guarded field was "added".
func TestTheHintOffersOnlyNoiseThatMakesTheCallMatch(t *testing.T) {
	sigV4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + strings.Repeat("a", 64)
	const json = `{"a":1}`
	const rerecord = "re-record with 'keploy record'"
	const structural = "Request structure changed since recording. Re-record the test set with 'keploy record', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping."
	hdr := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	header := func(kv ...string) map[string]map[string][]string {
		m := map[string][]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				m[kv[i]] = []string{}
			} else {
				m[kv[i]] = []string{kv[i+1]}
			}
		}
		return map[string]map[string][]string{"header": m}
	}
	for _, c := range []struct {
		name            string
		on, noAuto      bool
		cfg             map[string]map[string][]string
		recHdr, liveHdr map[string]string
		recBody, live   string
		diffs           []models.MockFieldDiff // each among the report's field diffs
		hint            string
		fix             map[string]map[string][]string // the noise the hint offers: declared, the call is served
		stillMisses     map[string]map[string][]string // a call offered no noise still misses with these declared
	}{
		{name: "a recorded header missing and one only the call carries, switch off", recHdr: hdr("Referer", "http://localhost/app"), liveHdr: hdr("X-New", "1"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive}, {Path: "header.X-New", Kind: models.DiffKindMissingInMock}},
			hint:  "Recorded headers are missing from this call (header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise " + rerecord + ".",
			fix:   header("referer", "")},
		{name: "a recorded header missing and one only the call carries, switch on", on: true, recHdr: hdr("Referer", "http://localhost/app"), liveHdr: hdr("X-New", "1"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.referer", Kind: models.DiffKindMissingInLive}, {Path: "header.x-new", Kind: models.DiffKindMissingInMock}},
			hint:        "Recorded headers are missing from this call (header.referer) and it carries headers its recording does not (header.x-new). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: [], x-new: []}); otherwise " + rerecord + ".",
			fix:         header("referer", "", "x-new", ""),
			stillMisses: header("referer", "")},
		{name: "a header only the call carries, switch on", on: true, recHdr: hdr(), liveHdr: hdr("X-New", "1"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.x-new", Kind: models.DiffKindMissingInMock}},
			hint:  "This call carries headers its recording does not (header.x-new). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {x-new: []}); otherwise " + rerecord + ".",
			fix:   header("x-new", "")},
		{name: "a recorded SigV4 Authorization missing, switch off, keploy's header noise disabled", noAuto: true, recHdr: hdr("Authorization", sigV4), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Authorization", Kind: models.DiffKindMissingInLive}}, hint: structural},
		{name: "a recorded Authorization of another scheme missing, switch off, keploy's header noise disabled", noAuto: true, recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Authorization", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.Authorization). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {authorization: []}); otherwise " + rerecord + ".",
			fix:   header("authorization", "")},
		{name: "a recorded SigV4 Authorization missing, switch on", on: true, recHdr: hdr("Authorization", sigV4), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.authorization", Kind: models.DiffKindMissingInLive}}, hint: structural},
		{name: "a recorded SigV4 Authorization signed another way, switch on", on: true, recHdr: hdr("Authorization", sigV4), liveHdr: hdr("Authorization", "Bearer t"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.authorization", Kind: models.DiffKindValueChanged}},
			hint:  "Only values drifted (header.authorization). If the change is expected, " + rerecord + "."},
		{name: "a recorded Authorization of another scheme missing, switch on", on: true, recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.authorization", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.authorization). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {authorization: []}); otherwise " + rerecord + ".",
			fix:   header("authorization", "")},
		{name: "a Content-Type the schema phase refuses, switch off", recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.Content-Type", Kind: models.DiffKindValueChanged, Expected: "application/json", Actual: "text/plain"}},
			hint:        "Only values drifted (header.Content-Type). If the change is expected, " + rerecord + ".",
			stillMisses: header("content-type", "")},
		{name: "a Content-Type the schema phase refuses beside a recorded header missing, switch off", recHdr: hdr("Content-Type", "application/json", "Referer", "http://localhost/app"), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.Content-Type", Kind: models.DiffKindValueChanged}, {Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:        structural,
			stillMisses: header("referer", "", "content-type", "")},
		// The schema phase reads Content-Type only when both sides send one.
		{name: "a Content-Type only the recording sends beside a recorded header missing, switch off", recHdr: hdr("Content-Type", "application/json", "Referer", "http://localhost/app"), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Content-Type", Kind: models.DiffKindMissingInLive}, {Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.Content-Type, header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {content-type: [], referer: []}); otherwise " + rerecord + ".",
			fix:   header("content-type", "", "referer", "")},
		{name: "a Content-Type only the call sends beside a recorded header missing, switch off", recHdr: hdr("Referer", "http://localhost/app"), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Content-Type", Kind: models.DiffKindMissingInMock}, {Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise " + rerecord + ".",
			fix:   header("referer", "")},
		{name: "a body of another type beside a recorded header missing, switch off", recHdr: hdr("Referer", "http://localhost/app"), liveHdr: hdr(), recBody: json, live: `<a>1</a>`,
			diffs:       []models.MockFieldDiff{{Path: "body", Kind: models.DiffKindTypeChanged, Expected: string(models.JSON), Actual: string(models.XML)}, {Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:        structural,
			stillMisses: header("referer", "")},
		{name: "a header keploy.yml guards with patterns missing, switch on", on: true, cfg: header("X-Id", `^a-\d$`), recHdr: hdr("X-Id", "a-1"), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.x-id", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.x-id). If they are optional, change the headers' entries under test.globalNoise.global.header to empty lists (e.g. header: {x-id: []}); otherwise " + rerecord + ".",
			fix:   header("X-Id", "")},
		{name: "a header keploy.yml guards with patterns drifted outside them, switch on", on: true, cfg: header("X-Id", `^a-\d$`), recHdr: hdr("X-Id", "a-1"), liveHdr: hdr("X-Id", "evil"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.x-id", Kind: models.DiffKindValueChanged}},
			hint:  "Only values drifted (header.x-id). If these are dynamic (timestamps, ids, tokens), change the headers' entries under test.globalNoise.global.header to empty lists (e.g. header: {x-id: []}); otherwise " + rerecord + ".",
			fix:   header("X-Id", "")},
		{name: "a body field keploy.yml guards with patterns drifted outside them, switch off", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"request_id":"r-1"}`, live: `{"request_id":"evil"}`,
			diffs: []models.MockFieldDiff{{Path: "body.request_id", Kind: models.DiffKindValueChanged}},
			hint:  "Only values drifted (body.request_id). If these are dynamic (timestamps, ids, tokens), change the request-body fields' entries under test.globalNoise.global.requestbody to empty lists (e.g. requestbody: {request_id: []}); otherwise " + rerecord + ".",
			fix:   map[string]map[string][]string{"requestbody": {"request_id": {}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := func() *models.Mock { return recorded("m", "/x", c.recHdr, c.recBody) }
			served, r := missWith(t, c.on, c.noAuto, c.cfg, rec(), c.liveHdr, c.live)
			if served {
				t.Fatal("served; want a miss")
			}
			for _, want := range c.diffs {
				if !slices.ContainsFunc(r.FieldDiffs, func(d models.MockFieldDiff) bool {
					return d.Path == want.Path && d.Kind == want.Kind && (want.Expected == "" || d.Expected == want.Expected) && (want.Actual == "" || d.Actual == want.Actual)
				}) {
					t.Errorf("field diffs %+v; want %+v among them", r.FieldDiffs, want)
				}
			}
			if r.NextSteps != c.hint {
				t.Errorf("hint:\n got  %q\n want %q", r.NextSteps, c.hint)
			}
			if c.fix != nil {
				if served, r := missWith(t, c.on, c.noAuto, c.fix, rec(), c.liveHdr, c.live); !served {
					t.Errorf("declaring the offered noise %v leaves a miss: %+v", c.fix, r.FieldDiffs)
				}
			}
			if c.stillMisses != nil {
				if served, _ := missWith(t, c.on, c.noAuto, c.stillMisses, rec(), c.liveHdr, c.live); served {
					t.Errorf("declaring %v serves the call, yet the hint names no such remedy", c.stillMisses)
				}
			}
		})
	}
}

// Under the switch the strict comparison reads Content-Type and the body
// noise aware, so a miss built from the closest recording of the pool when
// the strict comparison had none names no schema-phase type refusal: the
// schema phase never ran.
func TestUnderTheSwitchTheMissNamesNoSchemaTypeRefusal(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := recorded("m", "/x", map[string]string{"Content-Type": "application/json"}, `{"a":1}`)
	request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(`<a>1</a>`))
	request.Header.Set("Content-Type", "text/plain")
	diag := &matchDiag{phase: models.MatchPhaseSchema, candidates: 1, pool: []*models.Mock{rec}}
	r := newHTTP().buildHTTPMismatchReport(request, []byte(`<a>1</a>`), &mockMemDb{mocks: []*models.Mock{rec}}, nil, nil, nil, diag)
	for _, d := range r.FieldDiffs {
		if d.Path == "header.Content-Type" || d.Path == "body" {
			t.Errorf("field diffs %+v name a schema-phase refusal under the switch", r.FieldDiffs)
		}
	}
}

// The fields a strict miss names as keploy.yml's patterns refused are those
// with a reason that is no refused read of the application's clock, and a
// recording's SigV4 Authorization is named in the case it was recorded in.
func TestTheHintsNoiseFactsOfAStrictMiss(t *testing.T) {
	fields := []strictField{
		{diff: models.MockFieldDiff{Path: "header.x-id"}, why: "keploy.yml's noise lets header.x-id vary only within ..."},
		{diff: models.MockFieldDiff{Path: "body.since"}, why: "body.since was learned as a read of the application's clock", clock: true},
		{diff: models.MockFieldDiff{Path: "body.qty"}},
	}
	if got := guardedPaths(fields); !slices.Equal(got, []string{"header.x-id"}) {
		t.Errorf("guarded %v; want [header.x-id]", got)
	}
	sigV4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=" + strings.Repeat("a", 64)
	for _, c := range []struct {
		header map[string]string
		want   []string
	}{
		{map[string]string{"authorization": sigV4}, []string{"header.authorization"}},
		{map[string]string{"Authorization": sigV4}, []string{"header.Authorization"}},
		{map[string]string{"Authorization": "Bearer t"}, nil},
		{map[string]string{"X-Auth": sigV4}, nil},
	} {
		if got := signedAuthorization(recorded("m", "/x", c.header, "")); !slices.Equal(got, c.want) {
			t.Errorf("signedAuthorization(%v) = %v; want %v", c.header, got, c.want)
		}
	}
}
