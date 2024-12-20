//go:build linux || windows

package conn

import (
	"net/http"
	"regexp"
	"strconv"

	"go.keploy.io/server/v2/config"
	proxyHttp "go.keploy.io/server/v2/pkg/core/proxy/integrations/http"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func isFiltered(logger *zap.Logger, req *http.Request, opts models.IncomingOptions) bool {
	dstPort := 0
	var err error
	if p := req.URL.Port(); p != "" {
		dstPort, err = strconv.Atoi(p)
		if err != nil {
			utils.LogError(logger, err, "failed to obtain destination port from request")
			return false
		}
	}
	var bypassRules []config.BypassRule

	for _, filter := range opts.Filters {
		bypassRules = append(bypassRules, filter.BypassRule)
	}

	// Host, Path and Port matching
	headerOpts := models.OutgoingOptions{
		Rules:          bypassRules,
		MongoPassword:  "",
		SQLDelay:       0,
		FallBackOnMiss: false,
	}
	passThrough := proxyHttp.IsPassThrough(logger, req, uint(dstPort), headerOpts)

	for _, filter := range opts.Filters {
		if filter.URLMethods != nil && len(filter.URLMethods) != 0 {
			urlMethodMatch := false
			for _, method := range filter.URLMethods {
				if method == req.Method {
					urlMethodMatch = true
					break
				}
			}
			passThrough = urlMethodMatch
			if !passThrough {
				continue
			}
		}
		if filter.Headers != nil && len(filter.Headers) != 0 {
			headerMatch := false
			for filterHeaderKey, filterHeaderValue := range filter.Headers {
				regex, err := regexp.Compile(filterHeaderValue)
				if err != nil {
					utils.LogError(logger, err, "failed to compile the header regex")
					continue
				}
				if req.Header.Get(filterHeaderKey) != "" {
					for _, value := range req.Header.Values(filterHeaderKey) {
						headerMatch = regex.MatchString(value)
						if headerMatch {
							break
						}
					}
				}
				passThrough = headerMatch
				if passThrough {
					break
				}
			}
		}
	}

	return passThrough
}
