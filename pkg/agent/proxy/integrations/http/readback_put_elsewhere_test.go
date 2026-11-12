package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// kpOwners is a URL of the key store, not the record's, that names the
// account by.
func kpOwners(by string) string { return "/v1/sys/owners/" + by }

// kpIndex is the URL of the index of the record the application keeps under
// its account (api-server's key store keeps one per account).
func kpIndex(by string) string {
	return "/v1/secret/data/keploy/personal-access-tokens/_user/keploy.io/" + by + "/" + kpRecKey
}

// kpBetween runs the PUT, then c (recorded as rec, at the given offset of
// the recording), then the GET of the record, and returns what the GET
// answered. before puts c before the PUT instead.
func kpBetween(t *testing.T, proven string, bodyNoise map[string][]string, rec *models.Mock, c *req, before bool) string {
	t.Helper()
	m := kpMocks(kpGetAnswer(kpRecKey, kpRecBy, kpRecNano))
	put := live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault)
	get := live("GET", kpPath(kpRecKey), "", kpVault)
	mocks, calls := []*models.Mock{m[0], rec, m[1]}, []*req{put, c, get}
	if before {
		mocks, calls = []*models.Mock{rec, m[0], m[1]}, []*req{c, put, get}
	}
	_, served := kpRunWith(t, proven, bodyNoise, nil, mocks, calls)
	return served[2].Spec.HTTPResp.Body
}

