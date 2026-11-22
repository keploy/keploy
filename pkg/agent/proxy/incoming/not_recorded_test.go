package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A line that says a request is not recorded as a test case names it by its
// shape and its path tag, and the recording's stream is handed the request's
// path beside the same tag, for the client's console: the line alone names
// GET /events and GET /poll alike (GET /{}), and the console tells them
// apart by their tags. The line never names the path or a query value. The
// tag is made under the session the ingress serves (SetLogKeySession), which
// each connection it takes carries (withLogKeys), never the process's; a
// connection whose context carries a session of its own keeps that one.
func TestANotRecordedLineAndItsNoticeCarryOneTag(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	mgr := syncMock.New(zap.NewNop())
	feed, closeFeed := mgr.OpenNotRecordedFeed()
	defer closeFeed()
	pm := &IngressProxyManager{}
	keys := models.NewLogKeySession()
	pm.SetLogKeySession(keys)
	ctx := pm.withLogKeys(syncMock.NewContext(context.Background(), mgr))
	own := models.StartLogKeySession(context.Background())
	if models.LogKeys(pm.withLogKeys(own)) != models.LogKeys(own) {
		t.Fatal("a connection's own session was replaced by the ingress's")
	}

	events := httptest.NewRequest(http.MethodGet, "/events", nil)
	poll := httptest.NewRequest(http.MethodGet, "/poll?token=s3cr3t-v4lue", nil)
	tellClientLeft(ctx, logger, events)
	tellClientLeft(ctx, logger, poll)
	warnNotRecorded(ctx, logger, poll, &http.Response{StatusCode: http.StatusOK}, errors.New("the app answered early"))

	want := []models.NotRecordedCall{
		{Method: "GET", Path: "/events", PathTag: keys.PathTag("GET", "/events"), Why: clientLeftWhy},
		{Method: "GET", Path: "/poll", QueryKeys: "token", PathTag: keys.PathTag("GET", "/poll"), Why: clientLeftWhy},
		{Method: "GET", Path: "/poll", QueryKeys: "token", PathTag: keys.PathTag("GET", "/poll"), Why: answeredEarlyWhy},
	}
	if want[0].PathTag == want[1].PathTag || want[0].PathTag == models.PathTag(context.Background(), "GET", "/events") {
		t.Fatal("precondition: /events and /poll share a tag, or the session's key is the process's")
	}
	lines := logs.All()
	if len(lines) != len(want) {
		t.Fatalf("logged %d lines, want %d", len(lines), len(want))
	}
	for i, w := range want {
		select {
		case got := <-feed:
			if got != w {
				t.Errorf("notice %d is %+v, want %+v", i, got, w)
			}
		case <-time.After(time.Second): // NoteNotRecorded never waits: it is there or lost
			t.Fatalf("notice %d never reached the feed", i)
		}
		meta, _ := lines[i].ContextMap()["metadata"].(map[string]string)
		if meta["path"] != "/{}" || meta["path_tag"] != w.PathTag {
			t.Errorf("line %d names the request as %v, want its shape and tag %s", i, meta, w.PathTag)
		}
		for k, v := range meta {
			if strings.Contains(v, "events") || strings.Contains(v, "poll") || strings.Contains(v, "s3cr3t") {
				t.Errorf("line %d's %s names the request's path or query: %q", i, k, v)
			}
		}
	}
}
