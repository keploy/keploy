package http

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func createOrder(id, item string) string {
	body := `{"id":"` + id + `","item":"` + item + `"}`
	return "POST /orders HTTP/1.1\r\nHost: orders.local\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

func getOrder(id string) string {
	return "GET /orders/" + id + " HTTP/1.1\r\nHost: orders.local\r\n\r\n"
}

// A test that mints its own order id each run: it POSTs /orders with a new
// id, then GETs /orders/<that id>. The recording has the run that made it:
// POST /orders for a pen and for a book, each with the id minted then, and
// GET /orders/<the book's id>. On replay every id is new, so no recording is
// any of these calls' own. With the proven switch off each call is still
// served a recording, and each says it was another call's: the GET is
// reported answered with the recording of GET /orders/<recorded id>
// (models.AnswerOtherID, at url.path.1), and each POST with the closest of the
// two POST recordings (models.AnswerClosestBody, at body.id). None of them is
// the call's own answer, so mock replay neither says the run missed nothing
// nor counts it as proven. Under the switch none of the calls is served
// another call's recording, and each is reported a miss.
func TestATestMintedIDIsNeverAnsweredAsTheCallsOwn(t *testing.T) {
	const (
		recPen  = "ord-41d6a0c2e95b7f13"
		recBook = "ord-8ab071aa4d5a130a"
		newPen  = "ord-5c9e2b7d1f04a368"
		newBook = "ord-dcce109751713323"
	)
	recordings := func(t *testing.T, h *HTTP) *mockMemDb {
		return &mockMemDb{mocks: []*models.Mock{
			recordedAs(t, h, "mock-create-pen", 1, createOrder(recPen, "pen"), jsonAnswer(201, `{"id":"`+recPen+`","item":"pen"}`), nil),
			recordedAs(t, h, "mock-create-book", 2, createOrder(recBook, "book"), jsonAnswer(201, `{"id":"`+recBook+`","item":"book"}`), nil),
			recordedAs(t, h, "mock-get-book", 3, getOrder(recBook), jsonAnswer(200, `{"id":"`+recBook+`","item":"book"}`), nil),
		}, updateUnFilteredReturn: true}
	}
	t.Run("off", func(t *testing.T) {
		setProven(t, false)
		h := newHTTP()
		db := recordings(t, h)
		for _, c := range []struct {
			name, raw, mock string
		}{
			{"POST /orders for a pen", createOrder(newPen, "pen"), "mock-create-pen"},
			{"POST /orders for a book", createOrder(newBook, "book"), "mock-create-book"},
		} {
			got := serveReported(t, h, db, c.raw)
			if got.status != 201 {
				t.Fatalf("%s got %d %q, want the closest POST recording served", c.name, got.status, got.body)
			}
			a := got.answers()
			if len(a) != 1 || a[0].Answer != models.AnswerClosestBody || a[0].ClosestMock != c.mock || !slices.Equal(a[0].Differs, []string{"body.id"}) {
				t.Fatalf("%s's answer reports are %+v, want one %q with %s at body.id", c.name, a, models.AnswerClosestBody, c.mock)
			}
			if a[0].CountsAgainstTest() || a[0].NextSteps == "" {
				t.Errorf("%s's report counts against its test (%v) or has no next step (%q)", c.name, a[0].CountsAgainstTest(), a[0].NextSteps)
			}
		}
		got := serveReported(t, h, db, getOrder(newBook))
		if got.status != 200 || !strings.Contains(got.body, recBook) {
			t.Fatalf("GET /orders/%s got %d %q, want the recorded order's answer served", newBook, got.status, got.body)
		}
		a := got.answers()
		if len(a) != 1 || a[0].Answer != models.AnswerOtherID || a[0].ClosestMock != "mock-get-book" || !slices.Equal(a[0].Differs, []string{"url.path.1"}) {
			t.Fatalf("GET /orders/%s's answer reports are %+v, want one %q with mock-get-book at url.path.1", newBook, a, models.AnswerOtherID)
		}
		if want := "GET /orders/" + newBook + " was answered with the recording of GET /orders/" + recBook; !strings.Contains(a[0].Diff, want) {
			t.Errorf("the report says %q, want it to say %q", a[0].Diff, want)
		}
	})
	t.Run("on", func(t *testing.T) {
		setProven(t, true)
		h := newHTTP()
		db := recordings(t, h)
		for _, raw := range []string{createOrder(newPen, "pen"), createOrder(newBook, "book"), getOrder(newBook)} {
			call, _, _ := strings.Cut(raw, " HTTP/1.1")
			got := serveReported(t, h, db, raw)
			if strings.Contains(got.body, recBook) || strings.Contains(got.body, recPen) {
				t.Errorf("%s was served another call's recording under the switch: %d %q", call, got.status, got.body)
			}
			for _, r := range got.answers() {
				t.Errorf("%s was reported answered (%q with %q) under the switch", call, r.Answer, r.ClosestMock)
			}
			if len(got.reports) == 0 {
				t.Errorf("%s got %d %q and no report, want it reported a miss", call, got.status, got.body)
			}
		}
	})
}
