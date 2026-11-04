package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/matcher"
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
	return missIn(t, on, noAuto, cfg, []*models.Mock{rec}, liveHdr, liveBody)
}

// missIn is missWith with recs, in their order, as the call's pool.
func missIn(t *testing.T, on, noAuto bool, cfg map[string]map[string][]string, recs []*models.Mock, liveHdr map[string]string, liveBody string) (bool, *models.MockMismatchReport) {
	t.Helper()
	if on {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
	noise := NewRequestNoise(zap.NewNop(), models.OutgoingOptions{NoiseConfig: cfg, DisableAutoHeaderNoise: noAuto})
	db := &mockMemDb{mocks: recs, updateUnFilteredReturn: true}
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
	r := newHTTP().buildHTTPMismatchReport(request, []byte(liveBody), &mockMemDb{mocks: recs}, nil, noise.Header, noise.Body, diag)
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
//     every header is compared (except host, content-length and keploy*
//     headers and those keploy's own or keploy.yml's header noise names),
//     so it is.
//   - No Authorization, recorded or only the call's, and no
//     x-amz-content-sha256 is offered: under the switch a recorded SigV4
//     Authorization's parts are compared so that it may differ only in its
//     access key id, date and signature, and an x-amz-content-sha256 with
//     the body the call sent; declaring either header would stop that for
//     every recording it reaches (TestTheHintOffersNoAuthorizationNoise for
//     an Authorization of another scheme). (Without the switch both are
//     among the headers keploy itself treats as noise, so they cause a miss
//     only with that noise disabled.)
//   - Without the switch the schema phase refuses a recording on its
//     Content-Type's media type and its body's type whatever the noise says:
//     the miss names them, and the hint offers no header noise for a call
//     that would still miss, even when the refusal is past the 25 diffs the
//     report carries (the causes first). A Content-Type that differs only
//     in its parameters is no refusal. Nor is one the recording names under
//     a key in another case, a gap of the matcher: the schema phase reads
//     the key "Content-Type" exactly, so it skips the media-type check for
//     such a recording (recordings read by http.ReadRequest never hit it,
//     as it canonicalises keys), and the report mirrors it.
//   - A field keploy.yml already guards with patterns is to have its
//     patterns widened to match the value outside them, which the hint
//     names by its shape with the patterns: never added, and never emptied,
//     which would stop the field being compared. A pattern matches no
//     absent value, so a guarded field missing on a side is offered no
//     noise.
//
// Each offered remedy, declared, serves the call; a call offered none still
// misses with the headers the hint would otherwise have named declared.
// Before, a call that carried a new header was offered only recording again
// without the switch, a SigV4 Authorization was offered as header noise, the
// two schema refusals were not named (a missing header beside them was
// offered noise that left the call a miss), and a guarded field was "added";
// then its entry was to be emptied, which stops it being compared at all.
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
	// numbered is m with n headers named prefix and a number from 00.
	numbered := func(m map[string]string, prefix string, n int) map[string]string {
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("%s%02d", prefix, i)] = "1"
		}
		return m
	}
	payloadHash := sha256.Sum256([]byte(json))
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
	// recordedMissing declares the Referer and X-R00 to X-R23 with empty
	// lists, as header's pairs.
	recordedMissing := []string{"referer", ""}
	for i := 0; i < 24; i++ {
		recordedMissing = append(recordedMissing, fmt.Sprintf("x-r%02d", i), "")
	}
	for _, c := range []struct {
		name            string
		on, noAuto      bool
		cfg             map[string]map[string][]string
		recHdr, liveHdr map[string]string
		recBody, live   string
		diffs           []models.MockFieldDiff // each among the report's field diffs
		cut             []string               // paths the report does not carry, past the 25 it does
		full            bool                   // the report is cut to 25
		hint            string
		fix             map[string]map[string][]string // the noise the hint offers: declared, the call is served
		stillMisses     map[string]map[string][]string // a call offered no noise still misses with these declared
		// values are, by keploy.yml entry, each side's values of the field
		// it guards with patterns, which the user reads off the call to
		// widen them (remedyOf).
		values map[string]map[string][]string
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
		{name: "a recorded SigV4 Authorization missing, switch on", on: true, recHdr: hdr("Authorization", sigV4), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.authorization", Kind: models.DiffKindMissingInLive}}, hint: structural},
		{name: "a recorded SigV4 Authorization signed another way, switch on", on: true, recHdr: hdr("Authorization", sigV4), liveHdr: hdr("Authorization", "Bearer t"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.authorization", Kind: models.DiffKindValueChanged}},
			hint:  "Only values drifted (header.authorization). If the change is expected, " + rerecord + "."},
		{name: "a SigV4 Authorization only the call carries, switch on", on: true, recHdr: hdr(), liveHdr: hdr("Authorization", sigV4), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.authorization", Kind: models.DiffKindMissingInMock}}, hint: structural},
		// An x-amz-content-sha256 is compared with the body the call sent
		// under the switch, and is keploy's own header noise without it.
		{name: "an x-amz-content-sha256 that is no hash of the body the call sent, switch on", on: true,
			recHdr: hdr("X-Amz-Content-Sha256", hex.EncodeToString(payloadHash[:])), liveHdr: hdr("X-Amz-Content-Sha256", strings.Repeat("0", 64)), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.x-amz-content-sha256", Kind: models.DiffKindValueChanged}},
			hint:  "Only values drifted (header.x-amz-content-sha256). If the change is expected, " + rerecord + "."},
		{name: "a recorded x-amz-content-sha256 missing, switch off, keploy's header noise disabled", noAuto: true,
			recHdr: hdr("X-Amz-Content-Sha256", hex.EncodeToString(payloadHash[:])), liveHdr: hdr(), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.X-Amz-Content-Sha256", Kind: models.DiffKindMissingInLive}}, hint: structural},
		{name: "a Content-Type the schema phase refuses, switch off", recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.Content-Type", Kind: models.DiffKindValueChanged, Expected: "application/json", Actual: "text/plain"}},
			hint:        "Only values drifted (header.Content-Type). If the change is expected, " + rerecord + ".",
			stillMisses: header("content-type", "")},
		{name: "a Content-Type the schema phase refuses beside a recorded header missing, switch off", recHdr: hdr("Content-Type", "application/json", "Referer", "http://localhost/app"), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.Content-Type", Kind: models.DiffKindValueChanged}, {Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:        structural,
			stillMisses: header("referer", "", "content-type", "")},
		// The report carries 25 diffs, the causes first: without the switch
		// 24 headers only the call carries are no cause, so the refused
		// Content-Type after them is carried ahead of them, and keeps the
		// noise out.
		{name: "a Content-Type the schema phase refuses behind 24 headers only the call carries, beside a recorded header missing, switch off",
			recHdr: hdr("Content-Type", "application/json", "Referer", "http://localhost/app"), liveHdr: numbered(hdr("Content-Type", "text/plain"), "X-H", 24), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive}, {Path: "header.Content-Type", Kind: models.DiffKindValueChanged}},
			full:        true,
			hint:        structural,
			stillMisses: header("referer", "")},
		// 25 recorded headers missing are causes ahead of the refused
		// Content-Type, which the report cuts: the hint still reads it.
		{name: "a Content-Type the schema phase refuses past the 25 diffs the report carries, behind recorded headers missing, switch off",
			recHdr: numbered(hdr("Content-Type", "application/json", "Referer", "http://localhost/app"), "X-R", 24), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive}, {Path: "header.X-R23", Kind: models.DiffKindMissingInLive}},
			cut:         []string{"header.Content-Type"},
			hint:        structural,
			stillMisses: header(recordedMissing...)},
		// The schema phase compares media types, not their parameters.
		{name: "a Content-Type that differs only in its parameters beside a recorded header missing, switch off",
			recHdr: hdr("Content-Type", "application/json", "Referer", "http://localhost/app"), liveHdr: hdr("Content-Type", "application/json; charset=utf-8"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise " + rerecord + ".",
			fix:   header("referer", "")},
		// It reads the recording's Content-Type under the key "Content-Type"
		// exactly, and the report mirrors it (see above).
		{name: "a Content-Type recorded under a lowercase key beside a recorded header missing, switch off",
			recHdr: hdr("content-type", "application/json", "Referer", "http://localhost/app"), liveHdr: hdr("Content-Type", "text/plain"), recBody: json, live: json,
			diffs: []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive}},
			hint:  "Recorded headers are missing from this call (header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise " + rerecord + ".",
			fix:   header("referer", "")},
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
		// No pattern matches an absent value: widening the patterns leaves
		// the call a miss, so the hint offers recording again only.
		{name: "a header keploy.yml guards with patterns missing, switch on", on: true, cfg: header("X-Id", `^a-\d$`), recHdr: hdr("X-Id", "a-1"), liveHdr: hdr(), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.x-id", Kind: models.DiffKindMissingInLive}},
			hint:        structural,
			stillMisses: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`, `.*`}}}},
		{name: "a header keploy.yml guards with patterns drifted outside them, switch on", on: true, cfg: header("X-Id", `^a-\d$`), recHdr: hdr("X-Id", "a-1"), liveHdr: hdr("X-Id", "evil"), recBody: json, live: json,
			diffs:  []models.MockFieldDiff{{Path: "header.x-id", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (header.x-id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise header entries for x-id (\"^a-\\\\d$\") so one matches the live value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`, `^evil$`}}},
			values: map[string]map[string][]string{"x-id": {"live": {"evil"}}}},
		// The recorded value outside them, the live one inside: the hint
		// names the recorded side.
		{name: "a header keploy.yml guards with patterns recorded outside them, switch on", on: true, cfg: header("X-Id", `^a-\d$`), recHdr: hdr("X-Id", "old-12"), liveHdr: hdr("X-Id", "a-1"), recBody: json, live: json,
			diffs:  []models.MockFieldDiff{{Path: "header.x-id", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (header.x-id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise header entries for x-id (\"^a-\\\\d$\") so one matches the recorded value (a string of 6 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`, `^old-\d+$`}}},
			values: map[string]map[string][]string{"x-id": {"recorded": {"old-12"}}}},
		{name: "a body field keploy.yml guards with patterns drifted outside them, switch off", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"request_id":"r-1"}`, live: `{"request_id":"evil"}`,
			diffs:  []models.MockFieldDiff{{Path: "body.request_id", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.request_id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for request_id (\"^r-\\\\d$\") so one matches the live value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`, `^evil$`}}},
			values: map[string]map[string][]string{"request_id": {"live": {"evil"}}}},
		{name: "a body field keploy.yml guards with patterns drifted outside them, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"request_id":"r-1"}`, live: `{"request_id":12345}`,
			diffs: []models.MockFieldDiff{{Path: "body.request_id", Kind: models.DiffKindTypeChanged}},
			hint:  structural},
		{name: "a form field keploy.yml guards with patterns drifted outside them, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`}}},
			recHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), liveHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), recBody: "code=c1", live: "code=zz9",
			diffs:  []models.MockFieldDiff{{Path: "body.code", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so one matches the live value (a string of 3 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"code": {`^c\d$`, `^zz9$`}}},
			values: map[string]map[string][]string{"code": {"live": {"zz9"}}}},
		// Both sides outside the patterns: widened to match the live value
		// alone, as the hint once said, they leave the call a miss, so the
		// hint names both, with the switch on and off.
		{name: "a header keploy.yml guards with patterns, both sides outside them, switch on", on: true, cfg: header("X-Id", `^a-\d$`), recHdr: hdr("X-Id", "b-12"), liveHdr: hdr("X-Id", "evil"), recBody: json, live: json,
			diffs:       []models.MockFieldDiff{{Path: "header.x-id", Kind: models.DiffKindValueChanged}},
			hint:        "Only values drifted (header.x-id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise header entries for x-id (\"^a-\\\\d$\") so they match the live value (a string of 4 bytes) and the recorded value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:         map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`, `^evil$`, `^b-12$`}}},
			stillMisses: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`, `^evil$`}}},
			values:      map[string]map[string][]string{"x-id": {"live": {"evil"}, "recorded": {"b-12"}}}},
		{name: "a body field keploy.yml guards with patterns, both sides outside them, switch off", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"request_id":"x-12"}`, live: `{"request_id":"evil"}`,
			diffs:       []models.MockFieldDiff{{Path: "body.request_id", Kind: models.DiffKindValueChanged}},
			hint:        "Only values drifted (body.request_id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for request_id (\"^r-\\\\d$\") so they match the live value (a string of 4 bytes) and the recorded value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:         map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`, `^evil$`, `^x-12$`}}},
			stillMisses: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`, `^evil$`}}},
			values:      map[string]map[string][]string{"request_id": {"live": {"evil"}, "recorded": {"x-12"}}}},
		{name: "a body field keploy.yml guards with patterns, both sides outside them, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"request_id":"x-12"}`, live: `{"request_id":"evil"}`,
			diffs:       []models.MockFieldDiff{{Path: "body.request_id", Kind: models.DiffKindValueChanged}},
			hint:        "Only values drifted (body.request_id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for request_id (\"^r-\\\\d$\") so they match the live value (a string of 4 bytes) and the recorded value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:         map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`, `^evil$`, `^x-12$`}}},
			stillMisses: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`, `^evil$`}}},
			values:      map[string]map[string][]string{"request_id": {"live": {"evil"}, "recorded": {"x-12"}}}},
		{name: "a form field keploy.yml guards with patterns, both sides outside them, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`}}},
			recHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), liveHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), recBody: "code=q77", live: "code=zz9",
			diffs:       []models.MockFieldDiff{{Path: "body.code", Kind: models.DiffKindValueChanged}},
			hint:        "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so they match the live value (a string of 3 bytes) and the recorded value (a string of 3 bytes); otherwise " + rerecord + ".",
			fix:         map[string]map[string][]string{"requestbody": {"code": {`^c\d$`, `^zz9$`, `^q77$`}}},
			stillMisses: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`, `^zz9$`}}},
			values:      map[string]map[string][]string{"code": {"live": {"zz9"}, "recorded": {"q77"}}}},
		// A form field's every value outside the patterns is named.
		{name: "a form field keploy.yml guards with patterns, two live values outside them, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`}}},
			recHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), liveHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), recBody: "code=c1&code=c2", live: "code=zz9&code=c2&code=yy88",
			diffs:       []models.MockFieldDiff{{Path: "body.code", Kind: models.DiffKindValueChanged}},
			hint:        "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so they match the live values (a string of 3 bytes, a string of 4 bytes); otherwise " + rerecord + ".",
			stillMisses: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`, `^zz9$`}}},
			values:      map[string]map[string][]string{"code": {"live": {"zz9", "yy88"}}}},
		// A pattern matches a single value: for an array whose length
		// changed no widening serves, so the hint offers recording again,
		// and says why. Without the switch such a call is served.
		{name: "an array keploy.yml guards with patterns whose length changed, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"items": {`^\d+$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"items":[1,2]}`, live: `{"items":[1,2,3]}`,
			diffs:       []models.MockFieldDiff{{Path: "body.items", Kind: models.DiffKindValueChanged}},
			hint:        "Only values drifted (body.items). keploy.yml's noise lets body.items vary only within \"^\\\\d+$\", and a pattern matches a single value, never an array or an object. If the change is expected, " + rerecord + ".",
			stillMisses: map[string]map[string][]string{"requestbody": {"items": {`^\d+$`, `.*`}}}},
		// The hint names the entry that holds the patterns, which may name
		// the field by its last name alone or name its array: widening that
		// entry serves the call.
		{name: "a nested body field an entry names by its last name, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"qty": {`^\d{3}$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"order":{"qty":123}}`, live: `{"order":{"qty":1234}}`,
			diffs:  []models.MockFieldDiff{{Path: "body.order.qty", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.order.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^\\\\d{3}$\") so one matches the live value (a number of 4 characters); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"qty": {`^\d{3}$`, `^\d{4}$`}}},
			values: map[string]map[string][]string{"qty": {"live": {"1234"}}}},
		{name: "array elements an entry names by their array, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"ids": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"ids":[1,2,3]}`, live: `{"ids":[1,22,333]}`,
			diffs:  []models.MockFieldDiff{{Path: "body.ids[]", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.ids[], body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the live values (a number of 2 characters, a number of 3 characters); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"ids": {`^\d$`, `^\d\d$`, `^\d\d\d$`}}},
			values: map[string]map[string][]string{"ids": {"live": {"22", "333"}}}},
		{name: "recorded array elements an entry names by their array, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"ids": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"ids":[11,2,333]}`, live: `{"ids":[1,2,3]}`,
			diffs:  []models.MockFieldDiff{{Path: "body.ids[]", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.ids[], body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the recorded values (a number of 2 characters, a number of 3 characters); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"ids": {`^\d$`, `^\d\d$`, `^\d\d\d$`}}},
			values: map[string]map[string][]string{"ids": {"recorded": {"11", "333"}}}},
		// A guarded field beside one keploy.yml does not name: the one is
		// added, the other's patterns widened.
		{name: "a guarded body field beside a plain one, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"a": {`^x$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"a":"x","b":1}`, live: `{"a":"yy","b":2}`,
			diffs:  []models.MockFieldDiff{{Path: "body.a", Kind: models.DiffKindValueChanged}, {Path: "body.b", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.a, body.b). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody with root-relative keys (e.g. requestbody: {b: []}) and widen the patterns of keploy.yml's test.globalNoise requestbody entries for a (\"^x$\") so one matches the live value (a string of 2 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"a": {`^x$`, `^yy$`}, "b": {}}},
			values: map[string]map[string][]string{"a": {"live": {"yy"}}}},
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
			if (c.full || len(c.cut) > 0) && len(r.FieldDiffs) != 25 {
				t.Errorf("the report carries %d field diffs; want the 25 it is cut to", len(r.FieldDiffs))
			}
			for _, path := range c.cut {
				if slices.ContainsFunc(r.FieldDiffs, func(d models.MockFieldDiff) bool { return d.Path == path }) {
					t.Errorf("field diffs %+v carry %s; want it past the cut", r.FieldDiffs, path)
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
			// The rule every hint meets: declaring exactly the remedy it
			// names serves the call, or it names recording again alone.
			if remedy, offered := remedyOf(t, r.NextSteps, c.cfg, c.values); offered {
				if served, r := missWith(t, c.on, c.noAuto, remedy, rec(), c.liveHdr, c.live); !served {
					t.Errorf("declaring the remedy the hint names, %v, leaves a miss; its hint then %q", remedy, r.NextSteps)
				}
			} else if !strings.Contains(strings.ToLower(r.NextSteps), "re-record") {
				t.Errorf("hint %q offers neither noise nor recording again", r.NextSteps)
			}
		})
	}
}

