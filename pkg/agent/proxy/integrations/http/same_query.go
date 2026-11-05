package http

import (
	"net/url"

	"go.keploy.io/server/v3/pkg/models"
)

// SameRecordedQuery reports whether the strict comparison (queryDiffs) reads
// query, a call's, as rec's recorded query (its URL's, or its URLParams'):
// the same parameters, each with the same values, a repeated parameter's in
// any order; the parameters keploy adds (keploy*) not compared; a SigV4
// presigned URL's signing parameters read by their grammar
// (presignedDiffs), so its signature, date and session token may differ;
// and one value at a place learned names (url.query.<name>) free.
// keploy.yml's url noise is not read. It is the one query comparison a
// replay makes, exported so that an authoring tool reads two calls' queries
// as a replay will.
func SameRecordedQuery(rec *models.Mock, learned map[string][]string, query url.Values) bool {
	if rec == nil || rec.Spec.HTTPReq == nil {
		return false
	}
	_, recorded := recordedURL(rec.Spec.HTTPReq)
	k := strictKnown{learned: learned}
	return len(k.queryDiffs(recorded, query)) == 0
}
