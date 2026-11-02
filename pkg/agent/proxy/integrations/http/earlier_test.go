package http

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ownGateDb is roundGateDb whose per-test read shows the open test's own
// recordings, none of them answered, beside shared ones the open test does
// not own (integrations.TestOwner), and that holds a call to its round as
// itself. restageAt makes the lookup of a
// recording made elsewhere numbered so (from 1) say the replay staged
// another set, after showing every staged recording.
type ownGateDb struct {
	roundGateDb
	own, shared []*models.Mock
	restageAt   int
	lookups     *int
}

func (d ownGateDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return append(slices.Clone(d.own), d.shared...), nil
}

func (d ownGateDb) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return d.GetPerTestMocksInWindow()
}

func (d ownGateDb) OwnedByOpenTest(m *models.Mock) bool { return !slices.Contains(d.shared, m) }

func (d ownGateDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d ownGateDb) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	*d.lookups++
	if d.restageAt == 0 || *d.lookups != d.restageAt {
		return d.gateDb.FindStagedNotOwned(kind, call, match)
	}
	for _, m := range d.staged {
		match(m)
	}
	return "", true
}

// readFailsDb is ownGateDb whose per-test read fails: the open test's own
// recordings it has not answered are then unknown.
type readFailsDb struct{ ownGateDb }

func (d readFailsDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return nil, errors.New("per-test read failed")
}

// secretPut is a recording of a PUT of a secret with check-and-set cas, made
// at recordedAt, to host.
func secretPut(name, method, path, host string, cas int, recordedAt time.Time) *models.Mock {
	m := httpMockOnHost(name, method, path, host)
	m.Spec.HTTPReq.Header["Content-Type"] = "application/json"
	m.Spec.HTTPReq.Body = `{"options":{"cas":` + strconv.Itoa(cas) + `},"data":{"token":"t"}}`
	m.Spec.ReqTimestampMock = recordedAt
	return m
}

