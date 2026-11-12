package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// kpIDPath is the URL of the record's _id lookup, which api-server's key
// store PUTs right after the record (it holds the record's id, not the
// account).
const kpIDPath = "/v1/secret/data/keploy/personal-access-tokens/_id/" + kpRecKey

// api-server's key store PUTs the record, then its _id lookup and its
// _user index (the index holds the account in its path and its body), with
// one stamp for the three writes, and the token exchange then GETs the
// record. Every later call to another URL of the store is checked, not only
// the first: the two index writes wrote what the PUT wrote, so the GET
// reads back; a DELETE after them that sent the recorded account answers
// the account (createdBy, and its word of the email) as recorded, and the
// name's word, which the DELETE did not hold, as this run's PUT stored it.
// A later call that wrote what the PUT wrote does not undo an earlier
// one that did not: a DELETE before the _user index write holds the
// account as recorded too.
func TestReadBackOfAPutChecksEveryLaterCallElsewhere(t *testing.T) {
	const upd = "2026-11-12T01:24:51Z"
	hdr := map[string]string{"X-Vault-Request": "true", "X-Vault-Token": "dev-root-token", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}
	idBody := `{"data":{"cid":"keploy.io","updatedAt":"` + upd + `"}}`
	userBody := func(by string) string {
		return `{"data":{"cid":"keploy.io","createdBy":"` + by + `","updatedAt":"` + upd + `"}}`
	}
	ok := `{"data":{"version":1}}`
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven+"/the-two-index-writes", func(t *testing.T) {
			mocks := []*models.Mock{
				learnRecorded("mock-0", 1130*time.Millisecond, "PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpRecBy, kpRecNano), hdr, ok),
				learnRecorded("mock-2", 1150*time.Millisecond, "PUT", kpIDPath, idBody, hdr, ok),
				learnRecorded("mock-3", 1170*time.Millisecond, "PUT", kpIndex(kpRecBy), userBody(kpRecBy), hdr, ok),
				learnRecorded("mock-1", 1300*time.Millisecond, "GET", kpPath(kpRecKey), "", hdr, kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)),
			}
			_, served := kpRunWith(t, proven, nil, nil, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), hdr),
				live("PUT", kpIDPath, idBody, hdr),
				live("PUT", kpIndex(kpLiveBy), userBody(kpLiveBy), hdr),
				live("GET", kpPath(kpRecKey), "", hdr),
			})
			if got := served[3].Spec.HTTPResp.Body; got != kpLiveRecord {
				t.Fatalf("a GET after the record's two index writes answered\n%s\nwant what this run's PUT stored\n%s", got, kpLiveRecord)
			}
		})
		t.Run("proven="+proven+"/a-delete-after-the-two-index-writes", func(t *testing.T) {
			mocks := []*models.Mock{
				learnRecorded("mock-0", 1130*time.Millisecond, "PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpRecBy, kpRecNano), kpVault, ok),
				learnRecorded("mock-2", 1150*time.Millisecond, "PUT", kpIDPath, idBody, kpVault, ok),
				learnRecorded("mock-3", 1170*time.Millisecond, "PUT", kpIndex(kpRecBy), userBody(kpRecBy), kpVault, ok),
				learnRecorded("mock-4", 1200*time.Millisecond, "DELETE", kpOwners(kpRecBy), "", kpVault, `{}`),
				learnRecorded("mock-1", 1300*time.Millisecond, "GET", kpPath(kpRecKey), "", kpVault, kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)),
			}
			_, served := kpRunWith(t, proven, nil, nil, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("PUT", kpIDPath, idBody, kpVault),
				live("PUT", kpIndex(kpLiveBy), userBody(kpLiveBy), kpVault),
				live("DELETE", kpOwners(kpRecBy), "", kpVault),
				live("GET", kpPath(kpRecKey), "", kpVault),
			})
			if got, want := served[4].Spec.HTTPResp.Body, kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano); got != want {
				t.Fatalf("a GET after a DELETE, behind the two index writes, that named the recorded account answered\n%s\nwant\n%s", got, want)
			}
		})
		t.Run("proven="+proven+"/a-delete-before-the-user-index-write", func(t *testing.T) {
			mocks := []*models.Mock{
				learnRecorded("mock-0", 1130*time.Millisecond, "PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpRecBy, kpRecNano), kpVault, ok),
				learnRecorded("mock-4", 1150*time.Millisecond, "DELETE", kpOwners(kpRecBy), "", kpVault, `{}`),
				learnRecorded("mock-3", 1170*time.Millisecond, "PUT", kpIndex(kpRecBy), userBody(kpRecBy), kpVault, ok),
				learnRecorded("mock-1", 1300*time.Millisecond, "GET", kpPath(kpRecKey), "", kpVault, kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)),
			}
			_, served := kpRunWith(t, proven, nil, nil, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("DELETE", kpOwners(kpRecBy), "", kpVault),
				live("PUT", kpIndex(kpLiveBy), userBody(kpLiveBy), kpVault),
				live("GET", kpPath(kpRecKey), "", kpVault),
			})
			if got, want := served[3].Spec.HTTPResp.Body, kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano); got != want {
				t.Fatalf("a GET after a DELETE that named the recorded account, then the _user index write, answered\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// keploy reads nothing of a query parameter whose name begins with keploy
// (the learn step's shape skips it): a DELETE to another URL that held the
// account there, even sent with this run's account, is one keploy cannot
// tell what it wrote, so the GET answers the account as recorded.
func TestReadBackOfAPutStopsAtAKeployQueryParameterThatHoldsTheValue(t *testing.T) {
	want := kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano)
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", "/v1/sys/owners?keployOwner="+kpRecBy, "", kpVault, `{}`)
			c := live("DELETE", "/v1/sys/owners?keployOwner="+kpLiveBy, "", kpVault)
			if got := kpBetween(t, proven, nil, rec, c, false); got != want {
				t.Fatalf("a GET after a DELETE whose keploy query parameter held the account answered\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A leaf of a call to another URL that held the recorded account twice and
// sent this run's account in both places wrote what the PUT wrote: the GET
// reads back (keploy.yml's noise lets the leaf drift).
func TestReadBackOfAPutHoldsOverALeafThatSentTheValueTwice(t *testing.T) {
	body := func(by string) string { return `{"entry":"` + by + `/` + by + `"}` }
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "POST", "/v1/sys/audit", body(kpRecBy), kpVault, `{}`)
			c := live("POST", "/v1/sys/audit", body(kpLiveBy), kpVault)
			if got := kpBetween(t, proven, map[string][]string{"entry": {}}, rec, c, false); got != kpLiveRecord {
				t.Fatalf("a GET after a POST that sent this run's account twice in one leaf answered\n%s\nwant what this run's PUT stored\n%s", got, kpLiveRecord)
			}
		})
	}
}
