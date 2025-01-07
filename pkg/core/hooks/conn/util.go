package conn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	proxyHttp "go.keploy.io/server/v2/pkg/core/proxy/integrations/http"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var (
	realTimeOffset uint64
)

// convertUnixNanoToTime takes a Unix timestamp in nanoseconds as a uint64 and returns the corresponding time.Time
func convertUnixNanoToTime(unixNano uint64) time.Time {
	// Unix time is the number of seconds since January 1, 1970 UTC,
	// so convert nanoseconds to seconds for time.Unix function
	seconds := int64(unixNano / uint64(time.Second))
	nanoRemainder := int64(unixNano % uint64(time.Second))
	return time.Unix(seconds, nanoRemainder)
}

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

//// LogAny appends input of any type to a logs.txt file in the current directory
//func LogAny(value string) error {
//
//	logMessage := value
//
//	// Add a timestamp to the log message
//	timestamp := time.Now().Format("2006-01-02 15:04:05")
//	logLine := fmt.Sprintf("%s - %s\n", timestamp, logMessage)
//
//	// Open logs.txt in append mode, create it if it doesn't exist
//	file, err := os.OpenFile("logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
//	if err != nil {
//		return err
//	}
//	defer file.Close()
//
//	// Write the log line to the file
//	_, err = file.WriteString(logLine)
//	if err != nil {
//		return err
//	}
//
//	return nil
//}

func Capture(_ context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http request body")
		return
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the http response body")
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http response body")
		return
	}

	if isFiltered(logger, req, opts) {
		logger.Debug("The request is a filtered request")
		return
	}
	var formData []models.FormData
	if contentType := req.Header.Get("Content-Type"); strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, ";")
		if len(parts) > 1 {
			req.Header.Set("Content-Type", strings.TrimSpace(parts[0]))
		}
		formData = extractFormData(logger, reqBody, contentType)
		reqBody = []byte{}
	} else if contentType := req.Header.Get("Content-Type"); contentType == "application/x-www-form-urlencoded" {
		decodedBody, err := url.QueryUnescape(string(reqBody))
		if err != nil {
			utils.LogError(logger, err, "failed to decode the url-encoded request body")
			return
		}
		reqBody = []byte(decodedBody)
	}

	t <- &models.TestCase{
		Version: models.GetVersion(),
		Name:    pkg.ToYamlHTTPHeader(req.Header)["Keploy-Test-Name"],
		Kind:    models.HTTP,
		Created: time.Now().Unix(),
		HTTPReq: models.HTTPReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			// URL:        req.URL.String(),
			// URL: fmt.Sprintf("%s://%s%s?%s", req.URL.Scheme, req.Host, req.URL.Path, req.URL.RawQuery),
			URL: fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			//  URL: string(b),
			Form:      formData,
			Header:    pkg.ToYamlHTTPHeader(req.Header),
			Body:      string(reqBody),
			URLParams: pkg.URLParams(req),
			Timestamp: reqTimeTest,
		},
		HTTPResp: models.HTTPResp{
			StatusCode:    resp.StatusCode,
			Header:        pkg.ToYamlHTTPHeader(resp.Header),
			Body:          string(respBody),
			Timestamp:     resTimeTest,
			StatusMessage: http.StatusText(resp.StatusCode),
		},
		Noise: map[string][]string{},
		// Mocks: mocks,
	}
}

func extractFormData(logger *zap.Logger, body []byte, contentType string) []models.FormData {
	boundary := ""
	if strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, "boundary=")
		if len(parts) > 1 {
			boundary = strings.TrimSpace(parts[1])
		} else {
			utils.LogError(logger, nil, "Invalid multipart/form-data content type")
			return nil
		}
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var formData []models.FormData

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			utils.LogError(logger, err, "Error reading part")
			continue
		}
		key := part.FormName()
		if key == "" {
			continue
		}

		value, err := io.ReadAll(part)
		if err != nil {
			utils.LogError(logger, err, "Error reading part value")
			continue
		}

		formData = append(formData, models.FormData{
			Key:    key,
			Values: []string{string(value)},
		})
	}

	return formData
}
