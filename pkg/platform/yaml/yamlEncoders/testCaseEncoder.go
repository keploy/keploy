package yamlencoders

import (
	"errors"
	"strings"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func EncodeTestcase(tc models.TestCase, logger *zap.Logger) (*models.NetworkTrafficDoc, error) {

	header := pkg.ToHttpHeader(tc.HttpReq.Header)
	curl := pkg.MakeCurlCommand(string(tc.HttpReq.Method), tc.HttpReq.URL, pkg.ToYamlHttpHeader(header), tc.HttpReq.Body)
	doc := &models.NetworkTrafficDoc{
		Version: tc.Version,
		Kind:    tc.Kind,
		Name:    tc.Name,
		Curl:    curl,
	}
	// find noisy fields

	m, err := FlattenHttpResponse(pkg.ToHttpHeader(tc.HttpResp.Header), tc.HttpResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		logger.Error(msg, zap.Error(err))
	}
	noise := tc.Noise

	noiseFieldsFound := FindNoisyFields(m, func(k string, vals []string) bool {
		// check if k is date
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}

		// maybe we need to concatenate the values
		return pkg.IsTime(strings.Join(vals, ", "))
	})

	for _, v := range noiseFieldsFound {
		noise[v] = []string{}
	}

	switch tc.Kind {
	case models.HTTP:
		err := doc.Spec.Encode(models.HttpSchema{
			Request:  tc.HttpReq,
			Response: tc.HttpResp,
			Created:  tc.Created,
			Assertions: map[string]interface{}{
				"noise": noise,
			},
		})
		if err != nil {
			logger.Error("failed to encode testcase into a yaml doc", zap.Error(err))
			return nil, err
		}
	default:
		logger.Error("failed to marshal the testcase into yaml due to invalid kind of testcase")
		return nil, errors.New("type of testcases is invalid")
	}
	return doc, nil
}
