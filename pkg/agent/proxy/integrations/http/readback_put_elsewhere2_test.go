package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A call to another URL of the key store sent, in a header the comparison
// excuses (a correlation id), the recorded account: it did not send this
// run's in its place, so the GET answers the account as recorded.
func TestReadBackOfAPutStopsAtAnExcusedHeaderThatHoldsTheValue(t *testing.T) {
	want := kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano)
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			trace := map[string]string{"X-Correlation-Id": "owner-" + kpRecBy, "X-Vault-Request": "true"}
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", "/v1/sys/audit/hold", "", trace, `{}`)
			if got := kpBetween(t, proven, nil, rec, live("DELETE", "/v1/sys/audit/hold", "", trace), false); got != want {
				t.Fatalf("a GET after a DELETE whose excused header held the recorded account answered\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// The recording of a call to another URL of the key store held the account
// in a part keploy read nothing of in this run: an excused header (trace
// baggage) the call did not send this time. keploy cannot tell what the
// call wrote, so the GET answers the account as recorded; the name's word,
// which the call did not hold, as this run's PUT stored it.
func TestReadBackOfAPutStopsAtAPartTheCallDidNotSend(t *testing.T) {
	want := kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano)
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			bag := map[string]string{"Baggage": "owner=" + kpRecBy, "X-Vault-Request": "true"}
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", "/v1/sys/audit/hold", "", bag, `{}`)
			if got := kpBetween(t, proven, nil, rec, live("DELETE", "/v1/sys/audit/hold", "", kpVault), false); got != want {
				t.Fatalf("a GET after a DELETE recorded with the account in a header it did not send answered\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// sentAsWritten reads the parts of a call's recorded request that its
// exchange does not hold: with no exchange (none served from the call's
// mock), a call whose recorded request holds the value is not known to have
// sent the PUT's value; with an exchange that sent it there, it is.
func TestSentAsWrittenReadsThePartsNoExchangeHolds(t *testing.T) {
	c := learnRecorded("mock-2", 0, "DELETE", kpOwners(kpRecBy), "", kpVault, `{}`)
	if sentAsWritten(learn.Earlier{}, c, kpRecBy, kpLiveBy) {
		t.Fatal("a call with no exchange whose recorded path holds the value sent the PUT's value")
	}
	seg := learn.Leaf{Path: "url.path.3", Noise: "url.path.3", Type: learn.String, Value: kpRecBy}
	e := learn.Earlier{Mock: "mock-2", Seen: []learn.Seen{{Side: learn.Request, Leaf: seg, Live: kpLiveBy}}}
	if !sentAsWritten(e, c, kpRecBy, kpLiveBy) {
		t.Fatal("a call whose exchange sent the PUT's value in the recorded value's place did not send it")
	}
	e.Seen[0].Live = kpRecBy
	if sentAsWritten(e, c, kpRecBy, kpLiveBy) {
		t.Fatal("a call whose exchange sent the recorded value sent the PUT's value")
	}
}

// An answer of a call to another URL is what the store said, not what the
// call wrote: the index PUT answered, as recorded, with a note that holds
// the recorded account inside a longer value leaves the read-back as it is.
func TestReadBackOfAPutHoldsOverAnAnswerElsewhereThatHoldsTheValue(t *testing.T) {
	body := func(by string) string { return `{"data":{"cid":"keploy.io","createdBy":"` + by + `"}}` }
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "PUT", kpIndex(kpRecBy), body(kpRecBy), kpVault, `{"data":{"note":"owner `+kpRecBy+`","version":1}}`)
			if got := kpBetween(t, proven, nil, rec, live("PUT", kpIndex(kpLiveBy), body(kpLiveBy), kpVault), false); got != kpLiveRecord {
				t.Fatalf("a GET after the index's PUT answered with a note that holds the recorded account answered\n%s\nwant what this run's PUT stored\n%s", got, kpLiveRecord)
			}
		})
	}
}

// keploy reads what a call to another URL sent in a path segment
// keploy.yml's url noise masks as the call sent it (the served exchange is
// read unmasked): a DELETE that sent this run's account there wrote what
// the PUT wrote, so the GET reads back; one that sent the recorded account
// there answers the account as recorded, as it does with no noise.
func TestReadBackOfAPutReadsAMaskedSegmentAsTheCallSentIt(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		for _, c := range []struct {
			name, sent, want string
		}{
			{"sent-live", kpLiveBy, kpLiveRecord},
			{"sent-as-recorded", kpRecBy, kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano)},
		} {
			t.Run("proven="+proven+"/"+c.name, func(t *testing.T) {
				m := kpMocks(kpGetAnswer(kpRecKey, kpRecBy, kpRecNano))
				rec := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", kpOwners(kpRecBy), "", kpVault, `{}`)
				calls := []*req{
					live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
					live("DELETE", kpOwners(c.sent), "", kpVault),
					live("GET", kpPath(kpRecKey), "", kpVault),
				}
				_, served := kpRunWith(t, proven, nil, []string{`[0-9a-f]{24}`}, []*models.Mock{m[0], rec, m[1]}, calls)
				if got := served[2].Spec.HTTPResp.Body; got != c.want {
					t.Fatalf("a GET after a DELETE that sent %s in a masked segment answered\n%s\nwant\n%s", c.sent, got, c.want)
				}
			})
		}
	}
}
