package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// stagedDb is a mockMemDb that stages more than its reads show
// (integrations.StagedFinder): staged are the recordings no read shows, as
// exact serving keeps another test's.
type stagedDb struct {
	*mockMemDb
	staged []*models.Mock
}

func (d stagedDb) FindStagedNotOwned(kind models.Kind, match func(*models.Mock) bool) string {
	for _, m := range d.staged {
		if m.Kind == kind && match(m) {
			return m.Name
		}
	}
	return ""
}

// A call no read answers that is a staged recording no read shows, by the
// strict comparison, is named as that recording; one that differs from every
// such recording is not, nor is any with a store that stages nothing more.
func TestNotOwnedRecordingIsTheCallMadeElsewhere(t *testing.T) {
	const host = "localhost:50001"
	elsewhere := onHost("other-test-delete", "/delete", host)
	db := stagedDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{onHost("other-test-get", "/get", host), elsewhere}}
	n := newHTTP().newStrictNoise(nil, nil, nil)
	ctx := context.Background()
	if got := notOwnedRecording(ctx, getTo("/delete", host), db, n); got != elsewhere {
		t.Fatalf("named %v, want the recording that is this call", got)
	}
	if got := notOwnedRecording(ctx, getTo("/changed", host), db, n); got != nil {
		t.Fatalf("a call no recording is was named as %s", got.Name)
	}
	if got := notOwnedRecording(ctx, getTo("/delete", host), &mockMemDb{mocks: []*models.Mock{elsewhere}}, n); got != nil {
		t.Fatalf("a store that stages nothing more named %s", got.Name)
	}

	r := &models.MockMismatchReport{ClosestMock: "visible", Differs: []string{"path"}, FieldDiffs: []models.MockFieldDiff{{Path: "path"}}}
	markNotOwned(r, elsewhere)
	if r.ClosestMock != elsewhere.Name || !r.NotOwned || r.Differs != nil || r.FieldDiffs != nil || r.ClosestMockReq == "" || r.NextSteps == "" {
		t.Fatalf("the report of a call made elsewhere is %+v", r)
	}
}

// Under the proven switch, the HTTP parser's miss on a call that is another
// test's recording reports it as that recording (NotOwned), which record's
// judging reads to bake it into the worker scope (S12, a2). With the switch
// off nothing is named so.
func TestAMissOnACallMadeElsewhereNamesItsRecording(t *testing.T) {
	const addr, host = "127.0.0.1:50001", "localhost:50001"
	miss := func(t *testing.T) *models.MockMismatchReport {
		t.Helper()
		db := stagedDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{onHost("other-test-delete", "/delete", host)}}
		var reported error
		ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
		app, prox := net.Pipe()
		defer app.Close()
		done := make(chan error, 1)
		go func() {
			done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, models.OutgoingOptions{})
		}()
		if _, err := io.WriteString(app, "GET /delete HTTP/1.1\r\nHost: "+host+"\r\nAccept: application/json\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			t.Fatalf("the app got no answer: %v", err)
		}
		_ = resp.Body.Close()
		_ = app.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the parser did not return after the miss")
		}
		var withReport interface {
			MismatchReport() *models.MockMismatchReport
		}
		if !errors.As(reported, &withReport) || withReport.MismatchReport() == nil {
			t.Fatalf("the miss was reported as %v, with no mismatch report", reported)
		}
		return withReport.MismatchReport()
	}
	t.Setenv(provenswitch.Env, "1")
	if r := miss(t); !r.NotOwned || r.ClosestMock != "other-test-delete" {
		t.Fatalf("under the switch the miss names %q (not owned: %v), want other-test-delete", r.ClosestMock, r.NotOwned)
	}
	t.Setenv(provenswitch.Env, "")
	if r := miss(t); r.NotOwned {
		t.Fatalf("without the switch the miss is named not owned: %+v", r)
	}
}
