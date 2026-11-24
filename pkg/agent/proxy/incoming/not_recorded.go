package proxy

import (
	"context"
	"net/http"

	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
)

// notRecordedMeta is what the line that tells that req is not recorded as a
// test case says of it, and it tells the recording's client the request with
// why (hooksUtils.TellNotRecorded: the one builder of what the console is
// told, which the requests over a size limit are told by too).
func notRecordedMeta(ctx context.Context, req *http.Request, why string) map[string]string {
	return hooksUtils.TellNotRecorded(ctx, req, why)
}