// remedyOf is the keploy.yml noise a user declares from hint, over cfg, and
// whether the hint offers any: each field it says to add, with no patterns,
// and each entry whose patterns it says to widen with the patterns it quotes
// and, for each side it says they are to match, one matching exactly each
// of that side's values (values: entry, side, values).
func remedyOf(t *testing.T, hint string, cfg map[string]map[string][]string, values map[string]map[string][]string) (map[string]map[string][]string, bool) {
	t.Helper()
	out := map[string]map[string][]string{}
	for bucket, entries := range cfg {
		out[bucket] = maps.Clone(entries)
	}
	// set declares key under bucket with pats, as the entry cfg names in
	// any case, which keploy.yml reads so.
	set := func(bucket, key string, pats []string) {
		if out[bucket] == nil {
			out[bucket] = map[string][]string{}
		}
		for k := range out[bucket] {
			if strings.EqualFold(k, key) {
				key = k
			}
		}
		out[bucket][key] = pats
	}
	offered := false
	// Only the fields to add are given by example.
	for _, m := range regexp.MustCompile(`\(e\.g\. (\w+): \{([^}]*)\}\)`).FindAllStringSubmatch(hint, -1) {
		for _, key := range strings.Split(m[2], ", ") {
			set(m[1], strings.TrimSuffix(key, ": []"), []string{})
			offered = true
		}
	}
	widen := regexp.MustCompile(`widen the patterns of keploy\.yml's test\.globalNoise (\w+) entries for `)
	item := regexp.MustCompile(`(\S+) \(((?:"(?:[^"\\]|\\.)*"(?:, )?)+)\) so (?:one matches|they match) (the (?:live|recorded) values? \([^)]*\)(?: and the (?:live|recorded) values? \([^)]*\))?)`)
	quoted := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	starts := widen.FindAllStringSubmatchIndex(hint, -1)
	for i, at := range starts {
		end := len(hint)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		bucket, segment := hint[at[2]:at[3]], hint[at[1]:end]
		items := item.FindAllStringSubmatch(segment, -1)
		if len(items) == 0 {
			t.Fatalf("hint %q says to widen patterns it does not name", hint)
		}
		for _, it := range items {
			key := it[1]
			var pats []string
			for _, q := range quoted.FindAllString(it[2], -1) {
				p, err := strconv.Unquote(q)
				if err != nil {
					t.Fatal(err)
				}
				pats = append(pats, p)
			}
			for _, side := range []string{"live", "recorded"} {
				if !strings.Contains(it[3], "the "+side+" value") {
					continue
				}
				vs := values[key][side]
				if len(vs) == 0 {
					t.Fatalf("the row names no %s value of entry %s, which hint %q names", side, key, hint)
				}
				for _, v := range vs {
					pats = append(pats, "^"+regexp.QuoteMeta(v)+"$")
				}
			}
			set(bucket, key, pats)
			offered = true
		}
	}
	return out, offered
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

// Under the switch, a miss built from the closest recording of the pool (the
// strict comparison named none) offers no noise for a SigV4 Authorization
// only the call carries, as a strict miss offers none.
func TestUnderTheSwitchThePoolsClosestOffersNoSigV4Noise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	sigV4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + strings.Repeat("a", 64)
	rec := recorded("m", "/x", nil, `{"a":1}`)
	request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(`{"a":1}`))
	request.Header.Set("Authorization", sigV4)
	request.Header.Set("Host", request.Host) // as decode sets it
	diag := &matchDiag{phase: models.MatchPhaseSchema, candidates: 1, pool: []*models.Mock{rec}}
	r := newHTTP().buildHTTPMismatchReport(request, []byte(`{"a":1}`), &mockMemDb{mocks: []*models.Mock{rec}}, nil, nil, nil, diag)
	if !slices.ContainsFunc(r.FieldDiffs, func(d models.MockFieldDiff) bool {
		return d.Path == "header.Authorization" && d.Kind == models.DiffKindMissingInMock
	}) {
		t.Errorf("field diffs %+v; want header.Authorization only the call carries among them", r.FieldDiffs)
	}
	const structural = "Request structure changed since recording. Re-record the test set with 'keploy record', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping."
	if r.NextSteps != structural {
		t.Errorf("hint:\n got  %q\n want %q", r.NextSteps, structural)
	}
}

