package http

import (
	"net/url"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/sigv4"
)

// AWS Signature Version 4 under the proven switch (founder decision 25).
// The grammar and part-wise comparison live in pkg/agent/proxy/integrations/sigv4
// so HTTP/1 strict compare and HTTP/2 exact serving share one implementation.

const sigV4Algorithm = sigv4.Algorithm

const (
	amzAlgorithm     = sigv4.AmzAlgorithm
	amzCredential    = sigv4.AmzCredential
	amzDate          = sigv4.AmzDate
	amzSignature     = sigv4.AmzSignature
	amzSecurityToken = sigv4.AmzSecurityToken
)

type sigV4Auth = sigv4.Auth
type presignedSigV4 = sigv4.Presigned

func parseSigV4Authorization(v string) (sigV4Auth, bool) { return sigv4.ParseAuthorization(v) }

func samePayloadHash(rec string, live []string, sent []byte) bool {
	return sigv4.SamePayloadHash(rec, live, sent)
}

func parsePresignedSigV4(q url.Values) (presignedSigV4, bool) { return sigv4.ParsePresigned(q) }

func sigV4DiffsToStrict(diffs []sigv4.Diff) []strictField {
	if len(diffs) == 0 {
		return nil
	}
	out := make([]strictField, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, field(valueDiff(d.Path, d.Recorded, d.Live)))
	}
	return out
}

func sigV4AuthorizationDiffs(rec, live string) (out []strictField, ok bool) {
	diffs, ok := sigv4.AuthorizationDiffs(rec, live)
	if !ok {
		return nil, false
	}
	return sigV4DiffsToStrict(diffs), true
}

func presignedDiffs(rec, live url.Values) (byGrammar map[string]bool, out []strictField) {
	byGrammar, diffs := sigv4.PresignedDiffs(rec, live)
	return byGrammar, sigV4DiffsToStrict(diffs)
}
