package proxy

import (
	"context"
	"net/http"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// notRecordedMeta is what the line that tells that req is not recorded as a
// test case says of it (utils.GetReqMeta: its method, host, path's shape,
// path tag and query names), and it hands the recording's client the
// request, why it is not recorded, and the same path tag
// (syncMock NoteNotRecorded, models.NotRecordedPart): the client's console
// prints its path beside the tag, so the user can tell which request the
// line is about, which the line's shape alone does not say (GET /{} names
// /events and /poll alike).
func notRecordedMeta(ctx context.Context, req *http.Request, why string) map[string]string {
	meta := utils.GetReqMeta(ctx, req)
	if req != nil && req.URL != nil {
		syncMock.FromContextOrGlobal(ctx).NoteNotRecorded(models.NotRecordedCall{
			Method:    req.Method,
			Path:      req.URL.EscapedPath(),
			QueryKeys: meta["query keys"],
			PathTag:   meta["path_tag"],
			Why:       why,
		})
	}
	return meta
}