// No Authorization, of any scheme, is offered as keploy.yml noise, though
// declaring it would serve the call: keploy.yml's header noise, global or a
// test set's, applies to every recording it reaches, so declaring
// authorization would also stop, under the switch, the part comparison of
// each recording there whose Authorization is SigV4, and nothing the hint can
// read says that none is (mismatch.authorizationHeader). A pool of Bearer
// recordings only is what the hint sees when the signed recording is outside
// it (one the call's own test already answered, another test's outside the
// call's window, another test set's), so it is offered none either. Nor is
// an x-amz-content-sha256. Each row's pool is the call's recording (POST
// /x), then the other, if any (POST /s3, signed, or POST /y, Bearer).
func TestTheHintOffersNoAuthorizationNoise(t *testing.T) {
	sigV4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + strings.Repeat("a", 64)
	const json = `{"a":1}`
	const rerecord = "re-record with 'keploy record'"
	const structural = "Request structure changed since recording. Re-record the test set with 'keploy record', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping."
	payloadHash := sha256.Sum256([]byte(json))
	hdr := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	signed := func() *models.Mock { return recorded("s3", "/s3", hdr("Authorization", sigV4), json) }
	bearer := func() *models.Mock { return recorded("o", "/y", hdr("Authorization", "Bearer o"), json) }
	drifted := "Only values drifted (header.authorization). If the change is expected, " + rerecord + "."
	for _, c := range []struct {
		name            string
		on, noAuto      bool
		other           func() *models.Mock // nil: the call's recording is alone in its pool
		recHdr, liveHdr map[string]string
		diff            models.MockFieldDiff // among the report's field diffs
		hint            string
		noise           string // the header whose noise, declared, serves the call
	}{
		{name: "a Bearer Authorization drifted, switch on", on: true,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr("Authorization", "Bearer u"),
			diff: models.MockFieldDiff{Path: "header.authorization", Kind: models.DiffKindValueChanged}, hint: drifted, noise: "authorization"},
		{name: "a recorded Bearer Authorization missing, switch on", on: true,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(),
			diff: models.MockFieldDiff{Path: "header.authorization", Kind: models.DiffKindMissingInLive}, hint: structural, noise: "authorization"},
		{name: "a recorded Bearer Authorization missing, switch off, keploy's header noise disabled", noAuto: true,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(),
			diff: models.MockFieldDiff{Path: "header.Authorization", Kind: models.DiffKindMissingInLive}, hint: structural, noise: "authorization"},
		{name: "a Bearer Authorization only the call carries, switch on", on: true,
			recHdr: hdr(), liveHdr: hdr("Authorization", "Bearer u"),
			diff: models.MockFieldDiff{Path: "header.authorization", Kind: models.DiffKindMissingInMock}, hint: structural, noise: "authorization"},
		{name: "a Bearer Authorization drifted beside another Bearer recording, switch on", on: true, other: bearer,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr("Authorization", "Bearer u"),
			diff: models.MockFieldDiff{Path: "header.authorization", Kind: models.DiffKindValueChanged}, hint: drifted, noise: "authorization"},
		{name: "a recorded Bearer Authorization missing beside another Bearer recording, switch off, keploy's header noise disabled", noAuto: true, other: bearer,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(),
			diff: models.MockFieldDiff{Path: "header.Authorization", Kind: models.DiffKindMissingInLive}, hint: structural, noise: "authorization"},
		{name: "a Bearer Authorization drifted beside a signed recording, switch on", on: true, other: signed,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr("Authorization", "Bearer u"),
			diff: models.MockFieldDiff{Path: "header.authorization", Kind: models.DiffKindValueChanged}, hint: drifted, noise: "authorization"},
		{name: "a recorded Bearer Authorization missing beside a signed recording, switch on", on: true, other: signed,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(),
			diff: models.MockFieldDiff{Path: "header.authorization", Kind: models.DiffKindMissingInLive}, hint: structural, noise: "authorization"},
		{name: "a recorded Bearer Authorization missing beside a signed recording, switch off, keploy's header noise disabled", noAuto: true, other: signed,
			recHdr: hdr("Authorization", "Bearer t"), liveHdr: hdr(),
			diff: models.MockFieldDiff{Path: "header.Authorization", Kind: models.DiffKindMissingInLive}, hint: structural, noise: "authorization"},
		{name: "an x-amz-content-sha256 that is no hash of the body the call sent beside a signed recording, switch on", on: true, other: signed,
			recHdr: hdr("X-Amz-Content-Sha256", hex.EncodeToString(payloadHash[:])), liveHdr: hdr("X-Amz-Content-Sha256", strings.Repeat("0", 64)),
			diff: models.MockFieldDiff{Path: "header.x-amz-content-sha256", Kind: models.DiffKindValueChanged},
			hint: "Only values drifted (header.x-amz-content-sha256). If the change is expected, " + rerecord + ".", noise: "x-amz-content-sha256"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := func() []*models.Mock {
				p := []*models.Mock{recorded("m", "/x", c.recHdr, json)}
				if c.other != nil {
					p = append(p, c.other())
				}
				return p
			}
			served, r := missIn(t, c.on, c.noAuto, nil, pool(), c.liveHdr, json)
			if served {
				t.Fatal("served; want a miss")
			}
			if !slices.ContainsFunc(r.FieldDiffs, func(d models.MockFieldDiff) bool { return d.Path == c.diff.Path && d.Kind == c.diff.Kind }) {
				t.Errorf("field diffs %+v; want %+v among them", r.FieldDiffs, c.diff)
			}
			if r.NextSteps != c.hint {
				t.Errorf("hint:\n got  %q\n want %q", r.NextSteps, c.hint)
			}
			// The noise is in reach: the hint withholds it.
			declared := map[string]map[string][]string{"header": {c.noise: {}}}
			if served, r := missIn(t, c.on, c.noAuto, declared, pool(), c.liveHdr, json); !served {
				t.Errorf("declaring %v leaves a miss: %+v", declared, r.FieldDiffs)
			}
		})
	}
}