// After the PUT, a call to another URL of the key store whose request held
// the account's id as recorded may have written it into the record: a
// delete endpoint F sent as recorded, a POST to another URL that sent
// another id there (keploy.yml's noise lets it), and a POST whose body
// keploy reads whole (no JSON) that holds it. The GET then answers the
// account's id (createdBy, and its word of the email) as recorded, and the
// name's word, which no such call held, as this run's PUT stored it.
func TestReadBackOfAPutStopsWhereAnotherURLMayHaveWrittenIt(t *testing.T) {
	const other = "6af5aaaaaaaaaaaaaaaaaaaa"
	want := kpGetAnswer(kpRecKey, kpRecBy, kpLiveNano)
	owner := func(by string) string { return `{"owner":"` + by + `"}` }
	text := map[string]string{"Content-Type": "text/plain", "X-Vault-Request": "true"}
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven+"/a-delete-endpoint-sent-as-recorded", func(t *testing.T) {
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", kpOwners(kpRecBy), "", kpVault, `{}`)
			if got := kpBetween(t, proven, nil, rec, live("DELETE", kpOwners(kpRecBy), "", kpVault), false); got != want {
				t.Fatalf("a GET after a DELETE that named the recorded account answered\n%s\nwant\n%s", got, want)
			}
		})
		t.Run("proven="+proven+"/a-post-to-another-url-sent-another-value", func(t *testing.T) {
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "POST", "/v1/sys/owners/transfer", owner(kpRecBy), kpVault, `{}`)
			c := live("POST", "/v1/sys/owners/transfer", owner(other), kpVault)
			if got := kpBetween(t, proven, map[string][]string{"owner": {}}, rec, c, false); got != want {
				t.Fatalf("a GET after a POST to another URL that sent another account answered\n%s\nwant\n%s", got, want)
			}
		})
		t.Run("proven="+proven+"/a-text-body-sent-as-recorded", func(t *testing.T) {
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "POST", "/v1/sys/owners/reset", "owner "+kpRecBy, text, `{}`)
			if got := kpBetween(t, proven, nil, rec, live("POST", "/v1/sys/owners/reset", "owner "+kpRecBy, text), false); got != want {
				t.Fatalf("a GET after a POST whose text body held the recorded account answered\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// The read-back holds over the calls to other URLs that wrote what the PUT
// wrote, or wrote nothing: the index of the record PUT with this run's
// account (api-server's own flow, whole, inside the email, and answered
// with the recorded account), a read of
// another URL (a GET, a HEAD, an OPTIONS) and a call to another Host, each
// naming the recorded account, a call that named it before the PUT, and a
// PUT to the record's URL the store refused (it stored nothing).
func TestReadBackOfAPutHoldsOverCallsElsewhereThatKeepTheRecord(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		whole := func(by string) string { return `{"data":{"cid":"keploy.io","createdBy":"` + by + `"}}` }
		for _, c := range []struct {
			name, answer string
			body         func(by string) string
		}{
			{"whole", `{"data":{"version":1}}`, whole},
			{"email", `{"data":{"version":1}}`, func(by string) string { return `{"data":{"cid":"keploy.io","email":"sa-` + by + `@keploy.io.svc"}}` }},
			// An answer is what the store said, not what the call wrote.
			{"answered-with-the-recorded-account", `{"data":{"owner":"` + kpRecBy + `","version":1}}`, whole},
		} {
			body := c.body
			t.Run("proven="+proven+"/an-index-put-of-this-runs-account/"+c.name, func(t *testing.T) {
				rec := learnRecorded("mock-2", 1200*time.Millisecond, "PUT", kpIndex(kpRecBy), body(kpRecBy), kpVault, c.answer)
				if got := kpBetween(t, proven, nil, rec, live("PUT", kpIndex(kpLiveBy), body(kpLiveBy), kpVault), false); got != kpLiveRecord {
					t.Fatalf("a GET after the index's PUT answered\n%s\nwant what this run's PUT stored\n%s", got, kpLiveRecord)
				}
			})
		}
		for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
			t.Run("proven="+proven+"/a-read-of-another-url/"+method, func(t *testing.T) {
				rec := learnRecorded("mock-2", 1200*time.Millisecond, method, kpOwners(kpRecBy), "", kpVault, `{}`)
				if got := kpBetween(t, proven, nil, rec, live(method, kpOwners(kpRecBy), "", kpVault), false); got != kpLiveRecord {
					t.Fatalf("a GET after a %s of another URL answered\n%s\nwant what this run's PUT stored", method, got)
				}
			})
		}
		t.Run("proven="+proven+"/another-host", func(t *testing.T) {
			host := map[string]string{"Host": "other.test"}
			rec := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", kpOwners(kpRecBy), "", host, `{}`)
			if got := kpBetween(t, proven, nil, rec, live("DELETE", kpOwners(kpRecBy), "", host), false); got != kpLiveRecord {
				t.Fatalf("a GET after a DELETE to another Host answered\n%s\nwant what this run's PUT stored", got)
			}
		})
		t.Run("proven="+proven+"/before-the-put", func(t *testing.T) {
			rec := learnRecorded("mock-2", 1100*time.Millisecond, "DELETE", kpOwners(kpRecBy), "", kpVault, `{}`)
			if got := kpBetween(t, proven, nil, rec, live("DELETE", kpOwners(kpRecBy), "", kpVault), true); got != kpLiveRecord {
				t.Fatalf("a GET after a DELETE before the PUT answered\n%s\nwant what this run's PUT stored", got)
			}
		})
		t.Run("proven="+proven+"/a-refused-put-to-the-url", func(t *testing.T) {
			cas := `{"data":{"createdBy":"` + kpRecBy + `"},"options":{"cas":0}}`
			rec := kpStatus(learnRecorded("mock-2", 1200*time.Millisecond, "PUT", kpPath(kpRecKey), cas, kpVault, `{"errors":["check-and-set parameter did not match"]}`), 400)
			if got := kpBetween(t, proven, nil, rec, live("PUT", kpPath(kpRecKey), cas, kpVault), false); got != kpLiveRecord {
				t.Fatalf("a GET after a refused PUT that named the recorded account answered\n%s\nwant what the PUT before it stored", got)
			}
		})
	}
}

// putWrittenElsewhere passes over a call served that is no HTTP call (a
// Mongo one), and putAccess reads no answer leaf where nothing was stored:
// no call before the GET, or a DELETE of its URL.
func TestPutWrittenElsewhereReadsOnlyHTTPCalls(t *testing.T) {
	read := learnRecorded("mock-1", time.Second, "GET", "/k", "", nil, `{"a":"1"}`)
	write := learnRecorded("mock-0", 0, "PUT", "/k", `{"a":"1"}`, nil, `{}`)
	noPanic(t, "putWrittenElsewhere over a Mongo call", func() {
		if c, ok := putWrittenElsewhere(read, []*models.Mock{write, {Name: "mock-2", Kind: models.Mongo}}, nil, "mock-0", "1", "2"); ok {
			t.Fatalf("a Mongo call %s may have written the value", c)
		}
	})
	if a, ok := putAccess(read, nil, nil, learn.Shape{}); !ok || len(a.Read) != 0 {
		t.Fatalf("a GET with no call before it reads %v (%v), want no leaf", a.Read, ok)
	}
	del := learnRecorded("mock-0", 0, "DELETE", "/k", "", nil, `{}`)
	if a, ok := putAccess(read, []*models.Mock{del}, nil, learn.Shape{}); !ok || len(a.Read) != 0 || storedAny(a.Written) {
		t.Fatalf("a GET after a DELETE of its URL reads %v, stored %v (%v), want no leaf", a.Read, a.Written, ok)
	}
}
