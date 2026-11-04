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
	// many is n values, each of(i).
	many := func(n int, of func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = of(i)
		}
		return out
	}
	// jsonIDs is a JSON body whose array ids holds values, and form one
	// whose key code does.
	jsonIDs := func(values []string) string { return `{"ids":[` + strings.Join(values, ",") + `]}` }
	form := func(values []string) string { return "code=" + strings.Join(values, "&code=") }
	// 3000 numbers from 10, 37 apart: 3 of 2 characters, 24 of 3 and 243
	// of 4 before the rest, of 5 and 6; and 3000 form values z0 to z2999:
	// 10 of 2 bytes, 90 of 3 and 900 of 4 before 2000 of 5.
	liveIDs := many(3000, func(i int) string { return strconv.Itoa(10 + 37*i) })
	liveCodes := many(3000, func(i int) string { return "z" + strconv.Itoa(i) })
	digits := many(3000, func(i int) string { return strconv.Itoa(i % 10) })
	// 3000 recorded strings whose lengths cycle 2 to 51, and live ones of
	// 2 to 61, each 50 times in a row: no pattern matches either side's,
	// and each element is a pair of lengths of its own.
	recLengths := many(3000, func(i int) string { return strconv.Quote(strings.Repeat("r", i%50+2)) })
	liveLengths := many(3000, func(i int) string { return strconv.Quote(strings.Repeat("l", (i/50)%60+2)) })
	unquoted := func(values []string) []string {
		out := make([]string, len(values))
		for i, v := range values {
			out[i], _ = strconv.Unquote(v)
		}
		return out
	}
	bothLengths := []models.FieldReason{{Field: "body.ids[]", Why: "keploy.yml's noise lets body.ids[] vary only within \"^\\\\d$\"; the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2850 more) and the recorded values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2820 more) are outside it, so it is compared"}}
	codes := many(3000, func(i int) string { return "c" + strconv.Itoa(i%10) })
	// camel is keploy.yml's request-body noise as keploy reads it: viper
	// lowercases its keys, so an entry written requestId arrives requestid.
	camel := func(key, pattern string) map[string]map[string][]string {
		return map[string]map[string][]string{"requestbody": {strings.ToLower(key): {pattern}}}
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
		// maxHint, when set, bounds the hint's bytes: however many values
		// miss, it names a few shapes a side and counts the rest.
		maxHint int
		// reasons, when set, are the miss's reasons, each field's once;
		// maxNext bounds the bytes of the next step the run's summary
		// shows (UnmatchedCall.SanitizedNextStep), which adds them.
		reasons []models.FieldReason
		maxNext int
		// oneDiff: the report carries one field diff, however many places
		// of the field differ.
		oneDiff bool
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
			hint:   "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the live values (a number of 2 characters, a number of 3 characters); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"ids": {`^\d$`, `^\d\d$`, `^\d\d\d$`}}},
			values: map[string]map[string][]string{"ids": {"live": {"22", "333"}}}},
		{name: "recorded array elements an entry names by their array, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"ids": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"ids":[11,2,333]}`, live: `{"ids":[1,2,3]}`,
			diffs:  []models.MockFieldDiff{{Path: "body.ids[]", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the recorded values (a number of 2 characters, a number of 3 characters); otherwise " + rerecord + ".",
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
		// The hint names the entry by the run of fields of the path it
		// matches, in the API's case: keploy.yml's keys arrive lowercased,
		// and a hint naming requestid names a key keploy.yml does not hold.
		{name: "a camelCase body field keploy.yml guards with patterns, switch off", cfg: camel("requestId", `^r-\d$`),
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"requestId":"r-1"}`, live: `{"requestId":"evil"}`,
			diffs:  []models.MockFieldDiff{{Path: "body.requestId", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.requestId). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for requestId (\"^r-\\\\d$\") so one matches the live value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"requestId": {`^r-\d$`, `^evil$`}}},
			values: map[string]map[string][]string{"requestId": {"live": {"evil"}}}},
		{name: "a camelCase body field keploy.yml guards with patterns, switch on", on: true, cfg: camel("requestId", `^r-\d$`),
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"requestId":"r-1"}`, live: `{"requestId":"evil"}`,
			diffs:  []models.MockFieldDiff{{Path: "body.requestId", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.requestId). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for requestId (\"^r-\\\\d$\") so one matches the live value (a string of 4 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"requestId": {`^r-\d$`, `^evil$`}}},
			values: map[string]map[string][]string{"requestId": {"live": {"evil"}}}},
		{name: "a camelCase form key keploy.yml guards with patterns, switch on", on: true, cfg: camel("authCode", `^a\d$`),
			recHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), liveHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), recBody: "authCode=a1", live: "authCode=zz9",
			diffs:  []models.MockFieldDiff{{Path: "body.authCode", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.authCode). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for authCode (\"^a\\\\d$\") so one matches the live value (a string of 3 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"authCode": {`^a\d$`, `^zz9$`}}},
			values: map[string]map[string][]string{"authCode": {"live": {"zz9"}}}},
		{name: "a nested camelCase body field an entry names by its last name, switch on", on: true, cfg: camel("itemQty", `^\d{3}$`),
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"order":{"itemQty":123}}`, live: `{"order":{"itemQty":1234}}`,
			diffs:  []models.MockFieldDiff{{Path: "body.order.itemQty", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.order.itemQty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for itemQty (\"^\\\\d{3}$\") so one matches the live value (a number of 4 characters); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"itemQty": {`^\d{3}$`, `^\d{4}$`}}},
			values: map[string]map[string][]string{"itemQty": {"live": {"1234"}}}},
		// However many values miss, the hint names each distinct shape of a
		// side once, the first three, and counts the values past them: it
		// stays small, and the remedy it names still serves.
		{name: "a guarded array with 3000 elements outside its patterns, switch off", cfg: map[string]map[string][]string{"requestbody": {"ids": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: jsonIDs(digits), live: jsonIDs(liveIDs),
			diffs:   []models.MockFieldDiff{{Path: "body.ids[]", Kind: models.DiffKindValueChanged}},
			hint:    "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the live values (a number of 2 characters, a number of 3 characters, a number of 4 characters and 2730 more); otherwise " + rerecord + ".",
			values:  map[string]map[string][]string{"ids": {"live": liveIDs}},
			maxHint: 600},
		{name: "a guarded form key with 3000 values outside its patterns, switch off", cfg: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`}}},
			recHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), liveHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), recBody: form(codes), live: form(liveCodes),
			diffs:   []models.MockFieldDiff{{Path: "body.code", Kind: models.DiffKindValueChanged}},
			hint:    "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so they match the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2000 more); otherwise " + rerecord + ".",
			values:  map[string]map[string][]string{"code": {"live": liveCodes}},
			maxHint: 600},
		{name: "a guarded form key with 3000 values outside its patterns, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"code": {`^c\d$`}}},
			recHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), liveHdr: hdr("Content-Type", "application/x-www-form-urlencoded"), recBody: form(codes), live: form(liveCodes),
			diffs:   []models.MockFieldDiff{{Path: "body.code", Kind: models.DiffKindValueChanged}},
			hint:    "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so they match the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2000 more); otherwise " + rerecord + ".",
			values:  map[string]map[string][]string{"code": {"live": liveCodes}},
			maxHint: 600},
		// Each side's values outside, 3000 elements of 60 and 50 lengths:
		// the field has one reason, of a few shapes a side, so the next
		// step the run's summary shows, which adds it, stays under 1200
		// bytes; before, each element's pair of lengths added one. With
		// the switch on, each element is a diff of the array's name: they
		// are one diff, so the hint offers the widen it offers with the
		// switch off; before, 25 of them filled the report and it offered
		// only recording again.
		{name: "a guarded array with 3000 elements outside its patterns on both sides, switch off", cfg: map[string]map[string][]string{"requestbody": {"ids": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: jsonIDs(recLengths), live: jsonIDs(liveLengths),
			diffs:   []models.MockFieldDiff{{Path: "body.ids[]", Kind: models.DiffKindValueChanged}},
			hint:    "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2850 more) and the recorded values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2820 more); otherwise " + rerecord + ".",
			values:  map[string]map[string][]string{"ids": {"live": unquoted(liveLengths), "recorded": unquoted(recLengths)}},
			maxHint: 600, reasons: bothLengths, maxNext: 1200, oneDiff: true},
		{name: "a guarded array with 3000 elements outside its patterns on both sides, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"ids": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: jsonIDs(recLengths), live: jsonIDs(liveLengths),
			diffs:   []models.MockFieldDiff{{Path: "body.ids[]", Kind: models.DiffKindValueChanged}},
			hint:    "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^\\\\d$\") so they match the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2850 more) and the recorded values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2820 more); otherwise " + rerecord + ".",
			values:  map[string]map[string][]string{"ids": {"live": unquoted(liveLengths), "recorded": unquoted(recLengths)}},
			maxHint: 600, reasons: bothLengths, maxNext: 1200, oneDiff: true},
		// keploy.yml's keys are read without case: one entry two fields
		// name in two cases is widened once, named by its first naming.
		{name: "one entry two fields name in two cases, switch off", cfg: map[string]map[string][]string{"requestbody": {"id": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"order":{"id":"1"},"user":{"ID":"2"}}`, live: `{"order":{"id":"12"},"user":{"ID":"345"}}`,
			diffs:  []models.MockFieldDiff{{Path: "body.order.id", Kind: models.DiffKindValueChanged}, {Path: "body.user.ID", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.order.id, body.user.ID). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for id (\"^\\\\d$\") so they match the live values (a string of 2 bytes, a string of 3 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"id": {`^\d$`, `^\d{2,3}$`}}},
			values: map[string]map[string][]string{"id": {"live": {"12", "345"}}}},
		{name: "one entry two fields name in two cases, the first in capitals, switch on", on: true, cfg: map[string]map[string][]string{"requestbody": {"id": {`^\d$`}}},
			recHdr: hdr("Content-Type", "application/json"), liveHdr: hdr("Content-Type", "application/json"), recBody: `{"order":{"ID":"1"},"user":{"id":"2"}}`, live: `{"order":{"ID":"12"},"user":{"id":"345"}}`,
			diffs:  []models.MockFieldDiff{{Path: "body.order.ID", Kind: models.DiffKindValueChanged}, {Path: "body.user.id", Kind: models.DiffKindValueChanged}},
			hint:   "Only values drifted (body.order.ID, body.user.id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ID (\"^\\\\d$\") so they match the live values (a string of 2 bytes, a string of 3 bytes); otherwise " + rerecord + ".",
			fix:    map[string]map[string][]string{"requestbody": {"id": {`^\d$`, `^\d{2,3}$`}}},
			values: map[string]map[string][]string{"ID": {"live": {"12", "345"}}}},
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
			if c.oneDiff && len(r.FieldDiffs) != 1 {
				t.Errorf("the report carries %d field diffs; want one", len(r.FieldDiffs))
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
			if c.maxHint > 0 && len(r.NextSteps) > c.maxHint {
				t.Errorf("the hint is %d bytes; want at most %d", len(r.NextSteps), c.maxHint)
			}
			if c.reasons != nil && !reflect.DeepEqual(r.FieldReasons, c.reasons) {
				t.Errorf("reasons:\n got  %q\n want %q", r.FieldReasons, c.reasons)
			}
			if next := (models.UnmatchedCall{Protocol: "HTTP", NextSteps: r.NextSteps, FieldDiffs: r.FieldDiffs, FieldReasons: r.FieldReasons}).SanitizedNextStep(); c.maxNext > 0 && len(next) > c.maxNext {
				t.Errorf("the run summary's next step is %d bytes; want at most %d: %q", len(next), c.maxNext, next)
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
// of that side's values (values: entry, side, values). A keploy.yml edit
// the hint names that remedyOf cannot read fails the row, so a hint never
// passes the rule by offering noise in words the rule does not replay: any
// mention of keploy.yml but the edits it reads and the reason a container
// gets no noise, and an entry to widen joined to the one before it by any
// words but " and for ".
func remedyOf(t testing.TB, hint string, cfg map[string]map[string][]string, values map[string]map[string][]string) (map[string]map[string][]string, bool) {
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
	// unread is hint less each part of it remedyOf reads.
	unread := []byte(hint)
	read := func(from, to int) {
		for i := from; i < to; i++ {
			unread[i] = ' '
		}
	}
	offered := false
	// Only the fields to add are given by example, under the bucket the
	// sentence names.
	add := regexp.MustCompile(`(?:add|and) the [^()]*? under test\.globalNoise\.global\.(\w+)(?: with root-relative keys)? \(e\.g\. (\w+): \{([^}]*)\}\)`)
	for _, m := range add.FindAllStringSubmatchIndex(hint, -1) {
		bucket, example := hint[m[2]:m[3]], hint[m[4]:m[5]]
		if bucket != example {
			t.Fatalf("hint %q adds under test.globalNoise.global.%s by an example of %s", hint, bucket, example)
		}
		for _, key := range strings.Split(hint[m[6]:m[7]], ", ") {
			set(bucket, strings.TrimSuffix(key, ": []"), []string{})
			offered = true
		}
		read(m[0], m[1])
	}
	widen := regexp.MustCompile(`widen the patterns of keploy\.yml's test\.globalNoise (\w+) entries for `)
	item := regexp.MustCompile(`^(\S+) \(((?:"(?:[^"\\]|\\.)*"(?:, )?)+)\) so (?:one matches|they match) (the (?:live|recorded) values? \([^)]*\)(?: and the (?:live|recorded) values? \([^)]*\))?)`)
	quoted := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	starts := widen.FindAllStringSubmatchIndex(hint, -1)
	for i, at := range starts {
		end := len(hint)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		bucket := hint[at[2]:at[3]]
		read(at[0], at[1])
		// The entries, joined by " and for ", from the sentence's start,
		// up to the sentence's end or the next remedy's " and ".
		for pos, first := at[1], true; ; first = false {
			if !first {
				if rest := hint[pos:end]; !strings.HasPrefix(rest, " and for ") {
					if !strings.HasPrefix(rest, ";") && !strings.HasPrefix(rest, ".") && (rest != " and " || i+1 == len(starts)) {
						t.Fatalf("hint %q joins an entry to widen by words remedyOf does not read, at %q", hint, rest)
					}
					break
				}
				read(pos, pos+len(" and for "))
				pos += len(" and for ")
			}
			it := item.FindStringSubmatch(hint[pos:end])
			if it == nil {
				t.Fatalf("hint %q says to widen patterns it does not name, at %q", hint, hint[pos:end])
			}
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
					if p := "^" + regexp.QuoteMeta(v) + "$"; !slices.Contains(pats, p) {
						pats = append(pats, p)
					}
				}
			}
			set(bucket, key, pats)
			offered = true
			read(pos, pos+len(it[0]))
			pos += len(it[0])
		}
	}
	// Why a container gets no noise names keploy.yml's patterns, and no
	// edit.
	held := `\S+ vary only within (?:"(?:[^"\\]|\\.)*"(?:, )?)+`
	for _, m := range regexp.MustCompile(`keploy\.yml's noise lets `+held+`(?: and `+held+`)*, and a pattern matches a single value, never an array or an object\.`).FindAllIndex(unread, -1) {
		read(m[0], m[1])
	}
	if loc := regexp.MustCompile(`(?i)\bwiden\b|\badd\b|test\.globalNoise|keploy\.yml`).FindIndex(unread); loc != nil {
		t.Fatalf("hint %q names a keploy.yml edit remedyOf does not read, at %q", hint, hint[loc[0]:])
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
// clock, which has a reason too. The reason names each side's values
// outside them, by their shapes, a value or many.
func TestTheHintsNoiseFactsOfAStrictMiss(t *testing.T) {
	both := field(models.MockFieldDiff{Path: "header.x-id", Kind: models.DiffKindValueChanged})
	(&patternRefusal{patterns: []string{`^a-\d$`}, live: mismatch.ShapesOf("a string of 4 bytes"), recorded: mismatch.ShapesOf("a string of 6 bytes")}).refuse(&both, "header.x-id")
	// A key that may be a value: the reason names the field by its shape,
	// never by its path.
	liveOnly := shaped("body.k1x9.qty", "body.{}.qty", models.DiffKindValueChanged, "1", "22")
	(&patternRefusal{patterns: []string{"^1$"}, live: mismatch.ShapesOf("a number of 2 characters", "a number of 3 characters")}).refuse(&liveOnly, liveOnly.shape)
	recordedOnly := field(models.MockFieldDiff{Path: "body.order.items", Kind: models.DiffKindValueChanged})
	(&patternRefusal{entry: "items", patterns: []string{`^1$`}, recorded: mismatch.ShapesOf("an array"), container: true}).refuse(&recordedOnly, "body.order.items")
	fields := []strictField{
		both,
		{diff: models.MockFieldDiff{Path: "body.since"}, why: "body.since was learned as a read of the application's clock", clock: true},
		{diff: models.MockFieldDiff{Path: "body.qty"}},
		recordedOnly,
	}
	want := []mismatch.PatternMiss{
		{Path: "header.x-id", Patterns: []string{`^a-\d$`}, Live: mismatch.ShapesOf("a string of 4 bytes"), Recorded: mismatch.ShapesOf("a string of 6 bytes")},
		{Path: "body.order.items", Entry: "items", Patterns: []string{`^1$`}, Recorded: mismatch.ShapesOf("an array"), Container: true},
	}
	if got := patternMisses(fields); !reflect.DeepEqual(got, want) {
		t.Errorf("pattern misses %+v; want %+v", got, want)
	}
	for _, c := range []struct {
		f    strictField
		want string
	}{
		{both, "keploy.yml's noise lets header.x-id vary only within \"^a-\\\\d$\"; the live value (a string of 4 bytes) and the recorded value (a string of 6 bytes) are outside it, so it is compared"},
		{liveOnly, "keploy.yml's noise lets body.{}.qty vary only within \"^1$\"; the live values (a number of 2 characters, a number of 3 characters) are outside it, so it is compared"},
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

// fatalTB is a testing.TB whose Fatalf records the failure and stops the
// helper it fails, for a row that a helper fails.
type fatalTB struct {
	testing.TB
	failed string
}

type fatalStop struct{}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.failed = fmt.Sprintf(format, args...)
	panic(fatalStop{})
}

// The rule's replay reads every keploy.yml edit a hint names: a hint that
// names one in words it does not read fails the row, so no row passes the
// rule by a remedy it never replays.
func TestRemedyOfFailsOnAnEditItDoesNotRead(t *testing.T) {
	cfg := map[string]map[string][]string{"requestbody": {"a": {`^x$`}, "b": {`^y$`}}}
	values := map[string]map[string][]string{"a": {"live": {"xx"}}, "b": {"live": {"yy"}}}
	const widen = "widen the patterns of keploy.yml's test.globalNoise requestbody entries for "
	try := func(hint string) (remedy map[string]map[string][]string, offered bool, failed string) {
		f := &fatalTB{TB: t}
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(fatalStop); !ok {
					panic(r)
				}
			}
			failed = f.failed
		}()
		remedy, offered = remedyOf(f, hint, cfg, values)
		return remedy, offered, f.failed
	}
	for _, c := range []struct{ name, hint string }{
		{"an edit in other words", "Only values drifted (body.c). If these are dynamic, mark body.c as noise in keploy.yml; otherwise re-record."},
		{"a second entry joined by a comma", "Only values drifted (body.a, body.b). If these are dynamic, " + widen + `a ("^x$") so one matches the live value (a string of 2 bytes), b ("^y$") so one matches the live value (a string of 2 bytes); otherwise re-record.`},
		{"a second entry joined by another word", "Only values drifted (body.a, body.b). If these are dynamic, " + widen + `a ("^x$") so one matches the live value (a string of 2 bytes) plus b ("^y$") so one matches the live value (a string of 2 bytes); otherwise re-record.`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, failed := try(c.hint); failed == "" {
				t.Errorf("remedyOf read %q; want it to fail the row", c.hint)
			}
		})
	}
	// What it reads: two entries joined by " and for ", and why a
	// container gets no noise.
	two := "Only values drifted (body.a, body.b). If these are dynamic, " + widen + `a ("^x$") so one matches the live value (a string of 2 bytes) and for b ("^y$") so one matches the live value (a string of 2 bytes); otherwise re-record.`
	if remedy, offered, failed := try(two); failed != "" || !offered || !slices.Equal(remedy["requestbody"]["a"], []string{`^x$`, `^xx$`}) || !slices.Equal(remedy["requestbody"]["b"], []string{`^y$`, `^yy$`}) {
		t.Errorf("remedyOf(%q) = %v, %v, failed %q; want both entries widened", two, remedy, offered, failed)
	}
	container := `Only values drifted (body.a). keploy.yml's noise lets body.a vary only within "^x$", and a pattern matches a single value, never an array or an object. If the change is expected, re-record.`
	if _, offered, failed := try(container); failed != "" || offered {
		t.Errorf("remedyOf(%q) offered %v, failed %q; want no remedy and no failure", container, offered, failed)
	}
}