// The fields a strict miss names as keploy.yml's patterns refused are those
// a pattern refused, each with its entry, its patterns and the shapes of
// each side's values outside them; not a refused read of the application's
// clock, which has a reason too. The reason names the live side's first
// value outside them, else the recorded side's.
func TestTheHintsNoiseFactsOfAStrictMiss(t *testing.T) {
	both := field(models.MockFieldDiff{Path: "header.x-id", Kind: models.DiffKindValueChanged})
	(&patternRefusal{patterns: []string{`^a-\d$`}, live: []string{"a string of 4 bytes"}, recorded: []string{"a string of 6 bytes"}}).refuse(&both, "header.x-id")
	recordedOnly := field(models.MockFieldDiff{Path: "body.order.items", Kind: models.DiffKindValueChanged})
	(&patternRefusal{entry: "items", patterns: []string{`^1$`}, recorded: []string{"an array"}, container: true}).refuse(&recordedOnly, "body.order.items")
	fields := []strictField{
		both,
		{diff: models.MockFieldDiff{Path: "body.since"}, why: "body.since was learned as a read of the application's clock", clock: true},
		{diff: models.MockFieldDiff{Path: "body.qty"}},
		recordedOnly,
	}
	want := []mismatch.PatternMiss{
		{Path: "header.x-id", Patterns: []string{`^a-\d$`}, Live: []string{"a string of 4 bytes"}, Recorded: []string{"a string of 6 bytes"}},
		{Path: "body.order.items", Entry: "items", Patterns: []string{`^1$`}, Recorded: []string{"an array"}, Container: true},
	}
	if got := patternMisses(fields); !reflect.DeepEqual(got, want) {
		t.Errorf("pattern misses %+v; want %+v", got, want)
	}
	for _, c := range []struct {
		f    strictField
		want string
	}{
		{both, "keploy.yml's noise lets header.x-id vary only within \"^a-\\\\d$\"; the live value (a string of 4 bytes) is outside it, so it is compared"},
		{recordedOnly, "keploy.yml's noise lets body.order.items vary only within \"^1$\"; the recorded value (an array) is outside it, so it is compared"},
	} {
		if c.f.why != c.want {
			t.Errorf("reason %q; want %q", c.f.why, c.want)
		}
	}
}

// A form or multipart key no keploy.yml entry names is never excused by it,
// whatever its values, and no pattern is said to refuse it.
func TestAFieldNoEntryNamesIsNeverCovered(t *testing.T) {
	k := strictKnown{n: strictNoise{body: matcher.NewNoiseLookup(map[string][]string{"code": {`^c\d$`}})}}
	for _, c := range []struct {
		name      string
		rec, live []string
	}{{"no values", nil, nil}, {"values", []string{"a"}, []string{"b"}}} {
		if covered, refusal := k.fieldCovers("other", c.rec, c.live); covered || refusal != nil {
			t.Errorf("%s: covered %v, refusal %+v; want neither", c.name, covered, refusal)
		}
	}
}
