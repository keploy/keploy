package http

import (
	"net/http"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// SameRecordedBody reports whether the strict comparison (bodyDiffs) reads
// body, sent with contentType, as rec's recorded request body: by rec's
// Content-Type, a JSON body as a decoded document, a form by its parsed
// values (a repeated key's in any order), a multipart form part by part,
// anything else byte for byte. A value at a place learned names, in a
// mock's learned-noise dialect (MockSpec.ReqBodyNoise: body.amount,
// body.items[0].id, body.items[].id, or a container of them), one the
// obfuscator redacted in rec (Mock.Noise), and a field the run's request
// noise names (noise.Body: keploy.yml's test.globalNoise requestBody paths,
// as NewRequestNoise reads them, which a keploy test run hands its matcher)
// may differ; a requestBody path with patterns only while its recorded and
// its live value each match one. The zero RequestNoise names none, as
// keploy mock replay hands none. It is the one body comparison a replay
// makes, exported so that an authoring tool reads two calls' bodies as a
// replay will.
func SameRecordedBody(rec *models.Mock, learned map[string][]string, noise RequestNoise, body []byte, contentType string) bool {
	if rec == nil || rec.Spec.HTTPReq == nil {
		return false
	}
	header := http.Header{"Content-Type": {contentType}}
	k := strictKnown{n: strictNoise{body: matcher.NewNoiseLookup(noise.Body)}, learned: learned, values: util.NewNoiseChecker(rec.Noise)}
	return len(k.bodyDiffs(rec.Spec.HTTPReq, &req{body: body, header: header})) == 0
}
