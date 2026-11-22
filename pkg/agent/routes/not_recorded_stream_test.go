package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A request the ingress did not record as a test case (syncMock
// NoteNotRecorded) goes to the recording's client on its test-case stream
// as it comes, a part of its own (models.NotRecordedPart) closed by a
// delimiter, so the client reads it whole at once: the client's console
// names it by its path beside the path tag the agent's line names it by. One
// noted while no recording's stream is open reaches no stream.
func TestHandleIncoming_TellsTheClientOfARequestNotRecorded(t *testing.T) {
	early := models.NotRecordedCall{Method: "GET", Path: "/early", PathTag: models.PathTag(context.Background(), "GET", "/early"), Why: "before the stream"}
	syncmgr.Get().NoteNotRecorded(early)

	tcs := make(chan *models.TestCase)
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The headers are out: the stream's feed is open.
	want := []models.NotRecordedCall{
		{Method: "GET", Path: "/events", PathTag: models.PathTag(context.Background(), "GET", "/events"), Why: "its client closed its side of the connection before the app's answer ended"},
		{Method: "GET", Path: "/poll", QueryKeys: "token", PathTag: models.PathTag(context.Background(), "GET", "/poll"), Why: "its client closed its side of the connection before the app's answer ended"},
	}
	for _, c := range want {
		syncmgr.Get().NoteNotRecorded(c)
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var got []models.NotRecordedCall
	var names []string
	for len(got) < len(want) {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("the stream ended after %d notices (parts %v): %v", len(got), names, err)
		}
		names = append(names, p.FormName())
		if p.FormName() != models.NotRecordedPart {
			continue
		}
		b, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		var c models.NotRecordedCall
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("a not-recorded part of %q: %v", b, err)
		}
		got = append(got, c)
	}
	// Each closed by a delimiter, as a test case's metadata is.
	if p, err := mr.NextPart(); err != nil || p.FormName() != "delimiter" {
		t.Fatalf("the last notice is not closed by a delimiter (parts %v): %v", names, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("notice %d is %+v, want %+v", i, got[i], want[i])
		}
	}
	if strings.Join(names, ",") != "not-recorded,delimiter,not-recorded" {
		t.Errorf("the stream's parts were %v, want each notice then its delimiter, and none from before the stream", names)
	}
	close(tcs)
}