// However many fields keploy.yml's patterns refuse, each has one reason,
// and the next step the run's summary shows (SanitizedNextStep), which adds
// them, names the first three and counts the rest: 60 fields stay under
// 2000 bytes, where every reason was added before.
func TestTheRunSummaryNamesAFewReasonsHoweverManyFieldsMiss(t *testing.T) {
	jsonCT := map[string]string{"Content-Type": "application/json"}
	cfg := map[string]map[string][]string{"requestbody": {"id": {`^\d$`}}}
	var rec, live []string
	for i := 0; i < 60; i++ {
		key := string(rune('a'+i/26)) + string(rune('a'+i%26))
		rec = append(rec, `"`+key+`":{"id":"1"}`)
		live = append(live, `"`+key+`":{"id":"22"}`)
	}
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch on %v", on), func(t *testing.T) {
			served, r := missWith(t, on, false, cfg, recorded("m", "/x", jsonCT, "{"+strings.Join(rec, ",")+"}"), jsonCT, "{"+strings.Join(live, ",")+"}")
			if served {
				t.Fatal("served; want a miss")
			}
			fields := map[string]bool{}
			for _, fr := range r.FieldReasons {
				fields[fr.Field] = true
			}
			if len(r.FieldReasons) != 60 || len(fields) != 60 {
				t.Fatalf("%d reasons of %d fields; want one for each of 60: %+v", len(r.FieldReasons), len(fields), r.FieldReasons)
			}
			next := models.UnmatchedCall{Protocol: "HTTP", NextSteps: r.NextSteps, FieldDiffs: r.FieldDiffs, FieldReasons: r.FieldReasons}.SanitizedNextStep()
			want := " (" + r.FieldReasons[0].Why + "; " + r.FieldReasons[1].Why + "; " + r.FieldReasons[2].Why + "; and 57 more reasons)"
			if !strings.HasSuffix(next, want) || len(next) > 2000 {
				t.Errorf("the run summary's next step is %d bytes: %q; want at most 2000, ending %q", len(next), next, want)
			}
		})
	}
}