// Under exact serving, a call of the test's own that changed (the test
// recorded the same method and path to the same destination, and no
// recording is the call) names the latest recording another test made of
// that method and path before the test's own (Earlier): an earlier test's
// window, which the replay passed without the call. A secret's PUT whose
// check-and-set version moved because a skipped test's write of it is
// missing is such a call. The miss stays the test's own: its closest is not
// that recording, and it is not NotOwned. Nothing is named for a recording
// made after the test's own, of another method, path or destination, one
// keploy cannot place (no request time, its own or the test's), when the
// test recorded no such call, or when the call is that recording (NotOwned
// names it). The learn round of record names it alike.
func TestAChangedCallOfTheTestsOwnNamesTheEarlierTestsRecordingOfItsMethodAndPath(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "localhost:8200"
	const path = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/u1"
	t.Setenv(provenswitch.Env, "1")
	rec := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	own := secretPut("mock-own", "PUT", path, host, 2, rec)
	early := secretPut("mock-early", "PUT", path, host, 1, rec.Add(-time.Minute))
	earliest := secretPut("mock-earliest", "PUT", path, host, 0, rec.Add(-2*time.Minute))
	late := secretPut("mock-late", "PUT", path, host, 3, rec.Add(time.Minute))
	alone := earlierDb{mockMemDb: &mockMemDb{}}
	// db is the store of a test whose own recordings are own, none of them
	// answered, and that answered consumed, with staged other tests'.
	db := func(own, consumed []*models.Mock, staged ...*models.Mock) ownGateDb {
		return ownGateDb{roundGateDb: roundGateDb{gateDb{earlierDb: alone, staged: staged, consumed: consumed}}, own: own, lookups: new(int)}
	}
	put := func(cas int) string {
		body := `{"options":{"cas":` + strconv.Itoa(cas) + `},"data":{"token":"t"}}`
		return "PUT " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	for name, opts := range map[string]models.OutgoingOptions{"replay": {}, "learn": {Learn: true, LearnPool: true}} {
		t.Run(name, func(t *testing.T) {
			missOf := func(d ownGateDb, cas int) *models.MockMismatchReport {
				t.Helper()
				r := missReportOf(t, d, addr, put(cas), opts)
				if r.MissKind == models.MissExtraCall {
					t.Fatalf("the PUT is an extra call (recorded %d); want a changed call of the test's own", r.Recorded)
				}
				return r
			}
			miss := func(own []*models.Mock, cas int, staged ...*models.Mock) *models.MockMismatchReport {
				t.Helper()
				return missOf(db(own, nil, staged...), cas)
			}
			// The latest made before the test's own, in whichever order the
			// store shows them.
			for _, staged := range [][]*models.Mock{{late, early, earliest}, {earliest, early, late}} {
				if r := miss([]*models.Mock{own}, 5, staged...); r.Earlier != "mock-early" || r.NotOwned || r.ClosestMock == "mock-early" {
					t.Fatalf("a changed PUT names earlier %q (closest %q, not owned %v); want mock-early, the latest made before the test's own, and the miss its own", r.Earlier, r.ClosestMock, r.NotOwned)
				}
			}
			// The test's own recording already answered: the call is a second
			// PUT, changed, never one call more than recorded.
			if r := missOf(db(nil, []*models.Mock{own}, early), 5); r.Earlier != "mock-early" {
				t.Fatalf("a changed PUT after its own answered names earlier %q; want mock-early", r.Earlier)
			}
			// A shared recording the per-test read shows is no test's own:
			// made before the earlier test's, it is no bound.
			shared := db([]*models.Mock{own}, nil, early)
			shared.shared = []*models.Mock{secretPut("mock-shared", "PUT", path, host, 0, rec.Add(-3*time.Minute))}
			if r := missOf(shared, 5); r.Earlier != "mock-early" {
				t.Fatalf("beside a shared recording of the call, a changed PUT names earlier %q; want mock-early", r.Earlier)
			}
			for what, c := range map[string]struct {
				own    []*models.Mock
				staged []*models.Mock
			}{
				"only a later test's":                {[]*models.Mock{own}, []*models.Mock{late}},
				"a recording with no request":        {[]*models.Mock{own}, []*models.Mock{{Name: "mock-noreq", Kind: models.HTTP}}},
				"an earlier one with no time":        {[]*models.Mock{own}, []*models.Mock{secretPut("mock-untimed", "PUT", path, host, 1, time.Time{})}},
				"the test's own with no time":        {[]*models.Mock{secretPut("mock-own-untimed", "PUT", path, host, 2, time.Time{})}, []*models.Mock{early}},
				"no own recording of the call":       {[]*models.Mock{secretPut("mock-own-other", "PUT", path+"x", host, 2, rec)}, []*models.Mock{early}},
				"an earlier one of another method":   {[]*models.Mock{own}, []*models.Mock{secretPut("mock-post", "POST", path, host, 1, rec.Add(-time.Minute))}},
				"an earlier one of another path":     {[]*models.Mock{own}, []*models.Mock{secretPut("mock-path", "PUT", path+"x", host, 1, rec.Add(-time.Minute))}},
				"an earlier one to another host":     {[]*models.Mock{own}, []*models.Mock{secretPut("mock-host", "PUT", path, "localhost:8300", 1, rec.Add(-time.Minute))}},
				"own of another method, earlier PUT": {[]*models.Mock{secretPut("mock-own-post", "POST", path, host, 2, rec)}, []*models.Mock{early}},
			} {
				if r := miss(c.own, 5, c.staged...); r.Earlier != "" {
					t.Fatalf("with %s, a changed PUT names earlier %q; want none", what, r.Earlier)
				}
			}
			// The test recorded the call twice: the bound is the earliest
			// of its own recordings with a request time, whichever order
			// they are in.
			second := secretPut("mock-own-2", "PUT", path, host, 4, rec.Add(2*time.Minute))
			between := secretPut("mock-between", "PUT", path, host, 3, rec.Add(time.Minute))
			ownUntimed := secretPut("mock-own-untimed", "PUT", path, host, 2, time.Time{})
			for _, owns := range [][]*models.Mock{{second, own}, {own, ownUntimed, second}} {
				if r := miss(owns, 5, between); r.Earlier != "" {
					t.Fatalf("a recording made between the test's own two is named earlier %q; want none", r.Earlier)
				}
			}
			if r := miss([]*models.Mock{second, ownUntimed, own}, 5, early); r.Earlier != "mock-early" {
				t.Fatalf("beside an own recording with no request time, a changed PUT names earlier %q; want mock-early", r.Earlier)
			}
			// The replay staged another set while the earlier recording was
			// looked up (the miss's second lookup, after the one for a
			// recording the call is): none is named.
			restaged := db([]*models.Mock{own}, nil, early)
			restaged.restageAt = 2
			if r := missOf(restaged, 5); r.Earlier != "" || *restaged.lookups != 2 {
				t.Fatalf("with the set staged again under the lookup, a changed PUT names earlier %q after %d lookups; want none after 2", r.Earlier, *restaged.lookups)
			}
			// The call IS the earlier test's recording: it is named as that
			// recording made elsewhere, not as an earlier one.
			if r := miss([]*models.Mock{own}, 1, early); !r.NotOwned || r.ClosestMock != "mock-early" || r.Earlier != "" {
				t.Fatalf("a PUT that is mock-early is closest %q (not owned %v), earlier %q; want mock-early made elsewhere, no earlier", r.ClosestMock, r.NotOwned, r.Earlier)
			}
			// One call more than the test recorded is no changed call: it
			// names no earlier recording of its method and path.
			if r := missReportOf(t, db(nil, []*models.Mock{own}, early), addr, put(2), opts); r.MissKind != models.MissExtraCall || r.Earlier != "" {
				t.Fatalf("an extra PUT is kind %q, earlier %q; want an extra call naming no earlier recording", r.MissKind, r.Earlier)
			}
			// Without the switch nothing is named. (The test's own recording
			// carries a header the call lacks, so the matcher without the
			// switch, which this store cannot serve, has no candidate.)
			t.Setenv(provenswitch.Env, "")
			ownHdr := secretPut("mock-own-hdr", "PUT", path, host, 2, rec)
			ownHdr.Spec.HTTPReq.Header["X-Vault-Token"] = "t"
			if r := missReportOf(t, db([]*models.Mock{ownHdr}, nil, early), addr, put(5), opts); r.Earlier != "" {
				t.Fatalf("without the switch, a changed PUT names earlier %q; want none", r.Earlier)
			}
		})
	}
}

// When the per-test read fails, keploy cannot place the test's own
// recordings it has not answered, one of which may come before an earlier
// test's: ownHTTPRecordings gives none, not even the ones it answered, so
// earlierRecording names none. (The matcher reads the per-test pool first and
// fails the call when that read fails, so only a read that fails between the
// two reaches it.) Mutation: the failed read is ignored.
func TestNoOwnRecordingIsPlacedWhenThePerTestReadFails(t *testing.T) {
	own := secretPut("mock-own", "PUT", "/v1/secret/data/x", "localhost:8200", 2, time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	ok := ownGateDb{roundGateDb: roundGateDb{gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{own}}}, lookups: new(int)}
	if got := ownHTTPRecordings(ok); len(got) != 1 || got[0] != own {
		t.Fatalf("with the per-test read working, the test's own are %v; want its answered mock-own", got)
	}
	if got := ownHTTPRecordings(readFailsDb{ok}); got != nil {
		t.Fatalf("with the per-test read failing, the test's own are %v; want none", got)
	}
}