// reasonsOf gives a field one reason: its refusals by one entry's patterns,
// at every place of it (an array's elements, the values under keys that may
// be values), folded into one that names the values of each;
// refusals of one shape by two entries' patterns (keys that may be values
// share a shape) two, each with its patterns; a reason of another kind
// once; and none to a field with no shape or no reason.
func TestReasonsOfGivesAFieldOneReason(t *testing.T) {
	refused := func(path, shape string, pats []string, live, recorded string) strictField {
		var l, r mismatch.ValueShapes
		if live != "" {
			l.Add(live)
		}
		if recorded != "" {
			r.Add(recorded)
		}
		f := strictField{diff: models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged}, shape: shape}
		(&patternRefusal{patterns: pats, live: l, recorded: r}).refuse(&f, shape)
		return f
	}
	digit, x := []string{`^\d$`}, []string{`^x$`}
	clock := strictField{diff: models.MockFieldDiff{Path: "body.since"}, shape: "body.since", why: "body.since was learned as a read of the application's clock", clock: true}
	fields := []strictField{
		refused("body.ids[]", "body.ids[]", digit, matcher.StringShape(2), ""),
		clock,
		refused("body.ids[]", "body.ids[]", digit, "", matcher.StringShape(3)),
		refused("body.k1x9.id", "body.{}.id", digit, matcher.StringShape(4), ""),
		refused("body.ids[]", "body.ids[]", digit, matcher.StringShape(5), ""),
		refused("body.q7z2.id", "body.{}.id", x, matcher.StringShape(6), ""),
		refused("body.m3p8.id", "body.{}.id", digit, "", matcher.StringShape(7)),
		clock,
		{diff: models.MockFieldDiff{Path: "body.n"}, shape: "body.n"},
		{diff: models.MockFieldDiff{Path: "body.m"}, why: "a reason of no field"},
	}
	want := []models.FieldReason{
		{Field: "body.ids[]", Why: matcher.PatternValuesWhy("body.ids[]", digit, "the live values (a string of 2 bytes, a string of 5 bytes) and the recorded value (a string of 3 bytes)", true)},
		{Field: "body.since", Why: clock.why},
		{Field: "body.{}.id", Why: matcher.PatternValuesWhy("body.{}.id", digit, "the live value (a string of 4 bytes) and the recorded value (a string of 7 bytes)", true)},
		{Field: "body.{}.id", Why: matcher.PatternWhy("body.{}.id", x, "live", matcher.StringShape(6))},
	}
	if got := reasonsOf(fields); !reflect.DeepEqual(got, want) {
		t.Errorf("reasons:\n got  %q\n want %q", got, want)
	}
	// Only a learned read of the clock is warned as one (fieldReason.clock):
	// a pattern's reason is not, nor a reason of another kind, which no
	// comparison gives today.
	other := strictField{diff: models.MockFieldDiff{Path: "body.o"}, shape: "body.o", why: "a reason of another kind"}
	var clocks []string
	for _, r := range fieldReasons(append(fields, other)) {
		if r.clock {
			clocks = append(clocks, r.Field)
		}
	}
	if !slices.Equal(clocks, []string{"body.since"}) {
		t.Errorf("reasons said as a clock read: %q; want body.since alone", clocks)
	}
	if got := reasonsOf(nil); got != nil {
		t.Errorf("reasons of no field %#v; want none", got)
	}
}
